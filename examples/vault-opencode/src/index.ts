// OpenCode V2 server plugin: route Control MCP tool calls through a private
// `control mcp --vault-process` stdio child owned by the calling session.
//
// Only `import type` is used from @opencode/plugin, so the plugin has no
// runtime dependencies and loads from a local directory without installation.

import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process"
import type { Plugin } from "@opencode/plugin"
import { VaultSessionPool, type SessionFacts } from "./pool.ts"
import type { JsonObject } from "./stdio-mcp.ts"

export const PLUGIN_ID = "control.vault"
export const TOOL_NAME = "control_vault_call"
const VERSION = "0.1.0"

type HostSessionID = Parameters<Plugin.Context["session"]["get"]>[0]["sessionID"]

/** The fixed frontend command. Tool input never changes it. */
export const VAULT_ARGS = Object.freeze(["mcp", "--vault-process"])

/**
 * Session events after which a session's process must not continue:
 * deletion, archival, a move to another location, or a committed revert of
 * the conversation that may have led to approvals.
 */
const CLOSING_EVENTS = ["session.deleted", "session.archived", "session.moved", "session.revert.committed"] as const

/**
 * Credential variables that Control's Vault frontend treats as ambient
 * account, superuser or node/local API credentials (vaultstore.CheckAmbient).
 * A nonempty one in the OpenCode host means the host itself already holds
 * fleet authority, so the adapter refuses to start a Vault process rather
 * than hide it from the child. Empty ones are still removed from the child.
 */
