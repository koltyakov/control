import { spawn } from "node:child_process"
import { fileURLToPath } from "node:url"
import { childEnv, VAULT_ARGS } from "../src/index.ts"
import { VaultSessionPool, type PoolOptions, type SessionFacts } from "../src/pool.ts"

export const FIXTURE = fileURLToPath(new URL("./fake-control.ts", import.meta.url))

/** Starts the fake Vault through the current runtime with the fixed frontend arguments. */
export function spawnFake() {
  return spawn(process.execPath, [FIXTURE, ...VAULT_ARGS], { stdio: ["pipe", "pipe", "pipe"], env: childEnv() })
}

export function alive(pid: number | undefined): boolean {
  if (pid === undefined) return false
  try {
    process.kill(pid, 0)
    return true
  } catch {
    return false
  }
}

/** Waits briefly for an asynchronous exit observed through events. */
export async function eventually(check: () => boolean, ms = 3000): Promise<boolean> {
  const deadline = Date.now() + ms
  while (Date.now() < deadline) {
    if (check()) return true
    await new Promise((resolve) => setTimeout(resolve, 20))
  }
  return check()
}

export function parse(result: { content?: unknown }): Record<string, any> {
  const content = result.content as { type: string; text: string }[]
  return JSON.parse(content[0]!.text)
}

export function makePool(overrides: Partial<PoolOptions> & { sessions?: Record<string, SessionFacts> } = {}) {
  const sessions = overrides.sessions
  const pool = new VaultSessionPool({
    spawn: spawnFake,
    verifySession: async (id) => {
      if (sessions && !(id in sessions)) throw new Error(`session ${id} not found`)
      return sessions?.[id] ?? {}
    },
    maxSessions: 8,
    idleMs: 60_000,
    startupTimeoutMs: 10_000,
    clientInfo: { name: "test", version: "0" },
    closeGraceMs: 500,
    ...overrides,
  })
  pool.resume()
  return pool
}
