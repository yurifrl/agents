# Agent Definitions and Skills Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Evolve `AgentTask` from profile-name-only tasks into declarative agent definitions with inline Agent.md mode, reusable `AgentSkill` resources, a typed tool engine with a native Anthropic tool-use loop, and persistent Deployment-backed loop agents.

**Architecture:** Extend the existing controller/pod/gateway pipeline. `AgentTask` gains `spec.definition` (inline Agent.md), `spec.skills`, and execution fields (`image`, `serviceAccountName`, `secretName`, `env`). The controller resolves skills deterministically, composes instruction + tool policy into an env-provided bundle (hash on pod template), and creates either the existing ephemeral pod or a new per-task Deployment. In-pod, a new `internal/tool` registry executes typed tools and `internal/agent` runs a bounded native tool-use loop; loop agents self-start on pod boot.

**Tech Stack:** Go 1.26, controller-runtime (envtest for integration tests), Anthropic Messages API (`httptest` fakes), kubebuilder/controller-gen, stdlib-only A2A HTTP+SSE.

## Global Constraints

- Preserve profile mode: `spec.type` + `spec.prompt` keeps working unchanged; exactly one of `type` or `definition` is set (validation failure otherwise).
- `exec` accepts argv vectors only — never a shell string, never `sh -c`.
- `http` only reaches hosts in `allowHosts`; `notify` never exposes the webhook URL to the model (URL comes from `urlFromEnv`).
- Secret values are redacted from tool results and A2A events.
- Missing explicit skill = validation failure before workload creation; empty label-selector result = valid, zero skills.
- Tool-grant merge: union of allow rules/hosts with dedup; strictest non-zero call limit; shortest non-zero timeout; longest non-zero cooldown.
- Persistent lifecycle = one Deployment + one Service per `AgentTask`, owner refs for GC, bundle-hash annotation triggers rollout.
- Loop agents get a fresh model context per iteration plus only the prior iteration's compact final report.
- No `discord`/`alertmanager`/`kubernetes` tools in core; the example composes `exec`/`http`/`notify` + RBAC.
- API stays `v1alpha1`; run `task generate && task manifests` after any `api/` change.
- Tests are written before implementation in every task (TDD).
- Do not commit, push, or sync without explicit user approval; each task ends with a status report instead.

---

## File Map

| File | Responsibility | Status |
|---|---|---|
| `internal/agentmd/agentmd.go` | Parse Agent.md incl. `tools:` grants and `loop:` block | Modify |
| `internal/agentmd/agentmd_test.go` | Parser/validation tests | Create |
| `internal/tool/tool.go` | `Tool` interface, `Registry`, `Result`, policy enforcement (allow/timeout/limit/cooldown), redaction | Create |
| `internal/tool/tool_test.go` | Registry + policy tests | Create |
| `internal/tool/builtin.go` | `exec`, `http`, `notify`, `sleep` implementations | Create |
| `internal/tool/builtin_test.go` | Built-in tool tests (httptest, fake exec) | Create |
| `internal/agent/claude.go` | Content-block messages API + bounded native tool-use loop | Modify |
| `internal/agent/claude_test.go` | Fake Anthropic server multi-turn tests | Create |
| `internal/runner/runner.go` | Wire registry into runs; emit tool A2A events | Modify |
| `internal/runner/loop.go` | Loop runner: interval iterations, fresh context + prior report | Create |
| `internal/runner/loop_test.go` | Loop reset/continuity tests | Create |
| `internal/a2a/types.go` | `EventToolCall` / `EventToolResult` event types | Modify |
| `api/v1alpha1/agentskill_types.go` | `AgentSkill` CRD (description, instructions, tools) | Create |
| `api/v1alpha1/agenttask_types.go` | `definition`, `skills`, `image`, `serviceAccountName`, `secretName`, `env`; status: resolved skills, bundle hash | Modify |
| `api/v1alpha1/zz_generated.deepcopy.go` | Regenerated | Modify (generated) |
| `internal/controller/agenttask/skills.go` | Skill resolution, instruction composition, tool merge, bundle hash | Create |
| `internal/controller/agenttask/skills_test.go` | Resolution/merge/hash unit tests | Create |
| `internal/controller/agenttask/reconciler.go` | Mode validation, inline branch, persistent Deployment reconcile, skill watch | Modify |
| `internal/controller/agenttask/resources.go` | Pod/Deployment/Service builders with bundle env + exec fields | Modify |
| `internal/controller/agenttask/reconciler_test.go` | envtest integration tests | Create |
| `internal/gateway/gateway.go` | `CreateRequest` inline mode + validation | Modify |
| `internal/gateway/gateway_test.go` | Gateway validation tests for both modes | Create |
| `cmd/agents/entrypoint.go` | Load definition from bundle env; loop self-start when `loop.interval` set | Modify |
| `config/crd/`, `config/rbac/` | Regenerated CRDs + RBAC (agentskills, deployments) | Modify (generated) |
| `examples/kubernetes-crashloop-remediator/deploy.yaml` | SA, RBAC, ExternalSecret, 2 skills, 1 inline AgentTask | Create |
| `examples/kubernetes-crashloop-remediator/README.md` | Setup/operation | Create |
| `docs/architecture.md`, `docs/status.md` | Definition modes, skills, tool loop, persistent lifecycle | Modify |

