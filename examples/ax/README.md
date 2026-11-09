# ax examples

Runnable `ax apply -f <file>` examples for the home-lab ax substrate. Each file
is self-contained; run them in order for the full tour.

```bash
export AX_SERVER=http://ax.syscd.tech:18080   # or let the CLI auto-detect
```

| File | Demonstrates |
|---|---|
| [01-hello-task.yaml](01-hello-task.yaml) | Smallest Task: digest-pinned image, command, `env`, `resources`, `debug` + ssh retrieval |
| [02-workspace-model.yaml](02-workspace-model.yaml) | Standalone `Workspace` (git repos) and `Model` (provider) objects |
| [03-agent-task.yaml](03-agent-task.yaml) | A real agent run: omp headless against Gemini, results written to `/workspace` |

## Lifecycle — apply → resume → use → delete

New Tasks start **Suspended**; `resume` schedules the worker:

```bash
ax apply -f 01-hello-task.yaml    # → Suspended (actor reserved, nothing runs)
ax resume task ax-example-hello   # → Running; spec.command execs on the worker
ax get task ax-example-hello      # phase: Suspended | Running | Succeeded | Failed
ax ssh ax-example-hello -- cat /workspace/report.txt   # needs spec.debug: true
ax delete task ax-example-hello   # kind is required: task | workspace | model
```

## Field surface on this substrate (verified by apply)

| Field | Status |
|---|---|
| `spec.image`, `spec.command`, `spec.debug` | ✓ |
| `spec.env`, `spec.resources.limits` | ✓ |
| `spec.config` | ✗ unknown field (upstream added it later) |
| `spec.workspace.name` | ✗ unknown field — no task↔workspace binding yet |

Images must be **digest-pinned**; a movable tag is rejected at resume
(`TemplateCreationFailed`). Registry quirk (verified): **docker.io images fail
to start** on this substrate — a correct `alpine@sha256:…` digest still dies
with `runsc start: exit status 128` — while **ghcr.io images run**. The
examples therefore use the platform's image, `ghcr.io/yurifrl/omp-remote-agents`
(single-arch manifest digest, the running `omp-remote-*` tasks' image).

Model credentials: the ExternalSecret contract (`charts/ax` →
`gemini-api-secret` / `GEMINI_API_KEY`) injects the provider key into every
task's env, so agent tasks need no secret wiring. `spec.secretKey` on Model is
accepted (applies cleanly); upstream's `spec.parameters` was not verified here.

## Gotchas

- `ax delete <name>` errors — the kind is required (`ax delete task <name>`).
- Tasks are immutable: re-applying the same name fails; delete first or use a
  new name.
- `ax ssh` requires a **Running** task **and** `spec.debug: true`; stale
  actors (13d old) may answer with `ingress failed`.
