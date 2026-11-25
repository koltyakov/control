// Exercises the real plugin entrypoint against a mocked OpenCode V2 plugin
// context. Processes are real: the configured controlPath is the fake Vault
// fixture, started with the fixed `mcp --vault-process` arguments.

import { afterEach, describe, expect, test } from "bun:test"
import { chmodSync } from "node:fs"
import { ambientCredentials, createPlugin, parseOptions, TOOL_NAME, type PluginDeps } from "../src/index.ts"
import { alive, eventually, FIXTURE, spawnFake } from "./helpers.ts"

type Event = { type: string; data: { sessionID: string } }
type Execute = (input: unknown, context: unknown) => Promise<{ content: string }>

class EventStream implements AsyncIterable<Event> {
  private queue: Event[] = []
  private waiter: ((result: IteratorResult<Event>) => void) | undefined
  private done = false
  constructor(signal?: AbortSignal) {
    signal?.addEventListener("abort", () => this.end())
  }
  push(event: Event) {
    if (this.waiter) {
      const waiter = this.waiter
      this.waiter = undefined
      waiter({ value: event, done: false })
    } else this.queue.push(event)
  }
  end() {
    this.done = true
    this.waiter?.({ value: undefined, done: true })
    this.waiter = undefined
  }
  [Symbol.asyncIterator](): AsyncIterator<Event> {
    return {
      next: () => {
        const event = this.queue.shift()
        if (event) return Promise.resolve({ value: event, done: false })
        if (this.done) return Promise.resolve({ value: undefined, done: true })
        return new Promise((resolve) => (this.waiter = resolve))
      },
    }
  }
}

const cleanups: (() => Promise<void> | void)[] = []
afterEach(async () => {
  for (const cleanup of cleanups.splice(0)) await cleanup()
})

async function load(options: Record<string, unknown> = {}, deps: PluginDeps = {}) {
  chmodSync(FIXTURE, 0o755)
  const sessions = new Map<string, { id: string; parentID?: string }>([
    ["ses_root_a", { id: "ses_root_a" }],
    ["ses_root_b", { id: "ses_root_b" }],
    ["ses_parent", { id: "ses_parent" }],
    ["ses_sub_1", { id: "ses_sub_1", parentID: "ses_parent" }],
    ["ses_sub_2", { id: "ses_sub_2", parentID: "ses_parent" }],
  ])
  const streams: EventStream[] = []
  const subscribed: string[][] = []
  let execute: Execute | undefined
  const ctx = {
    options: { controlPath: FIXTURE, ...options },
    session: {
      get: async ({ sessionID }: { sessionID: string }) => {
        const session = sessions.get(sessionID)
        if (!session) throw new Error("SessionNotFoundError")
        return session
      },
    },
    event: {
      subscribe: ({ types, signal }: { types: string[]; signal?: AbortSignal }) => {
        subscribed.push(types)
        const stream = new EventStream(signal)
        streams.push(stream)
        return stream
      },
    },
    tool: {
      transform: async (callback: (editor: unknown) => void) => {
        callback({
          add: (tool: { name: string; execute: Execute }) => {
            expect(tool.name).toBe(TOOL_NAME)
            execute = tool.execute
          },
        })
        return { dispose: async () => undefined }
      },
    },
  }
  const cleanup = (await createPlugin(deps).setup(ctx as never)) as () => Promise<void>
  let unloaded = false
  const unload = async () => {
    if (!unloaded) {
      unloaded = true
      await cleanup()
    }
  }
  cleanups.push(unload)
  // Let the watcher subscribe before the first call.
  await eventually(() => streams.length > 0)
  const progress: Record<string, unknown[]> = {}
  const call = async (sessionID: string, input: unknown, signal?: AbortSignal) => {
    const updates = (progress[sessionID] ??= [])
    const result = await execute!(input, {
      sessionID,
      agent: "build",
      messageID: "msg_1",
      id: "call_1",
      signal: signal ?? new AbortController().signal,
      progress: async (update: unknown) => void updates.push(update),
    })
    return result
  }
  const whoami = async (sessionID: string) => JSON.parse((await call(sessionID, { tool: "control_whoami" })).content)
  return { call, whoami, streams, subscribed, unload, progress, sessions }
}

