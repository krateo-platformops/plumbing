# plumbing

The shared Go library of the Krateo platform — reusable building blocks and the cross-repo contracts (helm reconcile engine, crdgen, composition labels) consumed by core-provider, composition-dynamic-controller and the platform services.

[![test](https://github.com/krateo-platformops/plumbing/actions/workflows/test.yaml/badge.svg)](https://github.com/krateo-platformops/plumbing/actions/workflows/test.yaml)
[![Reference](https://pkg.go.dev/badge/github.com/krateo-platformops/plumbing)](https://pkg.go.dev/github.com/krateo-platformops/plumbing)

## What is this

A flat collection of independent Go packages with one module identity. It carries
the Helm client with the fork-free apply-if-changed `Reconcile` engine that
composition-dynamic-controller runs on, the `crdgen` JSON-Schema → CRD transpiler
core-provider uses, the `labels` package that pins the core-provider ↔ CDC label
contract at compile time, plus `kubeutil/*`, `jwtutil` and small utilities.
Library only: no binary, no image, no chart. Full picture:
[docs/index.md](docs/index.md).

## Install

```sh
go get github.com/krateo-platformops/plumbing@v1.13.2
```

## Configure

There is almost nothing to configure — see
[docs/configuration.md](docs/configuration.md). Most used:

| Setting | Default | Effect |
|---|---|---|
| `HELM_DRIVER` | `secret` | Helm release-storage driver used by `helm/v3`. |
| `CLUSTER_NAME` | unset | Overrides `kubeutil.DetectClusterName`. |
| build tag `envtest` | off | Selects the real-apiserver functional tests (`make test-envtest`). |

## Examples

- [examples/minimal](examples/minimal) — `values.schema.json` → CRD via
  `crdgen.Generate`; `go run ./examples/minimal`.

## Commands

- [cmd/values-schema](cmd/values-schema) — infer a chart's `values.schema.json`, or scaffold a
  wrapper chart, with `valuesschema`;
  `go install github.com/krateo-platformops/plumbing/cmd/values-schema@<version>`.

## Docs

- [docs/index.md](docs/index.md) — the map
- [docs/overview.md](docs/overview.md) — package design + the Reconcile engine
- [docs/usage.md](docs/usage.md) — go get + version-pinning conventions
- [docs/configuration.md](docs/configuration.md) — the whole config surface
- [docs/api.md](docs/api.md) — the exported package map
- [docs/examples.md](docs/examples.md) — examples index
- [docs/release.md](docs/release.md) — how a release ships (tag-only)
- [docs/log.md](docs/log.md) — curated history

## Develop & release

`go build ./... && make test-race` (CI adds `make test-envtest`). Releases are
plain `vX.Y.Z` git tags — see [docs/release.md](docs/release.md).
