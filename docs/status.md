# Status

What is actually built vs. pending. This is the truth about *current* state;
keep it honest, including known corner-cuts that still need fixing.

_Last reconciled: 2026-06-15._

## Built

- **`AgentTask` CRD** (`api/v1alpha1`) — spec: type, prompt, repo, owner,
  runtime, lifecycle, output; status: phase, podName, artifacts, restarts.
- **Controller (ephemeral)** — `AgentTask` → Pod (+Service), owner refs,
  `activeDeadlineSeconds`, configurable `imagePullSecrets`, pod-phase → status.
- **Agent entrypoint** — `agent-entrypoint`; `output: response` (Claude
  completion) and `output: pr|commit` (go-git clone/commit/push + GitHub API).
  Verified: a pod opened a real PR against `yurifrl/agents`.
- **Image** — `ghcr.io/yurifrl/agents`, distroless, multi-arch (amd64+arm64),
  built and pushed by CI with the workflow `GITHUB_TOKEN`.

## Pending

- **CRD install (declarative)** — the `AgentTask` CRD is still applied by hand.
  The agents repo is private, so an ArgoCD cross-repo source needs repo creds (or
  the repo going public) before the CRD install can move into the `agents` app.
- **1Password `agents` item** — the one documented manual prerequisite; the
  `agents` ExternalSecret (anthropic-api-key, github-token, git-email) is already
  declared in `home-systems`, but won't sync until the item exists.
- **Persistent lifecycle** — Deployment-backed warm agents (reconciler stubs
  only today).
- **Restart budget / max-concurrency** — config fields exist; not yet enforced.
- **ADK tool-use loop** — the entrypoint is a single completion / structured
  edit, not a real tool-using agent loop yet.
- **`Agent.md` parsing** — `--profile` is currently a name, not a parsed
  definition file.
- Gateway, pi extension, beads workstrator — not started in this repo.

## Known corner-cuts to repay (debt log)

- **Secrets — repaid in source.** The `agents` ExternalSecret and the ghcr pull
  secret are now declared in `home-systems` (`k8s/applications/agents.yaml`) via
  the support chart + 1Password. The live cluster still holds the hand-applied
  versions; they are superseded the moment ArgoCD syncs the app.
- **Namespace + CRD — still imperative.** Created by hand during the demo.
  Namespace is covered by `CreateNamespace=true`; the CRD install remains debt
  (see Pending) until the private-repo source access is resolved. Until both are
  declarative, the cluster is not fully reproducible from source.
