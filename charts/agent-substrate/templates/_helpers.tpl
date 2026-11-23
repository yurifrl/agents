{{/*
agent-substrate shared helpers.
The official install names the DaemonSet plain `atelet`; version stamps live
only on image tags.
*/}}

{{- define "agent-substrate.name" -}}
{{- "agent-substrate" -}}
{{- end -}}

{{/*
nodeSelector rendered into every pod template. The v1.0 upstream images are
multi-arch, but the substrate data plane (atelet + gvisor workers) runs only on
the amd64 ate.dev/substrate node, so control-plane pods default to
kubernetes.io/arch: amd64 to stay next to it. Override with `nodeSelector: {}`
to let them schedule anywhere.
Call as `include "agent-substrate.nodeSelector" (dict "root" $ "overrides" X)`
to use X instead of the default for upstream multi-arch images (postgres,
rustfs) that are safe on arm64.
*/}}
{{- define "agent-substrate.nodeSelector" -}}
{{- $sel := dict -}}
{{- if hasKey . "overrides" -}}
{{- $overrides := index . "overrides" -}}
{{- $root := index . "root" -}}
{{- if ne $overrides nil -}}
{{- $sel = $overrides -}}
{{- else if hasKey $root.Values "nodeSelector" -}}
{{- $sel = $root.Values.nodeSelector -}}
{{- else -}}
{{- $sel = dict "kubernetes.io/arch" "amd64" -}}
{{- end -}}
{{- else -}}
{{- if hasKey .Values "nodeSelector" -}}
{{- $sel = .Values.nodeSelector -}}
{{- else -}}
{{- $sel = dict "kubernetes.io/arch" "amd64" -}}
{{- end -}}
{{- end -}}
{{- if not (empty $sel) -}}
{{- toYaml $sel -}}
{{- end -}}
{{- end -}}
