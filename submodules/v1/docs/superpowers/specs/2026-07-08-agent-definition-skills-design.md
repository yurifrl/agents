# Agent Definitions and Skills

_Date: 2026-07-08_

## Summary

Evolve the remote-agent product from profile-name-only tasks into declarative agent definitions. An `AgentTask` can carry a complete inline Agent.md document. Reusable capabilities live in a new `AgentSkill` CRD and can be attached explicitly by name or dynamically by label selector.

The first consumer is a Kubernetes CrashLoopBackOff remediation example. The example is YAML-only: it declares skills, RBAC, ExternalSecret wiring, and one self-contained `AgentTask`. No example-specific Go controller or user-managed profile ConfigMap is introduced.

## Goals

- Make the workload CRD the agent definition boundary.
- Preserve the existing profile mode for current users of `type` + `prompt`.
- Add an inline Agent.md mode for self-contained agent deployments.
- Add reusable, labeled `AgentSkill` resources.
- Compose skill instructions and tool grants deterministically.
- Provide a native, typed model tool-use loop.
- Provide generic loop behavior for watch-style agents.
- Implement persistent agents as one Deployment per `AgentTask`.
- Keep Kubernetes, Alertmanager, and Discord concerns out of the core engine.
- Demonstrate the product with a small CrashLoopBackOff remediation example.

## Non-goals

- A separate `AgentDefinition` CRD. `AgentTask` is already the deployed workload/definition boundary; another definition resource would duplicate it.
- An example-specific remediation controller.
- A generic workflow engine.
- Discord approval workflows in the first version.
- A durable gateway task store.
- Multi-cluster orchestration.
- Arbitrary remediation actions.
- A top-level README rewrite.

## Current state

`AgentTask` currently selects an installed profile through `spec.type` and passes task input through `spec.prompt`. Profiles are loaded from the embedded filesystem or `AGENT_PROFILES_DIR`. The pod starts an A2A server and the gateway invokes it.

The model runtime performs one completion. `Agent.md` already has a `tools` field, but no typed tool execution loop exists. `lifecycle: persistent` is declared but intentionally skipped by the controller.

## Product design

### Agent definition modes

`AgentTask` supports two definition modes:

1. **Profile mode**: existing `spec.type` plus `spec.prompt`.
2. **Inline mode**: new `spec.definition` containing a complete Agent.md document.

Exactly one mode is selected. In profile mode, `prompt` remains required. In inline mode, the Agent.md body is the system prompt, and `spec.prompt` is optional additional run input.

Inline example:

```yaml
apiVersion: agents.yurifrl.dev/v1alpha1
kind: AgentTask
metadata:
  name: crashloop-remediator
spec:
  owner: yuri
  definition: |
    ---
    name: crashloop-remediator
    runtime: adk
    model: claude-sonnet-4-5-20250929
    output: response
    lifecycle: persistent
    loop:
      interval: 30s
    tools:
      - name: http
        allowHosts:
          - alertmanager-operated.monitoring.svc
      - name: notify
        urlFromEnv: DISCORD_WEBHOOK_URL
    ---

    You are a Kubernetes incident-response agent. Watch Alertmanager and
    CrashLoopBackOff pods, diagnose the cause, perform only the permitted
    remediation, and report each action.
  skills:
    - name: kubernetes-diagnostics
    - selector:
        matchLabels:
          domain: kubernetes
  image: ghcr.io/yurifrl/agents-kubectl:dev
  serviceAccountName: crashloop-remediator
  secretName: agents
  env:
    - name: ALERTMANAGER_URL
      value: http://alertmanager-operated.monitoring.svc:9093
```

The controller parses `spec.definition` with the existing Agent.md parser. The definition's `name`, `runtime`, `model`, `output`, `lifecycle`, `loop`, and `tools` are authoritative in inline mode. Top-level execution fields remain responsible for pod placement and environment.

### AgentSkill CRD

`AgentSkill` is a reusable capability document:

```yaml
apiVersion: agents.yurifrl.dev/v1alpha1
kind: AgentSkill
metadata:
  name: kubernetes-diagnostics
  labels:
    domain: kubernetes
spec:
  description: Read-only Kubernetes diagnostics.
  tools:
    - name: exec
      allow:
        - [kubectl, get]
        - [kubectl, describe]
        - [kubectl, logs]
  instructions: |
    Prefer bounded reads. Include recent events, owner/controller status,
    restart history, and a bounded log tail in every diagnosis.
```

