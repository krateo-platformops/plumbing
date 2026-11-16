# Vendored test inputs (plumbing#96)

The helm integration suite installs these instead of fetching them live, so its result does not depend
on GitHub, Docker Hub or any chart repository being up. Each test serves them from a server inside the
test (an HTTP chart repository, a pull-only OCI registry, or a plain `.tgz` URL; see `vendored_test.go`).

Every file is pinned in the `SHA256SUMS` beside it, and `verifyVendored` refuses to serve a file whose
bytes differ or that is not pinned. `TestLiveFetch_VendoredChartsMatchUpstream` is the only test that
reaches the internet: it fetches each one from the source below and requires the same bytes.

Fetched 2026-11-16.

| file | source | note |
|---|---|---|
| `charts/nginx-25.2.14.tgz` | `oci://registry-1.docker.io/bitnamicharts/nginx` | the tests used the unpinned latest; 25.2.14 was latest when vendored |
| `charts/ingress-nginx-4.10.0.tgz` | `https://kubernetes.github.io/ingress-nginx` | byte-identical to the GitHub release asset `helm-chart-4.10.0/ingress-nginx-4.10.0.tgz` |
| `charts/redis-operator-0.27.0.tgz` | `https://ot-container-kit.github.io/helm-charts` | the tests used the unpinned latest; now pinned |
| `charts/cloudnative-pg-0.27.1.tgz` | `https://cloudnative-pg.github.io/charts` | |
| `charts/cluster-0.5.0.tgz` | `https://cloudnative-pg.github.io/charts` | |
| `charts/snowplow-0.20.6.tgz` | `https://charts.krateo.io` | |
| `charts/full-stack-app-0.0.15.tgz` | `https://marketplace.krateo.io` | dependencies bundled in the archive |
| `frontend-crds/*.yaml` | `https://github.com/krateoplatformops/frontend-chart`, `crd-chart/templates` at `e83c0ed5750548de41ab7550c34a3799a8cc43d5` | was a live `git clone --depth 1` of HEAD |

To re-vendor: fetch the new version from the same source, replace the file, update `SHA256SUMS`, this
table, and the version the tests and `TestLiveFetch_VendoredChartsMatchUpstream` pin. Do it as its own
change, so a chart upgrade is never a side effect of a test fix.

Not covered: the cluster still pulls container images from their registries.