---

## Task 1: Shared tool policy schema in Agent.md

**Files:** `internal/agentmd/agentmd.go`, `internal/agentmd/agentmd_test.go`

**Interfaces produced:**
```go
type ToolGrant struct {
    Name       string     `json:"name"`
    Allow      [][]string `json:"allow,omitempty"`
    AllowHosts []string   `json:"allowHosts,omitempty"`
    URLFromEnv string     `json:"urlFromEnv,omitempty"`
    MaxCalls   int        `json:"maxCalls,omitempty"`
    Timeout    string     `json:"timeout,omitempty"`
    Cooldown   string     `json:"cooldown,omitempty"`
}
type LoopSpec struct { Interval string `json:"interval"` }
// Definition gains: Tools []ToolGrant, Loop *LoopSpec
func (d *Definition) LoopInterval() (time.Duration, error)
```

**Interfaces consumed:** existing `Parse`, `split`.

- [ ] Write failing tests: parse grants, `loop.interval`, defaults, invalid duration, unknown tool, empty interval.
- [ ] Run: `go test ./internal/agentmd/ -run TestParse -v`; confirm failures.
- [ ] Change `Definition.Tools` to `[]ToolGrant`, add `Loop *LoopSpec`, extend `validate()`. Migrate `agents/*.md` tool lists to grant objects.
- [ ] Run: `go test ./internal/agentmd/ && go build ./...`.
- [ ] Report changed files and validation output; do not commit.

## Task 2: Tool engine — registry, policy, built-ins

**Files:** `internal/tool/tool.go`, `internal/tool/builtin.go`, `internal/tool/tool_test.go`, `internal/tool/builtin_test.go`

**Interfaces produced:**
```go
type Call struct { Name string; Arguments json.RawMessage }
type Result struct { Text string; IsError bool }
type Tool interface {
    Name() string
    Schema() json.RawMessage
    Execute(ctx context.Context, args json.RawMessage) (Result, error)
}
type Registry struct { /* grants, redactors, per-tool state */ }
func NewRegistry(grants []agentmd.ToolGrant, secrets []string) (*Registry, error)
func (r *Registry) Schemas() []json.RawMessage
func (r *Registry) Execute(ctx context.Context, c Call) Result
```

**Interfaces consumed:** `agentmd.ToolGrant`.

- [ ] Write failing tests: unknown tool; schema failure; argv prefix denial; host denial; `MaxCalls`; `Timeout`; `Cooldown`; secret redaction.
- [ ] Run: `go test ./internal/tool/ -v`; confirm failures.
- [ ] Implement `Registry` policy checks, timeout, counters/cooldowns, redaction.
- [ ] Write failing built-in tests: argv-only exec, bounded `http`, `notify` via `httptest`, cancellable `sleep`.
- [ ] Implement `NewExec`, `NewHTTP`, `NewNotify`, `NewSleep`.
- [ ] Run: `go test ./internal/tool/ && go vet ./internal/tool/`.
- [ ] Report changed files and validation output; do not commit.