A skill has:

- `description`: short operator-facing purpose.
- `instructions`: markdown appended to the agent system prompt.
- `tools`: tool grants required by the skill.

Selection supports either one explicit name or one label selector per entry. Explicit skills are resolved in declaration order. Selector results are sorted by namespace and name. Duplicate selections are resolved once.

A missing explicit skill is a validation failure. An empty label selector result is valid and resolves to no skills.

### Skill composition

The controller resolves skills before creating or updating the workload.

The final system instruction is:

1. Inline Agent.md body or referenced profile instruction.
2. A deterministic `## Skill: <name>` section for each resolved skill.

Tool grants are the union of the agent definition and selected skills. Merging is deterministic:

- Allow rules and allowed hosts combine as a union.
- Duplicate rules are removed.
- Call limits use the strictest non-zero value.
- Timeouts use the shortest non-zero value.
- Cooldowns use the longest non-zero value.

The resolved definition, instruction, and tool policy are passed to the pod as environment-provided agent bundle data. Users do not create or mount profile ConfigMaps. A bundle hash is placed on the pod template; changing the definition or a resolved skill rolls persistent agents and leaves already-created ephemeral pods untouched.

`AgentTask.status` records the resolved skill names, bundle hash, and validation conditions.

### Tool engine

Add `internal/tool` with a typed registry:

- JSON-schema argument validation.
- Structured tool results.
- Per-tool allow policy, timeout, call limit, and cooldown.
- Secret redaction in tool output.
- A2A events for tool calls and tool results.

The initial built-in tools are generic:

- `exec`: executes an allowed argv vector without invoking a shell.
- `http`: performs bounded requests to explicitly allowed hosts.
- `notify`: posts text to a webhook URL named by `urlFromEnv`; the URL is never exposed to the model.
- `sleep`: waits for a bounded duration.

No `discord`, `alertmanager`, or `kubernetes` tool is required in the core engine. The example composes those behaviors from `exec`, `http`, `notify`, RBAC, and prompts/skills.

### Native model tool loop

Extend the Anthropic runtime from one completion to a bounded native tool-use loop:

1. Send the model the system instruction, run input, and tool schemas.
2. Execute each valid `tool_use` block through the registry.
3. Return structured `tool_result` blocks.
4. Repeat until final text, cancellation, error, or the configured turn budget.

Unknown tools, invalid arguments, policy denials, and tool failures become structured model-visible errors and A2A events.

### Loop agents

Agent.md gains:

```yaml
loop:
  interval: 30s
```

A loop agent performs repeated, bounded agent runs. Each iteration receives a fresh model context plus the prior iteration's compact final report. This avoids an unbounded conversation while retaining continuity.

Loop agents self-start when their pod starts. The pod still exposes health and A2A status. This matters after Deployment restarts: the watch resumes without requiring the gateway to redeliver an invoke request.

### Persistent lifecycle

`lifecycle: persistent` is implemented as:

- one Deployment per `AgentTask`;
- one stable Service per `AgentTask`;
- owner references for garbage collection;
- Deployment availability mapped to `AgentTask.status`;
- bundle-hash annotations for rollouts on definition or skill changes.

There is no shared persistent Deployment. Ownership and lifecycle stay obvious: one `AgentTask` owns one workload.

Ephemeral behavior remains pod-per-task through the existing gateway invocation flow.

### Execution fields

`AgentTask` gains the pod configuration required by real agents:

- `image`: overrides the configured default agent image.
- `serviceAccountName`: sets the pod ServiceAccount.
- `secretName`: overrides the configured default Secret used with `envFrom`.
- `env`: adds ordinary Kubernetes environment variables.

These fields are product features, not example-specific hooks.

### Gateway API

The gateway create API gains an inline-definition mode mirroring the CRD:

```json
{
  "owner": "yuri",
  "definition": "---\nname: ...",
  "skills": [{"name": "kubernetes-diagnostics"}],
  "image": "ghcr.io/yurifrl/agents-kubectl:dev",
  "serviceAccountName": "crashloop-remediator"
}
```

Profile mode remains:

```json
{
  "owner": "yuri",
  "type": "worker",
  "prompt": "Implement the requested change",
  "repo": "owner/repo",
  "output": "pr"
}
```

The gateway validates the same mode rules before creating the CRD.

## Example design

`examples/kubernetes-crashloop-remediator/deploy.yaml` contains only Kubernetes YAML:

