# Conventions

The rules every change must satisfy. An agent auditing adherence checks against
these; a violation is "cutting corners" regardless of whether the code works.

## The litmus test

> Could the cluster be deleted and rebuilt from **git + 1Password alone**?

If the answer is no, the change is incomplete. Working is not done; reproducible
is done.

## Hard rules

1. **Declarative over imperative.** State is expressed in source (CRDs, charts,
   ArgoCD apps), not produced by commands. If you ran a `kubectl create`/`patch`
   to make something work, it must instead exist as a manifest in `home-systems`.
2. **Secrets through ESO.** 1Password item → `ExternalSecret` → Secret. Never
   `kubectl create secret`, never `--from-literal`, never a literal in a manifest.
3. **Package-oriented design, separation of concerns.** One responsibility per
   package (`config`, `logging`, `kube`, `controller/...`, `agent`, `output`).
   No grab-bag `util` packages.
4. **No corner-cutting shims.** No backwards-compat hacks, no dead code kept
   "just in case", no error handling for impossible states, no abstractions for
   hypothetical futures.
5. **Generated artifacts are generated.** `zz_generated.deepcopy.go`,
   `config/crd`, `config/rbac` come from `task generate`/`task manifests` — never
   hand-edited.
6. **`go run`, not `go build`, for anything but shipping.** Test binaries (if
   ever) go to `/tmp`, never the working tree.
7. **The image stays distroless.** New side effects use Go libraries
   (go-git, REST), not shelled-out binaries, unless the base image changes is a
   recorded decision.

## Stack (do not reinvent)

- CLI: `cobra` + `viper` (config resolves from `AGENTS_*` env **and** file).
- Logging: `charmbracelet/log` via a `logr` adapter.
- Controller: `sigs.k8s.io/controller-runtime` + `controller-gen`.
- No DI framework (see architecture D4).

## Definition of done

- Code compiles (`task build`) and `task vet` is clean.
- Generated artifacts regenerated and committed.
- **`docs/` updated** to match the change (architecture/decisions/status).
- Deployment changes are declarative in `home-systems`, not hand-applied.
