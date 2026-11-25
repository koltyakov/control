// A deliberately small MCP client for one private stdio child.
//
// It implements only what the adapter needs: initialize, tools/list,
// tools/call, cancellation notifications, progress notifications and ping.
// Framing is newline-delimited JSON-RPC 2.0, as used by MCP stdio transports.
// Every buffer, line and pending table is bounded. Nothing here retries a
// request: a lost child or cancelled call is reported as uncertain.

import type { ChildProcessWithoutNullStreams } from "node:child_process"

export const PROTOCOL_VERSION = "2025-06-18"

export type JsonObject = Record<string, unknown>

/** Raised when a request may or may not have taken effect in the child. */
export class UncertainOutcomeError extends Error {
  override name = "UncertainOutcomeError"
}

/** A JSON-RPC error returned by the child. */
export class McpError extends Error {
  override name = "McpError"
  constructor(
    readonly code: number,
    message: string,
  ) {
    super(message)
  }
}

export interface StdioMcpOptions {
  /** Largest accepted stdout line. Exceeding it terminates the child. */
  readonly maxLineBytes?: number
  /** Largest number of concurrent requests to this child. */
  readonly maxPending?: number
  /** Receives complete stderr lines, truncated to 1 KiB each. */
  readonly onStderrLine?: (line: string) => void
  /** Called once when the child exits or fails to start. */
  readonly onExit?: (reason: string) => void
  /** Called when the child announces that its tool list changed. */
  readonly onToolsChanged?: () => void
}

export interface RequestOptions {
  readonly signal?: AbortSignal
  readonly timeoutMs?: number
  readonly onProgress?: (params: JsonObject) => void
}

interface Pending {
  readonly method: string
  readonly progressToken?: string
  readonly onProgress?: (params: JsonObject) => void
  resolve(value: unknown): void
  reject(error: Error): void
}

const DEFAULT_MAX_LINE = 16 * 1024 * 1024
const DEFAULT_MAX_PENDING = 32
const MAX_STDERR_LINE = 1024
const STDERR_TAIL_LINES = 20

function isObject(value: unknown): value is JsonObject {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}

export class StdioMcpClient {
  readonly pid: number | undefined
  private readonly proc: ChildProcessWithoutNullStreams
  private readonly maxLineBytes: number
  private readonly maxPending: number
  private readonly options: StdioMcpOptions
  private readonly pending = new Map<number, Pending>()
  private readonly progress = new Map<string, number>()
  private chunks: Buffer[] = []
  private chunkBytes = 0
  private stderrPartial = ""
  private readonly stderrLines: string[] = []
  private nextID = 1
  private ended: string | undefined
  private readonly exitPromise: Promise<void>
  private resolveExit!: () => void

  constructor(proc: ChildProcessWithoutNullStreams, options: StdioMcpOptions = {}) {
    this.proc = proc
    this.pid = proc.pid
    this.options = options
    this.maxLineBytes = options.maxLineBytes ?? DEFAULT_MAX_LINE
    this.maxPending = options.maxPending ?? DEFAULT_MAX_PENDING
    this.exitPromise = new Promise((resolve) => (this.resolveExit = resolve))
    proc.stdout.on("data", (chunk: Buffer) => this.onStdout(chunk))
    proc.stderr.on("data", (chunk: Buffer) => this.onStderr(chunk))
    // A write to an exited child surfaces here rather than as a throw.
    proc.stdin.on("error", () => this.fail("Vault process stdin closed"))
    proc.on("error", (error) => this.finish(`Vault process failed: ${error.message}`))
    // Prefer `close`, which follows the last stdout/stderr bytes, so errors
    // include the child's final diagnostics. If an inherited pipe keeps the
    // streams open after exit, finish shortly after `exit` anyway.
    const exited = (code: number | null, signal: NodeJS.Signals | null) =>
      `Vault process exited (${signal ? `signal ${signal}` : `code ${code}`})`
    proc.on("exit", (code, signal) => {
      this.exitedFlag = true
      setTimeout(() => this.finish(exited(code, signal)), 1000).unref?.()
    })
    proc.on("close", (code: number | null, signal: NodeJS.Signals | null) => this.finish(exited(code, signal)))
  }

  /** Set once the child has exited, failed to start, or broke the protocol. */
  get endedReason(): string | undefined {
    return this.ended
  }

  /** The last few stderr lines, for diagnostics in errors. */
  get stderrTail(): string {
    return this.stderrLines.join("\n")
  }

