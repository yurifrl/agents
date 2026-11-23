# agent-substrate

![Version: 0.3.0](https://img.shields.io/badge/Version-0.3.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v1.0.1](https://img.shields.io/badge/AppVersion-v1.0.1-informational?style=flat-square)

Deploys Agent Substrate v1.0.1 (ate-system): ate-api-server, ate-idp-server, ate-controller, atelet DaemonSet, atenet-router/egress on agentgateway, podcertificate-controller, the Kubernetes Secrets credential provider, CRDs, SandboxConfigs and RBAC. Replaces `ate-setup deploy ate-system`.

**Homepage:** <https://github.com/agent-substrate/substrate>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| yurifrlcw |  |  |

## Source Code

* <https://github.com/nsxbet/helm-charts/tree/main/charts/agent-substrate>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| ateapi.actorJwtIssuer | string | `"https://idp.ate-system.svc"` | iss claim of minted actor JWTs (ATE_API_ACTOR_JWT_ISSUER), also passed to ate-idp-server as --issuer. The default is the in-cluster ate-idp-server Service (templates/ate-idp-server.yaml), which relying parties such as ax-server's runner endpoint use for OIDC discovery. |
| ateapi.authentication.inClusterDiscovery | bool | `false` | Inject the pod's own projected service-account CA+token into the OIDC discovery client even for a non-default issuer, when that issuer is kube-apiserver itself but not publicly resolvable (talos, self-hosted). |
| ateapi.authentication.issuer | string | `"https://kubernetes.default.svc"` | JWT issuer the api-server trusts for Kubernetes service-account tokens (ate-api-authentication ConfigMap). In-cluster default; on GKE set https://container.googleapis.com/v1/projects/<p>/locations/<l>/clusters/<c>. |
| ateapi.authz.bootstrapOwners | list | `["system:serviceaccount:ate-system:ate-client"]` | Principals that are always global owners (--authz-bootstrap-owners, required since v1.0: ate-api-server exits without at least one). Upstream's default is the shared ate-client ServiceAccount. Clients that create atespaces/templates/actors (e.g. ax-server) must be listed or be granted an AccessPolicy by an owner. |
| ateapi.hostAliases | list | `[]` | hostAliases entries for the ate-api-server pod, needed when the trusted issuer hostname only resolves from outside the cluster (e.g. a LAN DNS name like api.k8s.lan that cluster CoreDNS upstreams don't serve). |
| ateapi.logLevel | string | `"info"` | apiconfig.textproto logging level (ate-api-config ConfigMap; ate-setup `create api-config`). |
| ateapi.replicas | int | `2` | ate-api-server replica count. |
| ateapi.snapshotLocation | string | `""` | snapshot-location). REQUIRED since v1.0 unless the PerTemplateSnapshotLocation preview gate is on; render fails when empty. On s3/rustfs use s3://<bucket>/[prefix]; snapshots land under <location>/atespaces/. |
| ateapi.transferServiceAccount | string | `""` | GCS-only: the transfer service account the control object-store plugin's credential exchange impersonates. Leave empty on s3 (the S3 exchange is unimplemented and never called). |
| atelet.extraArgs | list | `[]` | localhost-registry-replacement=kind-registry:5000. |
| atelet.gcpAuthForImagePulls | bool | `false` | Pass GCP credentials for pulling actor images from GCR/Artifact Registry. MUST be false here: the workload/actor images are public ghcr, and atelet's in-process go-containerregistry pulls are anonymous for non-GCP registries. |
| atenetRouter.ingressAllowedSpiffeIds | list | `[]` | SPIFFE IDs allowed through the router's mTLS ingress (agentgateway networkAuthorization rule; replaces ate-setup's [ATE_INGRESS_ALLOWED_SPIFFE_IDS] marker). Empty denies every client on the https/connect-tls gateways. ax-server is spiffe://cluster.local/ns/<ax-namespace>/sa/ax-server. |
| crds.install | bool | `true` |  |
| credentialProvider.enabled | bool | `false` | Deploy k8s-credential-provider and point atenet-egress at it. |
| credentialProvider.namespacePolicies | list | `[]` | namespace-policy.yaml policies (default-deny when empty): which Kubernetes namespaces each atespace may read Secrets from, optionally narrowed by a label selector. |
| idpServer.enabled | bool | `true` | Deploy ate-idp-server + Service idp. ax-server verifies runner Actor JWTs against it, so keep it on whenever ax runs. |
| idpServer.replicas | int | `2` | Replica count (upstream: 2). |
| imagePullSecrets | list | `[]` | ghcr.io/yurifrl/ate/* is public: no pull Secret, no imagePullSecrets. registry.* only builds one when a private registry needs it. |
| images.agentgateway | object | `{"repository":"cr.agentgateway.dev/agentgateway","tag":"v1.7.0@sha256:60216aafb03d37f94e952ff3d51d2f630a65b10ecaebdc0a864882f92610c45b"}` | atenet-router runs the agentgateway image; there is no separate atenet server image in the agentgateway dataplane. |
| images.agentgateway.repository | string | `"cr.agentgateway.dev/agentgateway"` | Official agentgateway image, digest-pinned exactly as substrate v1.0.1's components/agentgateway overlay pins it (atenet-router and atenet-egress both run it). |
| images.agentgatewayEgress | object | `{"repository":"cr.agentgateway.dev/agentgateway","tag":"v1.7.0@sha256:60216aafb03d37f94e952ff3d51d2f630a65b10ecaebdc0a864882f92610c45b"}` | atenet-egress runs the same digest-pinned agentgateway image; its internal dynamicCa bind mints MITM leaves signed by the egress-mitm-ca-pool secret. |
| images.ateapi.repository | string | `"ghcr.io/agent-substrate/substrate/ateapi"` | Upstream release images (ghcr.io/agent-substrate/substrate/<name>), published multi-arch (linux/amd64 + linux/arm64) by the v1.0.1 release workflow and digest-pinned to the index. Since v1.0 upstream publishes its own images, so nothing in this chart is self-built any more. |
| images.ateapi.tag | string | `"v1.0.1@sha256:5d9f6da2e1446766a48908764891cc3c66cc6ff657279415c0d3839d5e9476ee"` |  |
| images.atecontroller.repository | string | `"ghcr.io/agent-substrate/substrate/atecontroller"` |  |
| images.atecontroller.tag | string | `"v1.0.1@sha256:b63519a821f4c702a689caf5b1ff549a8d6134ac3da94def30c71f914a70d614"` |  |
| images.atelet.repository | string | `"ghcr.io/agent-substrate/substrate/atelet"` |  |
| images.atelet.tag | string | `"v1.0.1@sha256:f6707957b470d8a8529e65a4ec3eb1ad5182a607b313b6b1628c7fe6c0e5a9e6"` |  |
| images.ateworkerGvisor.repository | string | `"ghcr.io/agent-substrate/substrate/ateworker-gvisor"` | Worker image the gvisor WorkerPool runs (workerpool-gvisor.yaml). v1.0 renamed cmd/ateom-gvisor to cmd/ateworker-gvisor. |
| images.ateworkerGvisor.tag | string | `"v1.0.1@sha256:fd0c6a46d6c69d485856171e632906e774594bea9d00011a4d7038be3e084a2a"` |  |
| images.awscli.repository | string | `"docker.io/amazon/aws-cli"` | Bucket-bootstrap Job image (rustfs.yaml): official upstream image, digest-pinned. |
| images.awscli.tag | string | `"2.38.5@sha256:c8156a0e74e13929a42d7e60f0d4e14e7d28d980cd21822505d4bff80e5460c1"` |  |
| images.credentialProvider.repository | string | `"ghcr.io/agent-substrate/substrate/kubernetes-secrets"` | Bundled Kubernetes Secrets credential provider (credentialProvider.enabled): resolves ate-secret://k8s.io/... URIs in EgressPolicy header effects for the egress gateway. |
| images.credentialProvider.tag | string | `"v1.0.1@sha256:46a2872c624c6e3af98984f1097d2205f012ea6c471516ac5189ceb3adef96e5"` |  |
| images.idpServer.repository | string | `"ghcr.io/agent-substrate/substrate/ate-idp-server"` | ate-idp-server (new in v1.0): serves the actor-JWT OIDC discovery document and JWKS at https://idp.ate-system.svc from the same actor-id-jwt-pool ate-api-server signs with. |
| images.idpServer.tag | string | `"v1.0.1@sha256:5d9302255998a3f376cbea8d8860488d20b6c030417c0c3ea7fba53a67f2c9f7"` |  |
| images.jaeger.repository | string | `"docker.io/jaegertracing/all-in-one"` | Jaeger all-in-one trace store + UI (otel.collector.enabled): official upstream image, digest-pinned. |
| images.jaeger.tag | string | `"1.55@sha256:f6b5d09073f14f76873d300f565a6691d815e81bea8e07e1dc3ff67e0596dd4e"` |  |
| images.objectStorePlugin.repository | string | `"ghcr.io/agent-substrate/substrate/objectstore-plugin"` | object-store plugin: control sidecar next to ate-api-server, node sidecar next to atelet. |
| images.objectStorePlugin.tag | string | `"v1.0.1@sha256:311cc9cfb9ee8c344f3ebc3e4d5cafb88468840023b62d1c47c6338ed32b0352"` |  |
| images.otelcollector.repository | string | `"docker.io/otel/opentelemetry-collector-contrib"` | In-cluster OTLP collector (otel.collector.enabled): official upstream image, digest-pinned (kind/otel-collector.yaml). |
| images.otelcollector.tag | string | `"0.157.0@sha256:f2f01157055a9b2aab9df7118e1f1c9abf345e99b23bc7a2bc791db374a7d0f6"` |  |
| images.podcert.repository | string | `"ghcr.io/agent-substrate/substrate/podcertcontroller"` | podcertificate-controller (publishes the podidentity/servicedns/postgres ClusterTrustBundles the projected volumes use). |
| images.podcert.tag | string | `"v1.0.1@sha256:46aa01a624053c23080da41ea37937afc5b544cecf77088ab2196b4ffb734b4f"` |  |
| images.postgres.repository | string | `"docker.io/library/postgres"` | Bundled PostgreSQL (see postgres.bundled): official upstream image, digest-pinned (multi-arch: amd64 + arm64). |
| images.postgres.tag | string | `"18-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15"` |  |
| images.rustfs.repository | string | `"docker.io/rustfs/rustfs"` | S3-compatible object store for local clusters (storage.rustfs.enabled): official upstream image, digest-pinned. |
| images.rustfs.tag | string | `"1.0.3@sha256:9ca9559533b537d10482fe923288b676ad08f92a7069d9a17327fbae3cab688b"` |  |
| otel.collector | object | `{"enabled":false}` | Deploy the in-cluster collector + jaeger (kind profile, see templates/otel-collector.yaml). EKS keeps false: the pre-existing observability-namespace collector receives exports. |
| otel.exporterOtlpAddress | string | `"http://opentelemetry-collector.observability.svc.cluster.local:4317"` | OTLP exporter address used by atenet-router directly. |
| otel.exporterOtlpEndpoint | string | `"http://opentelemetry-collector.observability.svc.cluster.local:4317"` | OTLP endpoint for ate-otel-config (ate-api-server, ate-controller, atelet). Point at the cluster collector; empty string disables export only if the collector isn't deployed. |
| otel.extraEnv | object | `{}` | Additional data keys merged into the ate-otel-config ConfigMap. The kind profile sets OTEL_METRIC_EXPORT_INTERVAL, OTEL_METRIC_EXPORT_TIMEOUT (both 10000) and OTEL_LOGS_EXPORTER=otlp, mirroring the upstream overlay. |
| podMonitoring.enabled | bool | `false` | Deploy PodMonitoring CR (Google-managed K8s operator CRD) for atenet-router /stats/prometheus scraping. Enable only if the gke-managed-prometheus operator runs on the cluster. |
| postgres.bundled | bool | `true` | Deploy the bundled PostgreSQL StatefulSet. ate-setup's default deploy does the same for ate-system; set false when pointing the DSN at RDS or an external server. |
| postgres.connectionString | string | `""` | Full DSN override; empty uses the bundled server's default. Rendered into the ate-api-server-secret-envvars Secret (optional envFrom source). |
| postgres.nodeSelector | object | `{}` | Pod nodeSelector override. Upstream postgres is multi-arch; default `{}` lets it schedule on any untainted node (amd64-only pin can strand it when ko-built workloads fill the only amd64 node). |
| postgres.ownerRole | string | `"substrate_owner"` |  |
| postgres.readWriteRole | string | `"substrate_readwrite"` | Roles ATE_API_POSTGRES_READ_WRITE_ROLE / ATE_API_POSTGRES_OWNER_ROLE assert. Must match the identities setup.sql creates (fixed constants at postgressetup.ReadWriteRole/OwnerRole). |
| postgres.resources | object | `{"limits":{"cpu":"16","memory":"2Gi"},"requests":{"cpu":"2","memory":"1Gi"}}` | Upstream sizing sustains control-plane activation load. Local clusters override (kind overlay: 250m/1Gi requests, 2cpu limits). |
| postgres.schema | string | `"substrate"` | ate-api-server rejects an empty schema. official postgressetup creates the substrate schema; only change when the setup.sql in postgres.yaml was edited to match. |
| postgres.storage | string | `"500Gi"` | Headroom so WAL churn cannot fill the volume (full pg_wal = PANIC). Substrate base is 500Gi; local clusters override to 1Gi. |
| registry.password | string | `""` | Token for a PRIVATE registry. Empty creates no Secret. |
| registry.server | string | `"ghcr.io"` | Only used to build the ate-registry pull Secret (see imagePullSecrets) when registry.password is set; never persisted. |
| registry.username | string | `"yurifrl"` |  |
| sandbox.gvisor.defaultVersion | string | `"v7"` | Version actors boot when their template names none. |
| sandbox.gvisor.enabled | bool | `true` | Apply the cluster-wide gvisor SandboxConfig (name: gvisor-default, cluster-scoped since v1.0). |
| sandbox.gvisor.versions | list | `[{"assets":{"amd64":{"gvisor":{"sha256":"708afc62f2ee82d19930c703e85f9dab4f27a78c0aec40c0f55eb1040107f1a7","url":"gs://gvisor/releases/release/20261026.0/x86_64/gvisor.tar.zstd"}},"arm64":{"gvisor":{"sha256":"6257e49955f13e6caceb2c4181c5dc5406c460e805940c944e67ad65553f5b02","url":"gs://gvisor/releases/release/20261026.0/aarch64/gvisor.tar.zstd"}}},"name":"v6","state":"Enabled"},{"assets":{"amd64":{"gvisor":{"sha256":"a1439ffbba7d212e0fde32ea79fa9bb51cbc4c5f6c16af1f047260290b861d42","url":"gs://gvisor/releases/release/20261102.0/x86_64/gvisor.tar.zstd"}},"arm64":{"gvisor":{"sha256":"f702ae92ad7a0c4134be34ca23f52966dbb696db0c0f5b8ec439210ab2cd7c6a","url":"gs://gvisor/releases/release/20261102.0/aarch64/gvisor.tar.zstd"}}},"name":"v7","state":"Enabled"}]` | Named runtime versions, verbatim from substrate v1.0.1 manifests/ate-install/sandboxconfig-gvisor.yaml (gVisor releases 20261026.0 and 20261102.0). v1.0 dropped pauseImage; the validating admission policy requires a 'gvisor' tarball asset per architecture. |
| sandbox.microvm.assets.amd64.cloud-hypervisor.sha256 | string | `"448af3d4e59b22c2987f7df94c213ad40fb53a10d437e42b5ee6c4fce7c29ecc"` |  |
| sandbox.microvm.assets.amd64.cloud-hypervisor.url | string | `"kata-assets/cloud-hypervisor"` |  |
| sandbox.microvm.assets.amd64.kata-image.sha256 | string | `"96497f64da1de9c7473fef46c3d29ddd0805d334731cf9d903a21a5b2c33cefb"` |  |
| sandbox.microvm.assets.amd64.kata-image.url | string | `"kata-assets/rootfs.img"` |  |
| sandbox.microvm.assets.amd64.kata-kernel.sha256 | string | `"8e9dbc3a6e4c26adb089d23d31201edebce905c7f3e31aa67b32bdd41df1b86f"` |  |
| sandbox.microvm.assets.amd64.kata-kernel.url | string | `"kata-assets/vmlinux"` |  |
| sandbox.microvm.assets.amd64.virtiofsd.sha256 | string | `"15b2e72a78cc08a9bd8a6943e89fb69c88cb3cbeb63069efceade835342ac7d4"` |  |
| sandbox.microvm.assets.amd64.virtiofsd.url | string | `"kata-assets/virtiofsd"` |  |
| sandbox.microvm.assets.arm64.cloud-hypervisor.sha256 | string | `"f192b510eea1c710cbc439d716bb0573c223fc463dbe3e6523788a2b7ef62850"` |  |
| sandbox.microvm.assets.arm64.cloud-hypervisor.url | string | `"kata-assets/cloud-hypervisor"` |  |
| sandbox.microvm.assets.arm64.kata-image.sha256 | string | `"7e3bdd500eb92de17c377bd84f8ed57e50d6bb68c7ec08c88f5e1916cd49f30d"` |  |
| sandbox.microvm.assets.arm64.kata-image.url | string | `"kata-assets/rootfs.img"` |  |
| sandbox.microvm.assets.arm64.kata-kernel.sha256 | string | `"4960f6cf781274eaa445fc9f88bb8c5ce951649278bc9caa13afb1e99cfe80e6"` |  |
| sandbox.microvm.assets.arm64.kata-kernel.url | string | `"kata-assets/vmlinux"` |  |
| sandbox.microvm.assets.arm64.virtiofsd.sha256 | string | `"3aa9240045d2261930ef04138fe05cf288f94c867e63e5d0845758bac5e3d458"` |  |
| sandbox.microvm.assets.arm64.virtiofsd.url | string | `"kata-assets/virtiofsd"` |  |
| sandbox.microvm.bucket | string | `"ate-cluster-bucket"` |  |
| sandbox.microvm.enabled | bool | `false` | Apply the microvm (cloud-hypervisor/kata) SandboxConfig. Opt-in: requires the kata asset set staged in the cluster bucket first. |
| storage.backend | string | `"s3"` | Object-store backend for Actor data/snapshots. On EKS: s3 (atelet + ate-api-server read AWS credentials from the pod/node IRSA). |
| storage.rustfs.accessKey | string | `"rustfsadmin"` |  |
| storage.rustfs.enabled | bool | `false` | Deploy the in-cluster S3-compatible store + inject static AWS credentials into ate-api-server/atelet. Local clusters only. |
| storage.rustfs.endpoint | string | `"http://rustfs.ate-system.svc:9000"` |  |
| storage.rustfs.nodeSelector | object | `{}` | Pod nodeSelector override; upstream rustfs is multi-arch. |
| storage.rustfs.region | string | `"us-east-1"` |  |
| storage.rustfs.secretKey | string | `"rustfsadmin"` |  |
| storage.rustfs.storage | string | `"1Gi"` |  |
| validatingAdmissionPolicy.enabled | bool | `true` |  |
| workers.gvisor.enabled | bool | `true` | Deploy the gvisor WorkerPool so actors have placement capacity. |
| workers.gvisor.replicas | int | `1` | Worker pod count. Each worker reports node capacity to the substrate scheduler. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
