# pi-remote-agents

A pi extension exposing **remote agents** as explicit tools, distinct from local
subagents. It talks to the `agents` gateway.

## Tools

- `RemoteAgent` — dispatch work to a remote pod-isolated agent. Foreground streams
  the result; `background: true` returns a task id. `output: pr` opens a PR on `repo`.
- `RemoteAgentList` — list this owner's tasks (`unacknowledged: true` for unseen results).
- `RemoteAgentGet` — fetch one task (state, result, artifacts).
- `RemoteAgentKill` — cancel a running task (deletes its pod).

On `session_start` it reconciles: notifies if results finished while pi was away.

## Config (env)

- `AGENTS_GATEWAY_URL` — gateway base URL (default `http://localhost:9090`).
- `AGENTS_OWNER` — durable owner key (default `$USER`).

## Install

Symlink or copy into a pi auto-discovery path, then `/reload`:

```bash
ln -s "$PWD/pi/pi-remote-agents.ts" ~/.pi/agent/extensions/pi-remote-agents.ts
# or project-local: .pi/extensions/
```

Run the gateway it targets (in-cluster, or port-forwarded for local use):

```bash
kubectl -n agents port-forward svc/agents-gateway 9090:9090
```

> The gateway dispatches to in-pod A2A servers via cluster DNS, so it is meant to
> run **in the cluster**. `RemoteAgentSteer` and foreground-disconnect cancel are
> not implemented yet (see ../docs/status.md).
