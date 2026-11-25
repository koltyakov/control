import { afterEach, describe, expect, test } from "bun:test"
import type { VaultSessionPool } from "../src/pool.ts"
import { UncertainOutcomeError } from "../src/stdio-mcp.ts"
import { alive, eventually, makePool, parse, spawnFake } from "./helpers.ts"

const pools: VaultSessionPool[] = []
function track(pool: VaultSessionPool) {
  pools.push(pool)
  return pool
}
afterEach(async () => {
  await Promise.all(pools.splice(0).map((pool) => pool.stop("test finished")))
})

async function whoami(pool: VaultSessionPool, session: string) {
  return parse((await pool.call(session, "control_whoami", {})).result)
}

describe("session isolation", () => {
  test("two root sessions get separate processes and never share approvals", async () => {
    const pool = track(makePool())
    await pool.call("ses_a", "control_approve", {})
    const a = await whoami(pool, "ses_a")
    const b = await whoami(pool, "ses_b")
    expect(a.pid).not.toBe(b.pid)
    expect(a.approved).toBe(true)
    expect(b.approved).toBe(false)
    expect(pool.size).toBe(2)
  })

  test("parent, child and sibling sessions each get their own process", async () => {
    const pool = track(
      makePool({ sessions: { ses_parent: {}, ses_kid1: { parentID: "ses_parent" }, ses_kid2: { parentID: "ses_parent" } } }),
    )
    await pool.call("ses_parent", "control_approve", {})
    await pool.call("ses_kid1", "control_count", {})
    const parent = await whoami(pool, "ses_parent")
    const kid1 = await whoami(pool, "ses_kid1")
    const kid2 = await whoami(pool, "ses_kid2")
    expect(new Set([parent.pid, kid1.pid, kid2.pid]).size).toBe(3)
    expect(kid1.approved).toBe(false)
    expect(kid2.approved).toBe(false)
    expect(kid1.count).toBe(1)
    expect(kid2.count).toBe(0)
    expect(parent.count).toBe(0)
  })

  test("concurrent first calls from one session share one process", async () => {
    const pool = track(makePool())
    const results = await Promise.all([1, 2, 3].map(() => whoami(pool, "ses_same")))
    expect(new Set(results.map((r) => r.pid)).size).toBe(1)
    expect(pool.size).toBe(1)
  })

  test("the frontend receives only the fixed arguments, and empty credential variables are removed", async () => {
    const previous = process.env.CONTROL_TOKEN
    process.env.CONTROL_TOKEN = ""
    try {
      const pool = track(makePool())
      const me = await whoami(pool, "ses_env")
      expect(me.argv).toEqual(["mcp", "--vault-process"])
      expect(me.hasToken).toBe(false)
    } finally {
      if (previous === undefined) delete process.env.CONTROL_TOKEN
      else process.env.CONTROL_TOKEN = previous
    }
  })

  test("a failing preflight refuses before verifying or spawning, and also refuses running sessions", async () => {
    let blocked = false
    let spawned = 0
    let verified = 0
    const pool = track(
      makePool({
        preflight: () => {
          if (blocked) throw new Error("host holds credentials")
        },
        spawn: () => {
          spawned++
          return spawnFake()
        },
        verifySession: async () => {
          verified++
          return {}
        },
      }),
    )
    await whoami(pool, "ses_running")
    blocked = true
    await expect(pool.call("ses_new", "control_whoami", {})).rejects.toThrow("host holds credentials")
    await expect(pool.call("ses_running", "control_whoami", {})).rejects.toThrow("host holds credentials")
    expect(spawned).toBe(1)
    expect(verified).toBe(1)
    expect(pool.size).toBe(1)
  })
})