## Task 3: Anthropic native tool-use loop + A2A tool events

**Files:** `internal/agent/claude.go`, `internal/agent/claude_test.go`, `internal/a2a/types.go`, `internal/runner/runner.go`

**Interfaces produced:**
```go
type ToolExecutor interface {
    Schemas() []json.RawMessage
    Execute(ctx context.Context, c tool.Call) tool.Result
}
type LoopRequest struct {
    Model, System, Prompt string
    MaxTokens, MaxTurns   int
    Tools                 ToolExecutor
    OnToolEvent           func(name string, isResult bool, text string)
}
func (c *Client) RunLoop(ctx context.Context, req LoopRequest) (string, error)
// a2a: EventToolCall, EventToolResult, Event.Tool *ToolEvent
```

**Interfaces consumed:** `tool.Registry`, existing `agent.Client`, `a2a.Event`.

- [ ] Write failing fake-Anthropic tests: tool round trip, multi-turn, invalid args recovery, max turns, cancellation, event callbacks.
- [ ] Run: `go test ./internal/agent/ -v`; confirm failures.
- [ ] Rework messages to content blocks; keep `Run` as a wrapper over `RunLoop` with nil tools.
- [ ] Add A2A tool event types; map runner callbacks to events.
- [ ] Run: `go test ./internal/agent/ ./internal/a2a/ ./internal/runner/ && go build ./...`.
- [ ] Report changed files and validation output; do not commit.

## Task 4: AgentSkill CRD + resolver

**Files:** `api/v1alpha1/agentskill_types.go`, `api/v1alpha1/agenttask_types.go`, `internal/controller/agenttask/skills.go`, `internal/controller/agenttask/skills_test.go`, generated CRD/deepcopy files

**Interfaces produced:**
```go
type AgentSkillSpec struct {
    Description  string              `json:"description"`
    Instructions string              `json:"instructions"`
    Tools        []agentmd.ToolGrant `json:"tools,omitempty"`
}
type SkillRef struct { Name string; Selector *metav1.LabelSelector }
type ResolvedBundle struct {
    Definition  *agentmd.Definition
    Instruction string
    Grants      []agentmd.ToolGrant
    SkillNames  []string
    Hash        string
}
func ResolveSkills(ctx context.Context, c client.Client, ns string, refs []SkillRef) ([]agentsv1alpha1.AgentSkill, error)
func Compose(def *agentmd.Definition, skills []agentsv1alpha1.AgentSkill) *ResolvedBundle
func MergeGrants(a, b []agentmd.ToolGrant) []agentmd.ToolGrant
```

**Interfaces consumed:** `agentmd.Definition`, `agentmd.ToolGrant`, controller-runtime client.

- [ ] Write failing tests for order, sorting, dedup, missing skill, empty selector, composition, merge rules, hash changes.
- [ ] Run targeted tests; confirm failures.
- [ ] Create CRD types and status fields; run `task generate && task manifests`.
- [ ] Implement `skills.go`.
- [ ] Run: `go test ./internal/controller/agenttask/ && go build ./...`.
- [ ] Report changed files and validation output; do not commit.

## Task 5: AgentTask inline mode + execution fields

**Files:** `api/v1alpha1/agenttask_types.go`, `internal/controller/agenttask/reconciler.go`, `internal/controller/agenttask/resources.go`, `internal/controller/agenttask/reconciler_test.go`, `cmd/agents/entrypoint.go`

**Interfaces produced:**
```go
// AgentTaskSpec gains:
Definition string
Skills     []SkillRef
Image      string
ServiceAccountName string
SecretName string
Env        []corev1.EnvVar
// Bundle env: AGENT_BUNDLE_DEFINITION, AGENT_BUNDLE_INSTRUCTION,
// AGENT_BUNDLE_TOOLS, AGENT_BUNDLE_HASH
```

**Interfaces consumed:** `agentmd.Parse`, `ResolvedBundle`, `config.Config`.

