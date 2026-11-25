// Optional checks against a real Control binary. Skipped unless
// CONTROL_VAULT_SMOKE_BIN names the binary to test.
//
//   CONTROL_VAULT_SMOKE_BIN=/path/to/control bun test test/smoke.test.ts
//
// The inactive check uses an empty temporary CONTROL_HOME, so it never reads
// the operator's state. The live check runs only with
// CONTROL_VAULT_SMOKE_LIVE=1, uses the current user's activated Vault, and
// leaves at most two pending approval requests, which expire unapproved.
// It never grants anything and sends nothing to a worker.

import { afterEach, describe, expect, test } from "bun:test"
import { spawn } from "node:child_process"
import { mkdtempSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { assertNoAmbientCredentials, childEnv, VAULT_ARGS } from "../src/index.ts"
import type { VaultSessionPool } from "../src/pool.ts"
import { alive, makePool } from "./helpers.ts"

const bin = process.env.CONTROL_VAULT_SMOKE_BIN
const live = process.env.CONTROL_VAULT_SMOKE_LIVE === "1"
const pools: VaultSessionPool[] = []
const dirs: string[] = []
afterEach(async () => {
  await Promise.all(pools.splice(0).map((pool) => pool.stop("smoke finished")))
  for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true })
})

describe.skipIf(!bin)("real control binary", () => {
  test("without Vault activation the frontend refuses and nothing keeps running", async () => {
    const home = mkdtempSync(join(tmpdir(), "control-vault-smoke-"))
    dirs.push(home)
    const pids: (number | undefined)[] = []
    const pool = makePool({
      preflight: () => assertNoAmbientCredentials(),
      spawn: () => {
        const child = spawn(bin!, [...VAULT_ARGS], {
          stdio: ["pipe", "pipe", "pipe"],
          env: { ...childEnv(), CONTROL_HOME: home, CONTROL_CONFIG: join(home, "node.json") },
        })
        pids.push(child.pid)
        return child
      },
    })
    pools.push(pool)
    const error = await pool.describe("ses_smoke", undefined).then(
      () => undefined,
      (e: Error) => e,
    )
    expect(error).toBeInstanceOf(Error)
    console.log(`inactive frontend: ${error!.message.replaceAll("\n", " | ")}`)
    expect(pool.size).toBe(0)
    for (const pid of pids) expect(alive(pid)).toBe(false)
  })

  test.skipIf(!live)("two sessions get separate broker connections and separate approval codes", async () => {
    const pool = makePool({
      preflight: () => assertNoAmbientCredentials(),
      spawn: () => spawn(bin!, [...VAULT_ARGS], { stdio: ["pipe", "pipe", "pipe"], env: childEnv() }),
    })
    pools.push(pool)
    const listed = (await pool.describe("ses_live_a", undefined)).result as { tools: { name: string }[] }
    const names = listed.tools.map((t) => t.name)
    console.log(`vault tools: ${names.join(", ")}`)
    const probe = names.includes("control_nodes") ? "control_nodes" : names[0]!
    const codes = await Promise.all(
      ["ses_live_a", "ses_live_b"].map(async (session) => {
        const outcome = await pool.call(session, probe, {})
        const text = JSON.stringify(outcome.result)
        console.log(`${session}: ${text.slice(0, 300)}`)
        return outcome.result.isError === true ? text : undefined
      }),
    )
    // Fresh connections are unapproved, so each must be refused with its own code.
    expect(codes[0]).toContain("approval_required")
    expect(codes[1]).toContain("approval_required")
    expect(codes[0]).not.toBe(codes[1])
    expect(pool.size).toBe(2)
  })
})