- `crashloop-remediator` ServiceAccount.
- RBAC permitting bounded pod/event/log reads and pod deletion.
- ExternalSecret wiring for the existing `agents` 1Password item, including `DISCORD_WEBHOOK_URL`.
- `kubernetes-diagnostics` skill.
- `kubernetes-pod-restart` skill.
- One inline-definition `AgentTask`.

The agent image is a product-supported image variant containing `kubectl`.

On each loop iteration the agent:

1. Queries Alertmanager through the allowed `http` tool.
2. Queries Kubernetes for `CrashLoopBackOff` pods through allowed `kubectl` reads.
3. Gathers bounded events, owner/controller status, restart history, pod summary, and log tail.
4. Deletes one unhealthy controller-managed pod only when policy permits it.
5. Reports the diagnosis and action through `notify`.
6. Waits for the next iteration.

Discord is deployment configuration, not product coupling. The model sees only the `notify` tool and never the webhook URL.

## Safety boundaries

- Inline mode requires a parseable Agent.md document.
- `exec` accepts argv, never a shell command string.
- The example permits `kubectl delete` only for `pod`.
- One pod deletion is permitted per loop iteration.
- A tool cooldown prevents repeated deletion of the same failing pod.
- HTTP requests are limited to declared hosts.
- Secret values are redacted from tool results and events.
- Tool calls and results are streamed as A2A events.
- Missing explicit skills and invalid definitions fail validation before workload creation.
- Definition or skill changes roll persistent agents through a bundle hash.

## API compatibility

The API is `v1alpha1` and the product is still MVP, but existing profile mode remains supported:

- `type` + `prompt` continues to select installed profiles.
- `definition` selects inline mode.
- `runtime`, `output`, and `lifecycle` are read from the definition in inline mode.
- Existing top-level fields remain in profile mode rather than maintaining duplicate sources of truth in inline mode.

Validation enforces exactly one definition mode.

## Testing strategy

Tests are written before implementation.

### Agent definition and API

- Agent.md parsing and validation.
- Profile mode versus inline mode exclusivity.
- Inline definitions overriding runtime, output, lifecycle, and loop behavior.
- Gateway request validation for both modes.

### Skills

- Explicit selection order.
- Label selector ordering.
- Deduplication.
- Missing explicit skill failure.
- Empty selector success.
- Deterministic instruction composition.
- Tool-grant merge rules.
- Skill updates changing the bundle hash.

### Tool engine

- Schema validation.
- Unknown tools.
- Policy denial.
- Timeout, call limit, and cooldown behavior.
- Secret redaction.
- `exec` argv execution without shell interpretation.
- `http` host allowlists and response limits.
- `notify` against an `httptest` server.
- `sleep` cancellation.

### Model and runner

- Fake Anthropic server covering `tool_use` and `tool_result`.
- Multi-turn tool loop.
- Invalid tool arguments returned as model-visible errors.
- Turn budget and cancellation.
- Tool events emitted over A2A.
- Loop runner resets context and preserves only the compact prior report.

### Controller

Use envtest-style integration tests for:

- Inline-definition Deployment creation.
- ServiceAccount, image, Secret, env, and bundle propagation.
- Persistent Deployment status mapping.
- Definition rollout on bundle-hash change.
- Skill watch/reconcile behavior.
- Existing ephemeral profile mode.

### Example

- CRD/schema validation for `deploy.yaml`.
- RBAC covers exactly the commands granted by the skills.
- A mocked end-to-end runner test uses fake Anthropic, Alertmanager, and Discord endpoints.

## Documentation

After implementation:

- Update `docs/architecture.md` with definition modes, skills, tool loop, loop agents, and persistent lifecycle.
- Update `docs/status.md` with completed and remaining work.
- Add a concise example README with setup and operation.
- Leave the top-level README unchanged unless the implemented architecture makes its current MVP summary materially false.

## Acceptance criteria

- An operator can apply one YAML file under `examples/kubernetes-crashloop-remediator/` and run the remediation agent without creating a ConfigMap.
- The example uses only product CRDs and product tooling.
- An inline Agent.md definition is deployed directly from `AgentTask.spec.definition`.
- Skills can be attached by name and by label selector.
- Skill instructions and tool grants affect the running agent deterministically.
- Persistent agents run as Deployments and resume their loop after pod replacement.
- The Anthropic runtime executes typed tools through a native tool-use loop.
- Discord notifications are sent through a generic `notify` tool configured by `DISCORD_WEBHOOK_URL`.
- All new behavior is covered by tests.
