# agents

Control plane for **remote agents** — ephemeral, pod-isolated agents declared
Kubernetes-style. An `AgentTask` is to a running agent what a `Pod` spec is to a
container. This repo is the system; deployment manifests live in
[`home-systems`](https://github.com/yurifrl/home-systems).

See the design in
`home-systems/openspec/changes/remote-agents/` (proposal + design + specs).

## Status: MVP — controller only

What exists today:

- **`AgentTask` CRD** (`api/v1alpha1`) — spec: `type`, `prompt`, `repo`,
  `owner`, `runtime` (`adk`|`pi`), `lifecycle` (`ephemeral`|`persistent`);
  status: `phase`, `podName`, `artifacts`, `restarts`.
- **Controller** (`internal/controller/agenttask`) — reconciles an ephemeral
  `AgentTask` into a Pod (running `agent-entrypoint`) + a Service, with owner
  references for GC and an `activeDeadlineSeconds` hard ceiling. Pod phase is
  mapped back onto `AgentTask.status`.

Not yet built (next milestones, tracked in the OpenSpec change): the
`agent-entrypoint` image, the stateful A2A gateway, the pi extension, the beads
workstrator, and the persistent (`Deployment`-backed) lifecycle.

## Architecture

Package-oriented, separation of concerns, dependency injection via
[`uber/fx`](https://github.com/uber-go/fx):

```
cmd/agents/                 cobra root + `controller` subcommand
api/v1alpha1/               AgentTask CRD types (+kubebuilder markers)
internal/
  config/                   viper config (file AND env — see below) + fx module
  logging/                  charmbracelet/log adapted to logr + fx module
  kube/                     scheme, rest config, manager (fx lifecycle)
  controller/               fx module wiring reconcilers into the manager
    agenttask/              the AgentTask reconciler + pod/service builders
config/crd/                 generated CRD
config/rbac/                generated ClusterRole
config/samples/             example AgentTask
```

### Stack

- CLI: **cobra** + **viper**
- DI: **uber/fx**
- Logging: **charmbracelet/log** (adapted to `logr` for controller-runtime)
- Controller: **sigs.k8s.io/controller-runtime** directly (no kubebuilder
  scaffold), with **controller-gen** for DeepCopy + CRD/RBAC generation.

### Config: every setting is both a file key and an env var

`internal/config` uses viper so each setting resolves from, in order of
precedence: `AGENTS_*` env var → config file key → built-in default.

```bash
# env
AGENTS_NAMESPACE=agents AGENTS_AGENT_IMAGE=ghcr.io/yurifrl/agents:dev \
  go run ./cmd/agents controller

# or a file (same keys)
go run ./cmd/agents controller --config ./config.yaml   # or AGENTS_CONFIG=...
```

Nested keys map with `_`, e.g. `manager.metrics_addr` → `AGENTS_MANAGER_METRICS_ADDR`.

## Develop

This repo uses [Task](https://taskfile.dev):

```bash
task tidy        # go mod tidy
task generate    # DeepCopy methods (controller-gen)
task manifests   # CRD + RBAC into config/
task build       # compile all packages
task vet
task check       # tidy + generate + build + vet
task run:controller   # run against the current kubecontext
```

## Try it against a cluster

```bash
kubectl apply -f config/crd/agents.yurifrl.dev_agenttasks.yaml
task run:controller        # in another shell
kubectl apply -f config/samples/agenttask_researcher.yaml
kubectl get agenttasks     # short name: at
```

The pod will be `ImagePullBackOff` until the `agent-entrypoint` image exists —
that is the next milestone. The controller, CRD, status mapping, ownership/GC,
and deadline are what the MVP delivers.