  async initialize(clientInfo: { name: string; version: string }, options: RequestOptions): Promise<JsonObject> {
    const result = await this.request(
      "initialize",
      { protocolVersion: PROTOCOL_VERSION, capabilities: {}, clientInfo },
      options,
    )
    if (!isObject(result)) throw new McpError(-32603, "invalid initialize result")
    this.notify("notifications/initialized", {})
    return result
  }

  /** Lists every advertised tool, following at most `maxPages` cursors. */
  async listTools(options: RequestOptions, maxPages = 16): Promise<JsonObject[]> {
    const tools: JsonObject[] = []
    let cursor: string | undefined
    for (let page = 0; page < maxPages; page++) {
      const result = await this.request("tools/list", cursor ? { cursor } : {}, options)
      if (!isObject(result) || !Array.isArray(result.tools)) throw new McpError(-32603, "invalid tools/list result")
      for (const tool of result.tools) if (isObject(tool)) tools.push(tool)
      if (typeof result.nextCursor !== "string" || result.nextCursor === "") return tools
      cursor = result.nextCursor
    }
    throw new McpError(-32603, "tools/list exceeded the page limit")
  }

  async callTool(name: string, args: JsonObject, options: RequestOptions): Promise<JsonObject> {
    const result = await this.request("tools/call", { name, arguments: args }, options)
    if (!isObject(result)) throw new McpError(-32603, "invalid tools/call result")
    return result
  }

  /**
   * Sends one request. It is never retried. Aborting sends
   * notifications/cancelled and rejects with UncertainOutcomeError, because
   * the child may already have acted.
   */
  request(method: string, params: JsonObject, options: RequestOptions = {}): Promise<unknown> {
    if (this.ended) return Promise.reject(new Error(this.ended))
    if (options.signal?.aborted) return Promise.reject(new Error(`${method} was cancelled before it was sent`))
    if (this.pending.size >= this.maxPending) {
      return Promise.reject(new Error("too many concurrent calls to this session's Vault process"))
    }
    const id = this.nextID++
    let progressToken: string | undefined
    if (options.onProgress) {
      progressToken = `opencode-${id}`
      params = { ...params, _meta: { progressToken } }
    }
    return new Promise((resolve, reject) => {
      let timer: ReturnType<typeof setTimeout> | undefined
      const cleanup = () => {
        if (timer) clearTimeout(timer)
        options.signal?.removeEventListener("abort", onAbort)
        this.pending.delete(id)
        if (progressToken) this.progress.delete(progressToken)
      }
      const abandon = (why: string) => {
        if (!this.pending.has(id)) return
        cleanup()
        this.notify("notifications/cancelled", { requestId: id, reason: why })
        reject(
          new UncertainOutcomeError(
            `${method} ${why}; the Vault process may still complete it. It was not replayed.`,
          ),
        )
      }
      const onAbort = () => abandon("was cancelled by OpenCode")
      this.pending.set(id, {
        method,
        progressToken,
        onProgress: options.onProgress,
        resolve: (value) => {
          cleanup()
          resolve(value)
        },
        reject: (error) => {
          cleanup()
          reject(error)
        },
      })
      if (progressToken) this.progress.set(progressToken, id)
      options.signal?.addEventListener("abort", onAbort, { once: true })
      if (options.timeoutMs !== undefined) {
        timer = setTimeout(() => abandon(`timed out after ${options.timeoutMs} ms`), options.timeoutMs)
      }
      this.write({ jsonrpc: "2.0", id, method, params })
    })
  }

  notify(method: string, params: JsonObject): void {
    if (!this.ended) this.write({ jsonrpc: "2.0", method, params })
  }

  /**
   * Closes stdin, then escalates to SIGTERM and SIGKILL. Resolves once the
   * child has exited. In-flight requests are rejected as uncertain.
   */
  async close(graceMs = 2000): Promise<void> {
    this.ended ??= "the Vault process was closed"
    this.rejectAll(new UncertainOutcomeError("the Vault process was closed while a call was in flight; it was not replayed"))
    if (!this.hasExited()) {
      try {
        this.proc.stdin.end()
      } catch {
        // Already closed.
      }
      if (!(await this.waitExit(graceMs))) {
        this.proc.kill("SIGTERM")
        if (!(await this.waitExit(graceMs))) {
          this.proc.kill("SIGKILL")
          await this.waitExit(graceMs * 2)
        }
      }
    }
  }

  private hasExited(): boolean {
    return this.proc.exitCode !== null || this.proc.signalCode !== null || this.exitedFlag
  }

  private exitedFlag = false
  private finished = false

