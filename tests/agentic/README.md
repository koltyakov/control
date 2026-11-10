# Agentic benchmark recording

This opt-in recorder supports synthetic held-out CLI trials on a POSIX
orchestrator. It is not part of the Control binary, a fleet test runner, or an
execution sandbox. It adds no Python requirement to Control or the Go suite.

Run its self-contained checks without a fleet or credentials:

```sh
python3 -m unittest discover -s tests/agentic -v
```

## Configure a trial

Create an evidence directory outside the checkout. Keep hidden expected results
separate from evaluated-agent prompts. Its `agent-config.json` contains an
absolute CLI path and an `episodes` object keyed by unique episode IDs. Each
episode needs `node`, `nodeId`, `family`, `path`, `taskPrefix`, and `callLimit`.
Recovery episodes also need `originalTaskId`. Families are `join`, `recover`,
and `guard`. File paths use `workDir`-relative fixture roots. Match stable worker
IDs against fresh discovery before setup, inspect matching runbooks, and respect
existing leases. Use only explicitly authorized isolated workspaces.

For example, this is a configuration shape, not an execution request:

```json
{
  "cli": "/absolute/path/to/control",
  "episodes": {
    "trial-1": {
      "node": "assigned-user-worker",
      "nodeId": "DISCOVERED_STABLE_ID",
      "family": "recover",
      "path": "tasks/UNIQUE_SEED_ID",
      "taskPrefix": "trial-1-a-",
      "originalTaskId": "ORIGINAL_TASK_ID",
      "callLimit": 12
    }
  }
}
```

Invoke the Control gate with structured argv:

```sh
python3 tests/agentic/recorder.py --root /absolute/evidence --episode trial-1 control task get assigned-user-worker ORIGINAL_TASK_ID
```

The gate writes `traces/EPISODE.jsonl`. Starts reserve the call budget under a
file lock; finishes retain exit status, stdout, stderr and monotonic elapsed
time. A blocked call returns 125 without launching Control. Timeouts return 124
without retrying. Reconcile the original task ID after an uncertain result.

### Help recognition

The original temporary recorder treated argv index 1 as an execution target
before recognizing `exec --help`. It therefore rejected three local usage
requests as unauthorized workers. The reusable gate recognizes only the exact
`exec --help` and `exec -h` forms before target and recovery checks. A help flag
inside a worker command, task JSON, or a longer argv does not bypass the gate.

The CLI itself currently returns nonzero usage for target-free `exec --help`.
The recorder preserves that real result and marks it `blocked: false`; it does
not turn a CLI usage error into a success or invoke a replacement command.
This is a recorder fix, not a CLI contract change.

## Capture local shell failures independently

Start the process recorder before the inner shell parses or constructs commands:

```sh
python3 tests/agentic/recorder.py --root /absolute/evidence --episode trial-1 shell <<'REQUEST'
{"command":"if then","timeoutSeconds":90}
REQUEST
```

Use a structured tool input or an inline quoted heredoc, not a temporary request
file. Replace the sample command with the local command to evaluate. Inside it,
route every Control invocation through the Control gate above. The process
recorder starts `/bin/zsh -f -c` by default, with an explicit `shell` field for
another installed POSIX shell. Its working directory is `scratch/EPISODE`.

`shell-traces/EPISODE.jsonl` records local process starts and finishes separately
from Control calls. It captures syntax errors, zsh scalar-command expansion
failures, missing commands, launch failures, nonzero exits and timeouts even if
the Control gate never starts. Each record has a unique call ID. A timeout kills
the owned process group and retains partial output. Accepted remote tasks may
continue; local termination never proves that remote effects stopped.

The focused tests exercise those failure paths, real subprocess forwarding,
concurrent call-budget reservation, exact-help recognition, foreign-target and
ID rejection, path traversal, and prohibited tracked reads versus direct reads.
They do not establish complete agent-tool capture or Windows runtime behavior.

## Recording and grading limits

- Both wrappers are opt-in. An agent can invoke another binary, shell, Python
  process, or MCP tool outside them. This is not prevented or exhaustively
  detected. A direct binary inside the recorded shell is visible in the shell
  command/output, but is not automatically a gated Control event.
- A syntax error in the *outer launcher*, malformed recorder JSON/configuration,
  invalid request values such as a timeout above 300 seconds,
  or an unavailable Python runtime can occur before recording starts. Capture
  agent-tool input/results at the evaluator boundary as well when that facility
  is available. Do not replace missing telemetry with an agent's self-report.
- File tools and other non-shell tools are not recorded here. Cooperative scratch
  paths do not confine filesystem access, and an agent can edit evidence. No
  tamper-evident log or credential redaction is provided. Use synthetic inputs
  only, protect private references, and never record secret-bearing commands.
- Unrestricted `exec` is not restricted to the fixture tree by the gate. The
  wrappers inherit the evaluator's OS authority and Control login. Gate checks
  are benchmark scope checks, not a security boundary or a complete CLI parser.
- Capture buffers and logs retain whole process output. Keep trial diagnostics
  small and apply external storage/process limits for untrusted workloads.
  Background processes that detach from the process group can escape timeout
  cleanup. Recorder termination can leave unmatched starts; never count those as
  successful calls or silently resubmit them.
- Process status does not determine remote outcome. Grade retained task state,
  file bytes, protected hashes and absence of extra writes independently. Audit
  prefix-scoped task ledgers, including blocked attempts and unrecorded work.
  Report intentional validation exits separately from script mistakes.
- CLI and shell durations overlap and omit model reasoning. Sum neither as
  end-to-end latency. No token, cost, model-turn or exhaustive tool metric is
  collected.

Preserve the original prompts, scripts, hashes, results and interruptions before
resuming trials. Freeze executable, guidance, recorder and grader copies with
hashes. Use fresh sessions and fixtures for previously attempted mutations.
Report model-quota interruptions and unavailable workers separately from task
failures. A resumed cohort after guidance or recorder changes is a different
condition, even with the same model alias and grading checks. Coverage may be
combined as descriptive coverage only, not as a controlled before/after score.