- [ ] Write failing envtest tests: bundle env, mode exclusivity, invalid definition, execution-field propagation, profile-mode regression.
- [ ] Run targeted tests; confirm failures.
- [ ] Make `Type`/`Prompt` optional, add fields and `validateMode`; regenerate manifests.
- [ ] Reconciler validates, resolves bundle, and passes it to builders.
- [ ] Entrypoint loads bundle definition and constructs optional tool registry.
- [ ] Run: `go test ./... && go build ./...`.
- [ ] Report changed files and validation output; do not commit.

## Task 6: Persistent Deployment + loop self-start

**Files:** `internal/controller/agenttask/reconciler.go`, `internal/controller/agenttask/resources.go`, `internal/controller/agenttask/reconciler_test.go`, `internal/runner/loop.go`, `internal/runner/loop_test.go`, `cmd/agents/entrypoint.go`

**Interfaces produced:**
```go
type LoopRunner struct { /* runner, interval */ }
func NewLoopRunner(r *Runner, interval time.Duration) *LoopRunner
func (l *LoopRunner) Run(ctx context.Context, emit func(a2a.Event)) error
func (r *Reconciler) buildDeployment(task *agentsv1alpha1.AgentTask, b *ResolvedBundle) *appsv1.Deployment
```

**Interfaces consumed:** `runner.Runner`, `Definition.LoopInterval()`, `ResolvedBundle`.

- [ ] Write failing loop tests: cancellation, fresh context, prior report, interval, iteration error continues.
- [ ] Run targeted tests; confirm failures; implement `loop.go`.
- [ ] Write failing envtest tests: Deployment + Service, owner refs, status mapping, hash rollout, GC, ephemeral regression.
- [ ] Implement persistent reconcile, Deployment ownership/RBAC, skill watch requeue.
- [ ] Entrypoint self-starts `LoopRunner` when `loop.interval` is set.
- [ ] Run: `go test ./... && go build ./...`.
- [ ] Report changed files and validation output; do not commit.

## Task 7: Gateway inline-definition API

**Files:** `internal/gateway/gateway.go`, `internal/gateway/gateway_test.go`

**Interfaces produced:**
```go
// CreateRequest gains:
Definition string
Skills     []SkillRef
Image      string
ServiceAccountName string
Env        map[string]string
```

**Interfaces consumed:** extended `AgentTaskSpec`.

- [ ] Write failing tests: profile regression, inline CRD construction, both modes rejected, invalid inline definition rejected, profile mode requires prompt.
- [ ] Run: `go test ./internal/gateway/ -v`; confirm failures.
- [ ] Implement validation and CRD construction; dispatch only ephemeral tasks.
- [ ] Run: `go test ./internal/gateway/ && go build ./...`.
- [ ] Report changed files and validation output; do not commit.

## Task 8: Example + docs

**Files:** `examples/kubernetes-crashloop-remediator/deploy.yaml`, `examples/kubernetes-crashloop-remediator/README.md`, `docs/architecture.md`, `docs/status.md`, `Dockerfile`, `Taskfile.yml`

- [ ] Write failing validation test: parse `deploy.yaml`; validate AgentTask mode/definition; assert RBAC covers exactly the skill-granted kubectl operations.
- [ ] Run test; confirm failure.
- [ ] Author `deploy.yaml` with ServiceAccount, Role/RoleBinding, ExternalSecret, two skills, and one inline AgentTask.
- [ ] Add mocked end-to-end runner test with fake Anthropic, Alertmanager, and Discord endpoints.
- [ ] Add `agents-kubectl` image variant without changing the default image.
- [ ] Write example README; update architecture/status docs; leave top-level README unchanged.
- [ ] Run: `go test ./... && task check`.
- [ ] Report changed files and validation output; do not commit.

---

## Self-Review

- Every spec goal maps to a task: inline mode T5, skills T4, tool engine T2, model loop T3, loop agents T6, persistent lifecycle T6, gateway T7, example T8.
- Profile mode is preserved through parser compatibility, `Run` wrapping `RunLoop`, and regression tests.
- Every task has exact files, interfaces, test-first steps, and Go test commands.
- Safety boundaries have named tests: argv-only exec, host allowlist, URL hiding, redaction, call limits, cooldown.
- Generated artifacts are regenerated after API changes.
- No out-of-scope AgentDefinition CRD, workflow engine, Discord coupling, or durable gateway store.
