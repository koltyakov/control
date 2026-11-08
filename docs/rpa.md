# RPA migration

`rpa.run`, MCP `control_rpa`, the `rpa` configuration field, and the supplied Python helper have been removed. Legacy profiles containing `rpa`, including `null`, fail configuration loading with migration instructions.

Use [native agent-desktop](desktop.md) for protected operations, or the upstream CLI through `exec.run` for broader automation. These paths are not feature-equivalent to the removed adapter. See [migration and feature differences](desktop.md#migrate-from-rpa) before changing callers or tasks.
