#!/usr/bin/env bun
// Stand-in for `control mcp --vault-process` in tests. It is a real stdio MCP
// child whose only state is in-process memory, so any sharing between
// sessions would show up as shared state or a shared PID.

const args = process.argv.slice(2)
if (args.length !== 2 || args[0] !== "mcp" || args[1] !== "--vault-process") {
  process.stderr.write(`unexpected arguments: ${JSON.stringify(args)}\n`)
  process.exit(64)
}
const credentialEnv = ["CONTROL_TOKEN", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY"]
for (const name of credentialEnv) {
  if (process.env[name] !== undefined) {
    process.stderr.write(`${name} must not reach the Vault process\n`)
    process.exit(65)
  }
}

let approved = false
let count = 0
const cancelled: unknown[] = []

const tools = [
  "control_whoami",
  "control_approve",
  "control_count",
  "control_hang",
  "control_cancelled",
  "control_crash",
  "control_fail",
  "control_progress",
  "control_fleet",
].map((name) => ({ name, description: `Fake ${name}. Second sentence.`, inputSchema: { type: "object" } }))

function send(message: unknown) {
  process.stdout.write(`${JSON.stringify(message)}\n`)
}

function text(id: unknown, value: unknown, isError = false) {
  send({ jsonrpc: "2.0", id, result: { content: [{ type: "text", text: JSON.stringify(value) }], isError } })
}

function handle(message: { id?: unknown; method?: string; params?: Record<string, any> }) {
  const { id, method, params } = message
  if (method === "notifications/cancelled") {
    cancelled.push(params?.requestId)
    return
  }
  if (id === undefined) return
  if (method === "initialize") {
    send({
      jsonrpc: "2.0",
      id,
      result: {
        protocolVersion: params?.protocolVersion,
        capabilities: { tools: {} },
        serverInfo: { name: "fake-vault", version: "0" },
        instructions: `fake vault ${process.pid}`,
      },
    })
    return
  }
  if (method === "tools/list") return send({ jsonrpc: "2.0", id, result: { tools } })
  if (method !== "tools/call") return send({ jsonrpc: "2.0", id, error: { code: -32601, message: "no such method" } })
  switch (params?.name) {
    case "control_whoami":
      return text(id, { pid: process.pid, approved, count, argv: args, hasToken: credentialEnv.some((name) => process.env[name] !== undefined) })
    case "control_approve":
      approved = true
      return text(id, { approved })
    case "control_count":
      count++
      return text(id, { count })
    case "control_hang":
      return // never answers
    case "control_cancelled":
      return text(id, { cancelled })
    case "control_crash":
      process.exit(3)
    case "control_fleet":
      // Mirrors Vault: refuse before dispatch until this connection is approved.
      if (!approved) return text(id, { code: "approval_required", approvalCode: `R${process.pid}` }, true)
      return text(id, { ran: true })
    case "control_fail":
      return text(id, { error: "denied" }, true)
    case "control_progress":
      send({ jsonrpc: "2.0", method: "notifications/progress", params: { progressToken: params?._meta?.progressToken, progress: 1, message: "halfway" } })
      return text(id, { done: true })
    default:
      return send({ jsonrpc: "2.0", id, error: { code: -32602, message: "unknown tool" } })
  }
}

process.stderr.write(`approval code 4242-${process.pid}\n`)
let buffer = ""
process.stdin.setEncoding("utf8")
process.stdin.on("data", (chunk: string) => {
  buffer += chunk
  let newline: number
  while ((newline = buffer.indexOf("\n")) >= 0) {
    const line = buffer.slice(0, newline).trim()
    buffer = buffer.slice(newline + 1)
    if (line) handle(JSON.parse(line))
  }
})
process.stdin.on("end", () => process.exit(0))
