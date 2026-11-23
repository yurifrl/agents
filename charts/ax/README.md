# ax

![Version: 0.3.0](https://img.shields.io/badge/Version-0.3.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.4.0-rc.1](https://img.shields.io/badge/AppVersion-v0.4.0--rc.1-informational?style=flat-square)

Deploys google/ax v0.4.0-rc.1: ax-server (task executor, Gateway API and runner endpoint) and its PostgreSQL store, into the Helm release namespace. ax-server talks to the agent-substrate v1.0.x control plane (api.ate-system.svc, idp.ate-system.svc, atenet-router/egress), so this chart must deploy after agent-substrate.

**Homepage:** <https://github.com/yurifrl/agents>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| yurifrlcw |  |  |

## Source Code

* <https://github.com/yurifrl/agents/tree/main/k8s/charts/ax>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| ax.version | string | `"v0.4.0-rc.1"` | The ax build version (google/ax git ref the images were built from). |
| externalSecret | object | `{"enabled":true,"item":"litellm","name":"ax-gemini-api-secret","namespace":"default","property":"GEMINI_API_KEY","refreshInterval":"1h","secretStore":{"kind":"ClusterSecretStore","name":"onepassword"},"target":{"creationPolicy":"Owner","deletionPolicy":"Retain","name":"gemini-api-secret"}}` | Model-provider API key, provisioned via External Secrets Operator from 1Password. ax resolves the K8s Secret `gemini-api-secret` (fixed ax contract, internal/model DefaultSecretName) / key `GEMINI_API_KEY` in the task atespace's Kubernetes namespace — NOT the release namespace. The 1Password item must contain a field named GEMINI_API_KEY; by repo convention the item name equals the ExternalSecret name (see obs/, hermes/). |
| externalSecret.item | string | `"litellm"` | Upstream 1Password item + field holding the model-provider key. Defaults to the shared `litellm` item (the same item litellm proxies Gemini from) so the key is stored exactly once. |
| externalSecret.name | string | `"ax-gemini-api-secret"` | ExternalSecret name. |
| externalSecret.namespace | string | `"default"` | Namespace of the materialized K8s Secret: the atespace namespace ax-server reads secrets from (the substrate `default` atespace maps to the Kubernetes `default` namespace here). |
| externalSecret.refreshInterval | string | `"1h"` | ESO refresh interval for the upstream item. |
| externalSecret.secretStore.kind | string | `"ClusterSecretStore"` | ESO store holding the 1Password provider; ClusterSecretStore in the ESO namespace is the repo-wide default. |
| externalSecret.target.name | string | `"gemini-api-secret"` | K8s Secret name ax reads. Fixed by ax's secret contract; change only together with an ax version that renames it. |
| imagePullSecrets | list | `[]` | imagePullSecrets attached to every ax pod spec (kind profile sets [{name: ax-registry}]). |
| images.postgres | object | `{"repository":"docker.io/library/postgres","tag":"17-alpine@sha256:f9cf3e1f0066f0eb0c48707d864f0ac5941e9ff454a072077d97a4d278d07d73"}` | ax-postgres image (upstream deploy/postgres.yaml uses postgres:17-alpine), digest-pinned. |
| images.runnerVolume | string | `""` | Optional digest-pinned runner volume image (AX_RUNNER_VOLUME_IMAGE, Dockerfile.runner-volume) mounted read-only into task containers. Empty runs the runner baked into each task image. |
| images.server | object | `{"repository":"ghcr.io/yurifrl/ax/ax-server","tag":"v0.4.0-rc.1"}` | ko-built ax-server, published to public ghcr by the build-images workflow (upstream publishes no ax images; deploy/ax-server.yaml takes ${AX_SERVER_IMAGE}). Pin the digest the workflow prints once it ran. |
| images.taskRunner | object | `{"repository":"ghcr.io/yurifrl/ax/ax-task-runner","tag":"v0.4.0-rc.1"}` | Default task image (AX_DEFAULT_TASK_IMAGE) for Tasks whose spec.profile names no image: upstream Dockerfile.task-runner (python slim + git/curl/ssh + /usr/local/bin/ax-task-runner), built by the same workflow. Empty repository falls back to ax's compiled-in default (gcr.io/ax-substrate/ate-images/ax-task-runner). |
| networkPolicy.enabled | bool | `true` | Render upstream's NetworkPolicies: ax-server :8080/:9090 closed to worker pods and atenet-egress, :8081 open only to atenet-egress; ax-postgres open only to ax-server. |
| networkPolicy.extraIngressPorts | list | `[]` | Extra TCP ports admitted from any non-worker pod on ax-server. Istio ambient delivers meshed traffic (e.g. the ingress gateway) on the HBONE port 15008, which a port-scoped NetworkPolicy would otherwise drop. |
| otel.env | object | `{"OTEL_METRICS_EXPORTER":"prometheus","OTEL_TRACES_EXPORTER":"none"}` | ax-otel-config ConfigMap (envFrom). Upstream default: no OTLP export, metrics on /metrics (:9090), events on stdout. |
| postgres.password.generate | bool | `true` | Generate the ax-postgres password Secret (key: password) once with the External Secrets Operator Password generator (refreshInterval 0: never rotated, since postgres only reads it at initdb). Set false and create Secret ax-postgres yourself where ESO is absent. |
| postgres.resources.limits.cpu | string | `"1"` |  |
| postgres.resources.limits.memory | string | `"1Gi"` |  |
| postgres.resources.requests.cpu | string | `"100m"` |  |
| postgres.resources.requests.memory | string | `"256Mi"` |  |
| postgres.storage | string | `"10Gi"` | PVC size for ax-postgres (upstream: 10Gi). |
| postgres.storageClassName | string | `""` | StorageClass for the PVC; empty uses the cluster default. |
| registry | object | `{"password":"","server":"ghcr.io","username":"yurifrl"}` | ghcr.io/yurifrl/ax/* is public: no pull Secret, no imagePullSecrets. registry.* only builds one when a private registry needs it. |
| replicas.server | int | `1` |  |
| runner.endpoint | string | `""` | In-cluster host:port of ax-server's :8081 runner service, written into every task actor's EgressPolicy (AX_RUNNER_ENDPOINT). Empty uses ax's default, ax-server-runner.ax-system.svc.cluster.local:8081 — set it when the release namespace is not ax-system. |
| substrate.actorJwtIssuer | string | `"https://idp.ate-system.svc"` | OIDC issuer of the Actor JWTs the egress gateway injects on runner calls to :8081 (AX_ACTOR_JWT_ISSUER); must equal agent-substrate's ateapi.actorJwtIssuer. |
| substrate.atenetRouterAddr | string | `"https://atenet-router.ate-system.svc.cluster.local:443"` | atenet-router mTLS ingress ax-server reaches task actors through (ATENET_ROUTER_ADDR). ax-server presents its podidentity certificate; agent-substrate's atenetRouter.ingressAllowedSpiffeIds must list spiffe://cluster.local/ns/<release-namespace>/sa/ax-server. |
| substrate.authority | string | `"api.ate-system.svc"` | TLS server name (SNI/hostname verification) and the ServiceAccount token audience for ax-server's substrate connection. |
| substrate.caFile | string | `"/run/servicedns-ca/trust-bundle.pem"` | Path of the cluster trust bundle mounted for verifying the substrate control plane (and the ate-idp-server issuer). Served by agent-substrate's podcert servicedns signer. |
| substrate.endpoint | string | `"api.ate-system.svc.cluster.local:443"` | Control-plane endpoint ax-server connects to (gRPC over TLS). |
| template.atespace | string | `"default"` | Substrate atespace holding task ActorTemplates (a substrate concept, unrelated to the Kubernetes release namespace). Tasks run in the `default` atespace; ax-server reads its model-key Secret from the Kubernetes namespace matching this atespace. |
| template.name | string | `"default-template"` | ActorTemplate ax-server fetches from substrate for each task. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
