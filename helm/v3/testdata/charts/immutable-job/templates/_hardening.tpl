{{/* Restricted pod and container security settings, shared by both workloads so the only
difference between two renders of the Job is its command (core-provider#403). */}}
{{- define "probe.podSecurity" -}}
securityContext:
  runAsNonRoot: true
  runAsUser: 10001
  runAsGroup: 10001
  seccompProfile:
    type: RuntimeDefault
{{- end -}}
{{- define "probe.containerSecurity" -}}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: 10001
  runAsGroup: 10001
  capabilities:
    drop: ["ALL"]
  seccompProfile:
    type: RuntimeDefault
resources:
  requests:
    cpu: 10m
    memory: 16Mi
  limits:
    cpu: 50m
    memory: 32Mi
{{- end -}}
