// One private Vault process per live OpenCode session.
//
// The pool is keyed only by the session ID that OpenCode passes to a tool
// executor. Model-controlled input never selects a session or a process, and
// nothing from tool input reaches the child's argv or environment. Each
// process belongs to exactly one session: parents, children (subagents) and
// siblings each get their own. A replacement process never inherits anything
// from the one it replaces, and no call is ever replayed.

import type { ChildProcessWithoutNullStreams } from "node:child_process"
import { McpError, StdioMcpClient, UncertainOutcomeError, type JsonObject } from "./stdio-mcp.ts"

export interface SessionFacts {
  /** OpenCode's parent session ID, when the session is a child (subagent). */
  readonly parentID?: string
}

export interface PoolOptions {
  /** Starts `control mcp --vault-process`. Must not take caller input. */
  readonly spawn: () => ChildProcessWithoutNullStreams
  /**
   * Throws when the host must not use Vault (for example, it holds ambient
   * Control credentials). Runs before every call and before every spawn.
   */
  readonly preflight?: () => void
  /** Confirms the session exists in OpenCode before a process is started. */
  readonly verifySession: (sessionID: string, signal: AbortSignal | undefined) => Promise<SessionFacts>
  readonly maxSessions: number
  /** Close processes idle this long. 0 disables idle cleanup. */
  readonly idleMs: number
  readonly startupTimeoutMs: number
  readonly clientInfo: { name: string; version: string }
  readonly now?: () => number
  readonly closeGraceMs?: number
}

export interface CallContext {
  readonly signal?: AbortSignal
  readonly progress?: (update: Record<string, unknown>) => void
}

export interface CallOutcome {
  readonly result: JsonObject
  /** Set when this call started a replacement process for the session. */
  readonly notice?: string
}

interface Entry {
  readonly sessionID: string
  parentID?: string
  client?: StdioMcpClient
  ready: Promise<void>
  instructions?: string
  tools?: Map<string, JsonObject>
  toolsLoading?: Promise<Map<string, JsonObject>>
  inFlight: number
  lastUsed: number
  closed: boolean
  readonly listeners: Set<(update: Record<string, unknown>) => void>
}

// OpenCode session IDs are opaque host strings such as `ses_...`. Accept a
// conservative alphabet so a malformed host value fails closed.
const SESSION_ID = /^[A-Za-z0-9_.:-]{1,256}$/
const TOOL_NAME = /^[A-Za-z0-9_.-]{1,128}$/
const MAX_ARGUMENT_BYTES = 1024 * 1024
const MAX_INSTRUCTIONS = 16 * 1024
const MAX_REMEMBERED = 1024

export class VaultSessionPool {
  private readonly entries = new Map<string, Entry>()
  /** Why a session's last process ended, shown when a replacement starts. */
  private readonly ended = new Map<string, string>()
  /** Sessions that OpenCode reported deleted. They never get a process again. */
  private readonly deleted = new Set<string>()
  private suspended: string | undefined = "Vault session tracking has not started"
  private stopped = false
  private readonly now: () => number

  constructor(private readonly options: PoolOptions) {
    this.now = options.now ?? Date.now
  }

  /** Number of live (starting or running) processes. */
  get size(): number {
    return this.entries.size
  }

  /**
   * Refuses new calls and closes every process. Used while OpenCode's event
   * stream is unavailable, because a deletion could otherwise be missed.
   */
  async suspend(reason: string): Promise<void> {
    this.suspended = reason
    await this.closeAll(reason)
  }

  resume(): void {
    if (!this.stopped) this.suspended = undefined
  }

  /** Permanently stops the pool (plugin unload). */
  async stop(reason: string): Promise<void> {
    this.stopped = true
    this.suspended = reason
    await this.closeAll(reason)
  }

  async closeAll(reason: string): Promise<void> {
    await Promise.all([...this.entries.keys()].map((id) => this.closeSession(id, reason, { descendants: false })))
  }

  /** Session deleted in OpenCode: close it and every known descendant, and refuse them afterwards. */
  async deleteSession(sessionID: string): Promise<void> {
    this.remember(this.deleted, sessionID)
    await this.closeSession(sessionID, "the OpenCode session was deleted", { descendants: true })
  }

  async closeSession(sessionID: string, reason: string, options: { descendants: boolean }): Promise<void> {
    const closing: Promise<void>[] = []
    const entry = this.entries.get(sessionID)
    if (entry) closing.push(this.closeEntry(entry, reason))
    if (options.descendants) {
      for (const child of [...this.entries.values()]) {
        if (child.parentID === sessionID) {
          if (this.deleted.has(sessionID)) this.remember(this.deleted, child.sessionID)
          closing.push(this.closeSession(child.sessionID, reason, options))
        }
      }
    }
    await Promise.all(closing)
  }