describe("OpenCode V2 plugin", () => {
  test("routes each host session, including subagents and siblings, to its own Vault process", async () => {
    const host = await load()
    await host.call("ses_parent", { tool: "control_approve", arguments: {} })
    const ids = ["ses_root_a", "ses_root_b", "ses_parent", "ses_sub_1", "ses_sub_2"]
    const seen = await Promise.all(ids.map((id) => host.whoami(id)))
    expect(new Set(seen.map((s) => s.pid)).size).toBe(ids.length)
    expect(seen.map((s) => s.approved)).toEqual([false, false, true, false, false])
    for (const s of seen) {
      expect(s.argv).toEqual(["mcp", "--vault-process"])
      expect(s.hasToken).toBe(false)
    }
  })

  test("tool input cannot choose or claim a session", async () => {
    const host = await load()
    const a = await host.whoami("ses_root_a")
    await expect(
      host.call("ses_root_b", { tool: "control_whoami", arguments: {}, sessionID: "ses_root_a" }),
    ).rejects.toThrow("chosen by OpenCode")
    // Arguments are passed to the tool, never used for routing.
    const viaArgs = JSON.parse(
      (await host.call("ses_root_b", { tool: "control_whoami", arguments: { sessionID: "ses_root_a" } })).content,
    )
    expect(viaArgs.pid).not.toBe(a.pid)
  })

  test("an unknown host session fails closed", async () => {
    const host = await load()
    await expect(host.whoami("ses_missing")).rejects.toThrow("SessionNotFoundError")
  })

  for (const name of ["CONTROL_TOKEN", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY"]) {
    test(`a nonempty ${name} in the host fails closed before any process starts`, async () => {
      let spawned = 0
      const host = await load({}, {
        env: { PATH: "/usr/bin", [name]: "secret-value-123" },
        spawn: () => {
          spawned++
          return spawnFake()
        },
      })
      const error = await host.whoami("ses_root_a").then(
        () => undefined,
        (e: Error) => e,
      )
      expect(error?.message).toContain(name)
      expect(error?.message).toContain("no Vault process was started")
      expect(error?.message).not.toContain("secret-value-123")
      expect(spawned).toBe(0)
      await expect(host.call("ses_root_a", { describe: true })).rejects.toThrow(name)
      expect(spawned).toBe(0)
    })
  }

  test("credential variables are matched case-insensitively and empty ones are allowed", () => {
    expect(ambientCredentials({ control_token: "x", CONTROL_USER_KEY: "", OTHER: "y" })).toEqual(["CONTROL_TOKEN"])
    expect(ambientCredentials({ CONTROL_TOKEN: "", CONTROL_SUPERUSER_KEY: "" })).toEqual([])
  })

  test("empty credential variables in the host are still removed from the child", async () => {
    const names = ["CONTROL_TOKEN", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY"]
    const previous = names.map((name) => process.env[name])
    for (const name of names) process.env[name] = ""
    try {
      const host = await load()
      // The fixture exits if any of them reaches it, even empty.
      expect((await host.whoami("ses_root_a")).hasToken).toBe(false)
    } finally {
      names.forEach((name, i) => {
        if (previous[i] === undefined) delete process.env[name]
        else process.env[name] = previous[i]
      })
    }
  })

  test("session.deleted terminates the session's process and its subagents' processes", async () => {
    const host = await load()
    expect(host.subscribed[0]).toContain("session.deleted")
    const parent = await host.whoami("ses_parent")
    const sub = await host.whoami("ses_sub_1")
    const other = await host.whoami("ses_root_a")
    host.streams[0]!.push({ type: "session.deleted", data: { sessionID: "ses_parent" } })
    expect(await eventually(() => !alive(parent.pid) && !alive(sub.pid))).toBe(true)
    expect(alive(other.pid)).toBe(true)
    await expect(host.whoami("ses_parent")).rejects.toThrow("deleted")
  })

  test("a committed revert closes the process; the next call starts fresh and says so", async () => {
    const host = await load()
    await host.call("ses_root_a", { tool: "control_approve" })
    const before = await host.whoami("ses_root_a")
    host.streams[0]!.push({ type: "session.revert.committed", data: { sessionID: "ses_root_a" } })
    expect(await eventually(() => !alive(before.pid))).toBe(true)
    const result = await host.call("ses_root_a", { tool: "control_whoami" })
    expect(result.content).toContain("did not carry over")
    const after = JSON.parse(result.content.slice(result.content.indexOf("{")))
    expect(after.pid).not.toBe(before.pid)
    expect(after.approved).toBe(false)
  })

  test("losing the event stream closes every process and refuses calls until resubscribed", async () => {
    const host = await load()
    const a = await host.whoami("ses_root_a")
    host.streams[0]!.end()
    expect(await eventually(() => !alive(a.pid))).toBe(true)
    await expect(host.whoami("ses_root_a")).rejects.toThrow("events are unavailable")
    expect(await eventually(() => host.streams.length > 1, 5000)).toBe(true)
    const result = await host.call("ses_root_a", { tool: "control_whoami" })
    expect(result.content).toContain("did not carry over")
    const after = JSON.parse(result.content.slice(result.content.indexOf("{")))
    expect(after.pid).not.toBe(a.pid)
    expect(after.approved).toBe(false)
  })

  test("unloading the plugin terminates every process", async () => {
    const host = await load()
    const pids = await Promise.all(["ses_root_a", "ses_sub_1", "ses_sub_2"].map(async (id) => (await host.whoami(id)).pid))
    await host.unload()
    for (const pid of pids) expect(alive(pid)).toBe(false)
  })

  test("an approval code appears only in the requesting session, and approval applies only there", async () => {
    const host = await load()
    const a = await host.whoami("ses_root_a")
    const b = await host.whoami("ses_sub_1")
    await expect(host.call("ses_root_a", { tool: "control_fleet" })).rejects.toThrow(`R${a.pid}`)
    await expect(host.call("ses_sub_1", { tool: "control_fleet" })).rejects.toThrow(`R${b.pid}`)
    // Stand-in for the operator granting session A's code.
    await host.call("ses_root_a", { tool: "control_approve" })
    expect(JSON.parse((await host.call("ses_root_a", { tool: "control_fleet" })).content)).toEqual({ ran: true })
    await expect(host.call("ses_sub_1", { tool: "control_fleet" })).rejects.toThrow("approval_required")
  })

  test("describe is routed through the calling session and needs no tool", async () => {
    const host = await load()
    const a = await host.whoami("ses_root_a")
    const described = JSON.parse((await host.call("ses_root_a", { describe: true })).content)
    expect(described.instructions).toBe(`fake vault ${a.pid}`)
    const b = JSON.parse((await host.call("ses_root_b", { describe: true })).content)
    expect(b.instructions).not.toBe(described.instructions)
    await expect(host.call("ses_root_a", { describe: true, arguments: {} })).rejects.toThrow("describe does not take")
    await expect(host.call("ses_root_a", {})).rejects.toThrow("tool is required")
  })

  test("MCP tool errors surface as tool failures", async () => {
    const host = await load()
    await expect(host.call("ses_root_a", { tool: "control_fail" })).rejects.toThrow("denied")
  })

  test("options are validated", () => {
    expect(() => parseOptions({ command: ["sh"] })).toThrow("unknown option")
    expect(() => parseOptions({ maxSessions: 0 })).toThrow("maxSessions")
    expect(parseOptions({}).controlPath).toBe("control")
    // Idle cleanup is opt-in.
    expect(parseOptions({}).idleMs).toBe(0)
    expect(parseOptions({ idleMinutes: 15 }).idleMs).toBe(15 * 60_000)
    expect(() => parseOptions({ idleMinutes: -1 })).toThrow("idleMinutes")
  })
})
