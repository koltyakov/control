# Terminology

[README](../README.md) · [Architecture](architecture.md) · [Engineering principles](principles.md) · [Protocol](protocol.md)

Use these terms in documentation, code comments, CLI help, dashboard text, and AI-facing instructions.

## Roles

| Term | Meaning |
| --- | --- |
| Orchestrator | The caller coordinating work: selecting workers, submitting tasks or workflows, and collecting results. It can be an AI agent, a script, or a person using the CLI. |
| Worker | A node performing requested work or exposing capabilities to the caller. It can fetch inputs and deliver outputs directly to other nodes. |
| Gateway | The shared service handling enrollment, discovery, connection signaling, encrypted relay fallback, and fleet administration. It does not schedule or execute application work. |

Orchestrator and worker are roles within an operation, not fixed machine types. The same node can coordinate one task and execute another. A worker can request work from another worker. "Remote" and "local" describe location relative to the caller, not separate runtime types.

## Components

| Term | Meaning |
| --- | --- |
| Client | The CLI or MCP component used by an orchestrator. It routes calls through a local node API or opens a standalone authenticated client session. Standalone clients are not enrolled fleet machines and expose no execution capabilities. |
| Node | The enrolled execution service running on a machine. It owns capabilities, tasks, artifacts, and peer connections, and can take either the orchestrator or worker role. |
| Gateway | The gateway component, implemented separately from nodes and clients. |
| AI agent | AI software coordinating work or invoked through a node's optional AI provider. "Agent" does not mean a Control node, client, gateway, or updater. |

Use "orchestrator machine" for the machine running the coordinating client or AI agent. It may also run a node, but remote execution does not require one. Use "worker node" for a node executing requested work, and "machine" for the host or its fleet registration.

## Naming rules

- Keep component names `client`, `node`, and `gateway` in packages, types, commands, and APIs. Do not introduce separate orchestrator and worker runtimes or a fixed execution-role enum.
- Use orchestrator and worker when describing who coordinates and who executes a particular operation.
- Reserve agent terminology for AI integrations. Existing `agents` configuration, `agent.run`, AI-client installation helpers, and third-party agent names retain their meaning.
- Deployment labels such as `role=worker` are user-defined selection metadata, not runtime types or authorization roles.
- Authentication roles such as `account`, `common`, and `superuser`, and session backend values `client` and `node-api`, are separate from execution roles. Keep their existing identifiers.

For example: "The orchestrator uses a client to submit a task to a worker node. The gateway helps establish the connection. The worker executes the task and delivers its artifact to another node."

Older [design decisions](decisions.md) sometimes call the node service an agent. Use the terms above for current documentation and new code; those historical references do not define another component type.