export const AMBIENT_CREDENTIAL_ENV = Object.freeze(["CONTROL_TOKEN", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY"])

/** Names (never values) of nonempty Control credential variables in `source`. */
export function ambientCredentials(source: NodeJS.ProcessEnv = process.env): string[] {
  const found = new Set<string>()
  for (const [key, value] of Object.entries(source)) {
    // Windows environment names are case-insensitive.
    const name = key.toUpperCase()
    if (AMBIENT_CREDENTIAL_ENV.includes(name) && value !== undefined && value !== "") found.add(name)
  }
  return [...found].sort()
}

/** Throws before any process starts if the host environment holds Control credentials. */
export function assertNoAmbientCredentials(source: NodeJS.ProcessEnv = process.env): void {
  const names = ambientCredentials(source)
  if (names.length === 0) return
  throw new Error(
    `the OpenCode host environment contains Control credentials (${names.join(", ")}). ` +
      "Vault cannot protect access from a host that already holds them, so no Vault process was started. " +
      "Remove them from the environment OpenCode runs in, then restart OpenCode.",
  )
}

export interface AdapterOptions {
  readonly controlPath: string
  readonly maxSessions: number
  readonly idleMs: number
  readonly startupTimeoutMs: number
}

export function parseOptions(raw: Readonly<Record<string, unknown>>): AdapterOptions {
  const known = new Set(["controlPath", "maxSessions", "idleMinutes", "startupTimeoutSeconds"])
  for (const key of Object.keys(raw)) if (!known.has(key)) throw new Error(`${PLUGIN_ID}: unknown option ${key}`)
  const controlPath = raw.controlPath ?? "control"
  if (typeof controlPath !== "string" || controlPath === "" || controlPath.includes("\0")) {
    throw new Error(`${PLUGIN_ID}: controlPath must be a non-empty path`)
  }
  const int = (name: string, fallback: number, min: number, max: number): number => {
    const value = raw[name] ?? fallback
    if (typeof value !== "number" || !Number.isInteger(value) || value < min || value > max) {
      throw new Error(`${PLUGIN_ID}: ${name} must be an integer from ${min} to ${max}`)
    }
    return value
  }
  return {
    controlPath,
    maxSessions: int("maxSessions", 8, 1, 64),
    // 0 (the default) disables idle cleanup: an approved session keeps its
    // connection, grant and foreground forwards for the grant's own lifetime.
    idleMs: int("idleMinutes", 0, 0, 24 * 60) * 60_000,
    startupTimeoutMs: int("startupTimeoutSeconds", 60, 5, 600) * 1000,
  }
}

export function childEnv(source: NodeJS.ProcessEnv = process.env): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {}
  for (const [key, value] of Object.entries(source)) {
    if (value !== undefined && !AMBIENT_CREDENTIAL_ENV.includes(key.toUpperCase())) env[key] = value
  }
  return env
}

function spawnVault(controlPath: string): ChildProcessWithoutNullStreams {
  return spawn(controlPath, [...VAULT_ARGS], {
    stdio: ["pipe", "pipe", "pipe"],
    env: childEnv(),
    windowsHide: true,
    shell: false,
  })
}

function render(result: JsonObject): string {
  const parts: string[] = []
  if (Array.isArray(result.content)) {
    for (const item of result.content) {
      if (typeof item !== "object" || item === null) continue
      const content = item as JsonObject
      if (content.type === "text" && typeof content.text === "string") parts.push(content.text)
      else parts.push(JSON.stringify(content))
    }
  }
  if (parts.length === 0 && result.structuredContent !== undefined) parts.push(JSON.stringify(result.structuredContent))
  return parts.join("\n")
}

const DESCRIPTION = [
  "Call one Control MCP tool through this OpenCode session's private Vault process",
  "(`control mcp --vault-process`). Each session, including each subagent session, has its own",
  "process, so a Vault approval covers only this session; a replacement process starts unapproved.",
  "Use `describe: true` without `tool` to list the tools and server instructions, or with `tool` to",
  "read one tool's input schema; describing never invokes a tool. Otherwise set `tool` to a",
  "Control MCP tool name such as control_nodes and `arguments` to its input.",
  "If a call returns approval_required, nothing was sent: show the user the request code from this",
  "chat, tell them to compare it with the code in their `control vault grant CODE` terminal prompt",
  "(the caller label there is not verified), and call again only after they confirm approval.",
  "Other failed, cancelled or interrupted calls are never retried automatically and may have taken",
  "effect; inspect state before deciding whether to issue new work.",
].join(" ")

const INPUT_SCHEMA = {
  type: "object",
  properties: {
    tool: {
      type: "string",
      pattern: "^[A-Za-z0-9_.-]{1,128}$",
      description: "Control MCP tool name, for example control_nodes",
    },
    arguments: { type: "object", description: "Input for that tool", additionalProperties: true },
    describe: { type: "boolean", description: "Describe instead of calling" },
  },
  additionalProperties: false,
} as const

export interface PluginDeps {
  /** Replaces process creation in tests. The command stays fixed in production. */
  readonly spawn?: () => ChildProcessWithoutNullStreams
  /** Host environment to check for ambient credentials. Defaults to `process.env`. */
  readonly env?: NodeJS.ProcessEnv
}

export function createPlugin(deps: PluginDeps = {}): Plugin.Plugin {
  return {
    id: PLUGIN_ID,
    async setup(ctx) {
      const options = parseOptions(ctx.options)
      const pool = new VaultSessionPool({
        spawn: deps.spawn ?? (() => spawnVault(options.controlPath)),
        // Checked against the live host environment on every call and again
        // immediately before each spawn.
        preflight: () => assertNoAmbientCredentials(deps.env ?? process.env),
        verifySession: async (sessionID, signal): Promise<SessionFacts> => {
          // The pool only holds IDs that came from the host tool context, so
          // restoring the host's branded type here is not a new claim.
          const info = await ctx.session.get({ sessionID: sessionID as HostSessionID }, { signal })
          if (!info || info.id !== sessionID) throw new Error("OpenCode could not confirm this session")
          return { parentID: typeof info.parentID === "string" ? info.parentID : undefined }
        },
        maxSessions: options.maxSessions,
        idleMs: options.idleMs,
        startupTimeoutMs: options.startupTimeoutMs,
        clientInfo: { name: "opencode-control-vault", version: VERSION },
      })

      const watcher = new AbortController()
      const watching = watch(ctx, pool, watcher.signal)
      // Idle cleanup is opt-in; by default only lifecycle events, the
      // session cap and unload end a session's process.
      const sweeper =
        options.idleMs > 0
          ? setInterval(() => void pool.sweepIdle(), Math.min(60_000, Math.max(1000, options.idleMs / 4)))
          : undefined
      sweeper?.unref?.()

      await ctx.tool.transform((editor) => {
        editor.add({
          name: TOOL_NAME,
          description: DESCRIPTION,
          input: INPUT_SCHEMA,
          execute: async (input, context) => {
            const request = readInput(input)
            const callContext = {
              signal: context.signal,
              progress: (update: Record<string, unknown>) => void context.progress(update).catch(() => undefined),
            }
            // The session comes from the host tool context only.
            if (request.describe) {
              const outcome = await pool.describe(context.sessionID, request.tool, callContext)
              const text = JSON.stringify(outcome.result, null, 2)
              return { content: outcome.notice ? `${outcome.notice}\n\n${text}` : text }
            }
            const outcome = await pool.call(context.sessionID, request.tool, request.args, callContext)
            const text = render(outcome.result)
            const content = outcome.notice ? `${outcome.notice}\n\n${text}` : text
            if (outcome.result.isError === true) throw new Error(content || `${request.tool} failed`)
            return { content }
          },
        })
      })

      return async () => {
        if (sweeper) clearInterval(sweeper)
        watcher.abort()
        await pool.stop("the OpenCode plugin was unloaded")
        await watching
      }
    },
  }
}

type Request = { describe: true; tool?: string } | { describe: false; tool: string; args: JsonObject }

function readInput(input: unknown): Request {
  if (typeof input !== "object" || input === null || Array.isArray(input)) throw new Error("input must be an object")
  const record = input as Record<string, unknown>
  for (const key of Object.keys(record)) {
    if (key !== "tool" && key !== "arguments" && key !== "describe") {
      throw new Error(`unexpected input field ${key}; the session is chosen by OpenCode, not by tool input`)
    }
  }
  if (record.tool !== undefined && typeof record.tool !== "string") throw new Error("tool must be a string")
  if (record.describe !== undefined && typeof record.describe !== "boolean") throw new Error("describe must be a boolean")
  if (record.describe === true) {
    if (record.arguments !== undefined) throw new Error("describe does not take arguments")
    return { describe: true, tool: record.tool }
  }
  if (record.tool === undefined) throw new Error("tool is required unless describe is true")
  const args = record.arguments ?? {}
  if (typeof args !== "object" || args === null || Array.isArray(args)) throw new Error("arguments must be an object")
  return { describe: false, tool: record.tool, args: args as JsonObject }
}

/**
 * Follows session lifecycle events. While the stream is down, the pool is
 * suspended and every process closed, so a missed deletion cannot leave a
 * process (and its approvals) reachable.
 */
async function watch(ctx: Plugin.Context, pool: VaultSessionPool, signal: AbortSignal): Promise<void> {
  let delay = 500
  while (!signal.aborted) {
    try {
      const events = ctx.event.subscribe({ types: [...CLOSING_EVENTS], signal })
      pool.resume()
      for await (const event of events) {
        delay = 500
        const sessionID = (event.data as { sessionID?: unknown }).sessionID
        if (typeof sessionID !== "string") continue
        if (event.type === "session.deleted") await pool.deleteSession(sessionID)
        else await pool.closeSession(sessionID, `the session received ${event.type}`, { descendants: false })
      }
    } catch {
      // Fall through to suspension and a bounded backoff.
    }
    if (signal.aborted) return
    await pool.suspend("OpenCode session events are unavailable; retrying")
    await sleep(delay, signal)
    delay = Math.min(delay * 2, 30_000)
  }
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(done, ms)
    signal.addEventListener("abort", done, { once: true })
    function done() {
      clearTimeout(timer)
      signal.removeEventListener("abort", done)
      resolve()
    }
  })
}

export default createPlugin()