  private async waitExit(ms: number): Promise<boolean> {
    if (this.hasExited()) return true
    let timer: ReturnType<typeof setTimeout> | undefined
    const timeout = new Promise<boolean>((resolve) => (timer = setTimeout(() => resolve(false), ms)))
    const exited = this.exitPromise.then(() => true)
    const result = await Promise.race([exited, timeout])
    if (timer) clearTimeout(timer)
    return result
  }

  private write(message: JsonObject): void {
    try {
      this.proc.stdin.write(`${JSON.stringify(message)}\n`)
    } catch {
      this.fail("could not write to the Vault process")
    }
  }

  private onStdout(chunk: Buffer): void {
    let start = 0
    while (!this.ended) {
      const newline = chunk.indexOf(10, start)
      const piece = chunk.subarray(start, newline < 0 ? chunk.length : newline)
      if (this.chunkBytes + piece.length > this.maxLineBytes) {
        this.fail("Vault process sent an oversized message")
        return
      }
      if (newline < 0) {
        if (piece.length > 0) {
          this.chunks.push(piece)
          this.chunkBytes += piece.length
        }
        return
      }
      this.chunks.push(piece)
      const line = Buffer.concat(this.chunks).toString("utf8").trim()
      this.chunks = []
      this.chunkBytes = 0
      start = newline + 1
      if (line !== "") this.onLine(line)
    }
  }

  private onLine(line: string): void {
    let message: unknown
    try {
      message = JSON.parse(line)
    } catch {
      this.fail("Vault process wrote invalid JSON to stdout")
      return
    }
    if (!isObject(message)) {
      this.fail("Vault process wrote a non-object message")
      return
    }
    const hasID = typeof message.id === "number" || typeof message.id === "string"
    if (typeof message.method === "string") {
      if (hasID) this.onServerRequest(message.id as number | string, message.method)
      else this.onNotification(message.method, isObject(message.params) ? message.params : {})
      return
    }
    if (typeof message.id !== "number") return
    const pending = this.pending.get(message.id)
    if (!pending) return
    if (isObject(message.error)) {
      const code = typeof message.error.code === "number" ? message.error.code : -32603
      const text = typeof message.error.message === "string" ? message.error.message : "unknown error"
      pending.reject(new McpError(code, text))
      return
    }
    pending.resolve(message.result)
  }

  private onServerRequest(id: number | string, method: string): void {
    // The adapter offers no client capabilities: no sampling, roots or
    // elicitation. Only liveness checks are answered.
    if (method === "ping") this.write({ jsonrpc: "2.0", id, result: {} })
    else this.write({ jsonrpc: "2.0", id, error: { code: -32601, message: `${method} is not supported by this client` } })
  }

  private onNotification(method: string, params: JsonObject): void {
    if (method === "notifications/progress") {
      const token = params.progressToken
      const id = typeof token === "string" ? this.progress.get(token) : undefined
      const pending = id === undefined ? undefined : this.pending.get(id)
      if (pending?.onProgress) {
        try {
          pending.onProgress(params)
        } catch {
          // Progress display must never break the call.
        }
      }
      return
    }
    if (method === "notifications/tools/list_changed") this.options.onToolsChanged?.()
  }

  private onStderr(chunk: Buffer): void {
    const text = this.stderrPartial + chunk.toString("utf8")
    const lines = text.split(/\r?\n/)
    this.stderrPartial = (lines.pop() ?? "").slice(0, MAX_STDERR_LINE)
    for (const raw of lines) {
      const line = raw.slice(0, MAX_STDERR_LINE)
      if (line === "") continue
      this.stderrLines.push(line)
      if (this.stderrLines.length > STDERR_TAIL_LINES) this.stderrLines.shift()
      this.options.onStderrLine?.(line)
    }
  }

  /** Protocol failure: terminate the child and fail every request. */
  private fail(reason: string): void {
    if (this.ended) return
    this.ended = reason
    this.rejectAll(new UncertainOutcomeError(`${reason}; any in-flight call has an uncertain outcome and was not replayed`))
    try {
      this.proc.kill("SIGKILL")
    } catch {
      // Already gone.
    }
    this.options.onExit?.(reason)
  }

  private finish(reason: string): void {
    if (this.finished) return
    this.finished = true
    this.exitedFlag = true
    if (!this.ended) {
      this.ended = reason
      this.rejectAll(new UncertainOutcomeError(`${reason}; any in-flight call has an uncertain outcome and was not replayed`))
      this.options.onExit?.(reason)
    }
    this.resolveExit()
  }

  private rejectAll(error: Error): void {
    for (const pending of [...this.pending.values()]) pending.reject(error)
  }
}