  /** Closes processes whose sessions have been idle longer than `idleMs`. */
  async sweepIdle(): Promise<void> {
    if (this.options.idleMs <= 0) return
    const cutoff = this.now() - this.options.idleMs
    const idle = [...this.entries.values()].filter((entry) => entry.inFlight === 0 && entry.lastUsed <= cutoff)
    await Promise.all(idle.map((entry) => this.closeEntry(entry, "it was idle")))
  }

  /**
   * Routes one MCP tool call through the calling session's own process.
   * `sessionID` must come from the host tool context, never from tool input.
   */
  async call(sessionID: string, tool: string, args: JsonObject, context: CallContext = {}): Promise<CallOutcome> {
    if (typeof tool !== "string" || !TOOL_NAME.test(tool)) throw new Error("tool must be an MCP tool name")
    if (JSON.stringify(args).length > MAX_ARGUMENT_BYTES) throw new Error("tool arguments are too large")
    return this.withEntry(sessionID, context, async (entry, client) => {
      await this.requireTool(entry, tool, context.signal)
      return client.callTool(tool, args, {
        signal: context.signal,
        onProgress: context.progress ? (params) => context.progress?.({ status: progressText(params) }) : undefined,
      })
    })
  }

  /**
   * Describes the calling session's tools without invoking any of them. With
   * a name, returns that tool's description and input schema; otherwise the
   * server instructions and every tool name with a short description.
   */
  async describe(sessionID: string, tool: string | undefined, context: CallContext = {}): Promise<CallOutcome> {
    if (tool !== undefined && (typeof tool !== "string" || !TOOL_NAME.test(tool))) {
      throw new Error("tool must be an MCP tool name")
    }
    return this.withEntry(sessionID, context, async (entry) => {
      if (tool !== undefined) return { tool: await this.requireTool(entry, tool, context.signal) }
      const tools = await this.toolsFor(entry, context.signal)
      return {
        instructions: entry.instructions,
        tools: [...tools.values()].map((t) => ({ name: t.name, description: summary(t.description) })),
      }
    })
  }

  private async withEntry(
    sessionID: string,
    context: CallContext,
    action: (entry: Entry, client: StdioMcpClient) => Promise<JsonObject>,
  ): Promise<CallOutcome> {
    if (typeof sessionID !== "string" || !SESSION_ID.test(sessionID)) {
      throw new Error("OpenCode did not supply a usable session identity; refusing to route the call")
    }
    if (this.suspended) throw new Error(`Vault routing is unavailable: ${this.suspended}`)
    if (this.deleted.has(sessionID)) throw new Error("this OpenCode session was deleted")
    this.options.preflight?.()

    let entry = this.entries.get(sessionID)
    let notice: string | undefined
    if (!entry) {
      if (this.entries.size >= this.options.maxSessions) {
        throw new Error(
          `${this.options.maxSessions} OpenCode sessions already hold a Vault process; ` +
            "finish or delete another session, or wait for one to go idle",
        )
      }
      const previous = this.ended.get(sessionID)
      if (previous) {
        notice =
          `This session's previous Vault process ended because ${previous}. ` +
          "A new Vault process was started; earlier approvals and grants did not carry over."
      }
      entry = this.start(sessionID, context)
    }
    entry.inFlight++
    entry.lastUsed = this.now()
    const listener = context.progress
    if (listener) entry.listeners.add(listener)
    try {
      await entry.ready
      if (entry.closed || !entry.client) throw new Error(this.ended.get(sessionID) ?? "the Vault process is closed")
      const result = await action(entry, entry.client)
      return { result, notice }
    } catch (error) {
      if (entry.client?.endedReason && !entry.closed) {
        // The child died under us: forget it so the next call starts fresh,
        // and say so instead of silently reconnecting.
        await this.closeEntry(entry, entry.client.endedReason)
      }
      throw this.explain(error, entry)
    } finally {
      entry.inFlight--
      entry.lastUsed = this.now()
      if (listener) entry.listeners.delete(listener)
    }
  }

