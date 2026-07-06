# Helm Validation (Goal Z1)

Recorded 2026-07-06, helm v4.2.2.

## helm lint agentatlas

```
[INFO] Chart.yaml: icon is recommended

1 chart(s) linted, 0 chart(s) failed
```

## helm template atlas-test agentatlas

Renders 7 manifests cleanly (4 Deployments + Services for atlas-api,
atlas-agent, atlas-worker, parser-gateway per templates/, values from
values.yaml). All four service templates carry readiness + liveness probes:

- atlas-api / atlas-agent / parser-gateway: `httpGet /healthz` on the service port.
- atlas-worker: `httpGet /metrics` on the 9091 metrics listener (no API port).

Re-run:

```powershell
cd services/agentatlas/deploy/helm
helm lint agentatlas
helm template atlas-test agentatlas
```