describe("lifecycle", () => {
  test("deleting a parent terminates it and its known children", async () => {
    const pool = track(makePool({ sessions: { ses_p: {}, ses_c: { parentID: "ses_p" }, ses_other: {} } }))
    const p = await whoami(pool, "ses_p")
    const c = await whoami(pool, "ses_c")
    const other = await whoami(pool, "ses_other")
    await pool.deleteSession("ses_p")
    expect(alive(p.pid)).toBe(false)
    expect(alive(c.pid)).toBe(false)
    expect(alive(other.pid)).toBe(true)
    await expect(pool.call("ses_p", "control_whoami", {})).rejects.toThrow("deleted")
    await expect(pool.call("ses_c", "control_whoami", {})).rejects.toThrow("deleted")
  })

  test("stop terminates every process and refuses new calls", async () => {
    const pool = track(makePool())
    const pids = [(await whoami(pool, "ses_1")).pid, (await whoami(pool, "ses_2")).pid]
    await pool.stop("unload")
    for (const pid of pids) expect(alive(pid)).toBe(false)
    await expect(pool.call("ses_1", "control_whoami", {})).rejects.toThrow("unload")
  })

  test("idle cleanup is disabled when idleMs is 0", async () => {
    let now = 1_000
    const pool = track(makePool({ idleMs: 0, now: () => now }))
    await pool.call("ses_keep", "control_approve", {})
    const first = await whoami(pool, "ses_keep")
    now += 48 * 60 * 60_000
    await pool.sweepIdle()
    expect(alive(first.pid)).toBe(true)
    const again = await pool.call("ses_keep", "control_whoami", {})
    expect(again.notice).toBeUndefined()
    expect(parse(again.result)).toMatchObject({ pid: first.pid, approved: true })
  })

  test("opt-in idle cleanup closes processes and a replacement starts with no approvals", async () => {
    let now = 1_000
    const pool = track(makePool({ idleMs: 100, now: () => now }))
    await pool.call("ses_idle", "control_approve", {})
    const first = await whoami(pool, "ses_idle")
    now += 50
    await pool.sweepIdle()
    expect(alive(first.pid)).toBe(true)
    now += 200
    await pool.sweepIdle()
    expect(alive(first.pid)).toBe(false)
    const outcome = await pool.call("ses_idle", "control_whoami", {})
    expect(outcome.notice).toContain("did not carry over")
    const second = parse(outcome.result)
    expect(second.pid).not.toBe(first.pid)
    expect(second.approved).toBe(false)
  })

  test("the number of live processes is bounded and excess sessions fail closed", async () => {
    const pool = track(makePool({ maxSessions: 2 }))
    await whoami(pool, "ses_1")
    await whoami(pool, "ses_2")
    await expect(pool.call("ses_3", "control_whoami", {})).rejects.toThrow("already hold a Vault process")
    expect(pool.size).toBe(2)
    await pool.deleteSession("ses_1")
    expect((await whoami(pool, "ses_3")).approved).toBe(false)
  })

  test("suspension closes every process until resumed", async () => {
    const pool = track(makePool())
    const { pid } = await whoami(pool, "ses_s")
    await pool.suspend("events lost")
    expect(alive(pid)).toBe(false)
    await expect(pool.call("ses_s", "control_whoami", {})).rejects.toThrow("events lost")
    pool.resume()
    const outcome = await pool.call("ses_s", "control_whoami", {})
    expect(parse(outcome.result).pid).not.toBe(pid)
    expect(outcome.notice).toContain("did not carry over")
  })

  test("an unconfirmed session never starts a process", async () => {
    let spawned = 0
    const pool = track(
      makePool({
        sessions: {},
        spawn: () => {
          spawned++
          throw new Error("must not spawn")
        },
      }),
    )
    await expect(pool.call("ses_ghost", "control_whoami", {})).rejects.toThrow("not found")
    expect(spawned).toBe(0)
    expect(pool.size).toBe(0)
  })

  test("a malformed host session identity fails closed", async () => {
    const pool = track(makePool())
    await expect(pool.call("", "control_whoami", {})).rejects.toThrow("session identity")
    await expect(pool.call(undefined as unknown as string, "control_whoami", {})).rejects.toThrow("session identity")
    expect(pool.size).toBe(0)
  })
})

describe("no replay", () => {
  test("a crash mid-call is reported as uncertain and never replayed", async () => {
    const pool = track(makePool())
    await pool.call("ses_crash", "control_count", {})
    const first = await whoami(pool, "ses_crash")
    const crash = pool.call("ses_crash", "control_crash", {})
    await expect(crash).rejects.toBeInstanceOf(UncertainOutcomeError)
    expect(await eventually(() => !alive(first.pid))).toBe(true)
    const outcome = await pool.call("ses_crash", "control_whoami", {})
    const second = parse(outcome.result)
    expect(second.pid).not.toBe(first.pid)
    expect(second.count).toBe(0)
    expect(outcome.notice).toContain("did not carry over")
  })

  test("cancellation notifies the process, keeps it, and reports an uncertain outcome", async () => {
    const pool = track(makePool())
    const before = await whoami(pool, "ses_cancel")
    const abort = new AbortController()
    const hang = pool.call("ses_cancel", "control_hang", {}, { signal: abort.signal })
    await new Promise((resolve) => setTimeout(resolve, 100))
    abort.abort()
    await expect(hang).rejects.toBeInstanceOf(UncertainOutcomeError)
    const cancelled = parse((await pool.call("ses_cancel", "control_cancelled", {})).result).cancelled
    expect(cancelled.length).toBe(1)
    expect((await whoami(pool, "ses_cancel")).pid).toBe(before.pid)
  })

  test("describe lists this session's tools and schemas without invoking anything", async () => {
    const pool = track(makePool())
    const all = (await pool.describe("ses_d", undefined)).result as { instructions: string; tools: { name: string; description: string }[] }
    expect(all.tools.map((t) => t.name)).toContain("control_crash")
    expect(all.tools[0]!.description).toBe("Fake control_whoami.")
    const one = (await pool.describe("ses_d", "control_crash")).result as { tool: { name: string; inputSchema: unknown } }
    expect(one.tool.name).toBe("control_crash")
    expect(one.tool.inputSchema).toEqual({ type: "object" })
    // Describing control_crash did not run it: the same process still answers.
    const me = await whoami(pool, "ses_d")
    expect(all.instructions).toBe(`fake vault ${me.pid}`)
    await expect(pool.describe("ses_d", "control_nope")).rejects.toThrow("Available:")
  })

  test("unknown tools are rejected with this session's tool names", async () => {
    const pool = track(makePool())
    await expect(pool.call("ses_t", "control_nope", {})).rejects.toThrow("control_whoami")
    await expect(pool.call("ses_t", "bad name!", {})).rejects.toThrow("MCP tool name")
  })

  test("progress notifications reach only the calling session", async () => {
    const pool = track(makePool())
    const a: unknown[] = []
    const b: unknown[] = []
    await pool.call("ses_pa", "control_progress", {}, { progress: (u) => a.push(u) })
    await pool.call("ses_pb", "control_whoami", {}, { progress: (u) => b.push(u) })
    expect(a).toContainEqual({ status: "halfway" })
    expect(b).not.toContainEqual({ status: "halfway" })
  })
})