  private start(sessionID: string, context: CallContext): Entry {
    const entry: Entry = {
      sessionID,
      ready: Promise.resolve(),
      inFlight: 0,
      lastUsed: this.now(),
      closed: false,
      listeners: new Set(),
    }
    this.entries.set(sessionID, entry)
    entry.ready = this.launch(entry, context.signal)
    // Errors are reported through `withEntry`; avoid unhandled rejections.
    entry.ready.catch(() => undefined)
    return entry
  }

  private async launch(entry: Entry, signal: AbortSignal | undefined): Promise<void> {
    try {
      const facts = await this.options.verifySession(entry.sessionID, signal)
      entry.parentID = facts.parentID
      if (entry.closed) throw new Error("the session was closed while its Vault process was starting")
      if (facts.parentID && this.deleted.has(facts.parentID)) throw new Error("the parent session was deleted")
      // The host environment may have changed while the session was verified.
      this.options.preflight?.()
      for (const listener of entry.listeners) listener({ status: "Starting this session's Vault process" })
      const client = new StdioMcpClient(this.options.spawn(), {
        onStderrLine: (line) => {
          for (const listener of entry.listeners) listener({ status: `Vault: ${line}` })
        },
        onExit: (reason) => {
          if (!entry.closed) void this.closeEntry(entry, reason)
        },
        onToolsChanged: () => {
          entry.tools = undefined
        },
      })
      entry.client = client
      if (entry.closed) {
        await client.close(this.options.closeGraceMs)
        throw new Error("the session was closed while its Vault process was starting")
      }
      // The frontend answers initialize and tools/list itself; it contacts
      // the broker lazily, when a fleet call first needs it. The timeout
      // covers only process start and the MCP handshake, and the call that
      // started the process can cancel it.
      const init = await client.initialize(this.options.clientInfo, { signal, timeoutMs: this.options.startupTimeoutMs })
      if (typeof init.instructions === "string") entry.instructions = init.instructions.slice(0, MAX_INSTRUCTIONS)
    } catch (error) {
      await this.closeEntry(entry, `it failed to start (${message(error)})`)
      throw error
    }
  }

  private async requireTool(entry: Entry, tool: string, signal: AbortSignal | undefined): Promise<JsonObject> {
    const tools = await this.toolsFor(entry, signal)
    const found = tools.get(tool)
    if (found) return found
    const names = [...tools.keys()].sort().slice(0, 128).join(", ")
    throw new Error(`this session's Vault process does not offer ${tool}. Available: ${names || "none"}`)
  }

  private async toolsFor(entry: Entry, signal: AbortSignal | undefined): Promise<Map<string, JsonObject>> {
    if (entry.tools) return entry.tools
    const client = entry.client
    if (!client) throw new Error("the Vault process is closed")
    entry.toolsLoading ??= client
      .listTools({ signal, timeoutMs: 60_000 })
      .then((tools) => {
        const byName = new Map<string, JsonObject>()
        for (const tool of tools) if (typeof tool.name === "string") byName.set(tool.name, tool)
        entry.tools = byName
        return byName
      })
      .finally(() => {
        entry.toolsLoading = undefined
      })
    return entry.toolsLoading
  }

  private async closeEntry(entry: Entry, reason: string): Promise<void> {
    if (!entry.closed) {
      entry.closed = true
      if (this.entries.get(entry.sessionID) === entry) this.entries.delete(entry.sessionID)
      this.ended.delete(entry.sessionID)
      if (this.ended.size >= MAX_REMEMBERED) this.ended.delete(this.ended.keys().next().value as string)
      this.ended.set(entry.sessionID, reason)
    }
    await entry.client?.close(this.options.closeGraceMs)
  }

  private remember(set: Set<string>, value: string): void {
    set.delete(value)
    if (set.size >= MAX_REMEMBERED) set.delete(set.values().next().value as string)
    set.add(value)
  }

  private explain(error: unknown, entry: Entry): Error {
    if (error instanceof McpError) return error
    const tail = entry.client?.stderrTail
    if (error instanceof UncertainOutcomeError && tail) {
      return new UncertainOutcomeError(`${error.message}\nRecent Vault stderr:\n${tail}`)
    }
    return error instanceof Error ? error : new Error(String(error))
  }
}

function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}

function summary(description: unknown): string | undefined {
  if (typeof description !== "string") return undefined
  const first = description.split(/(?<=\.)\s/)[0] ?? description
  return first.length > 160 ? `${first.slice(0, 157)}...` : first
}

function progressText(params: JsonObject): string {
  if (typeof params.message === "string") return params.message
  if (typeof params.progress === "number") {
    return typeof params.total === "number" ? `${params.progress}/${params.total}` : `${params.progress}`
  }
  return "working"
}
