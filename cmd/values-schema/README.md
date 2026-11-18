# values-schema

Infers a `values.schema.json` for a Helm chart that ships none, using plumbing's
[`valuesschema`](../../valuesschema) library (core-provider#568, child 4). A person runs it,
reviews the output and commits it. It is the offline half of the design; core-provider's opt-in
in-cluster inference uses the same library.

It versions with plumbing, so the schema it writes always carries the `RulesVersion`
(`x-krateo-values-schema.rules`) of the library it was built with.

```sh
go install github.com/krateo-platformops/plumbing/cmd/values-schema@<plumbing version>
```

## Infer a chart's schema

```sh
values-schema infer ./mychart --out ./mychart/values.schema.json --report report.json
values-schema infer mychart-1.2.3.tgz
values-schema infer oci://ghcr.io/kyverno/charts/kyverno --version 3.9.2
```

The source is a chart directory, a `.tgz` archive, or an OCI reference with `--version`.

- **Directory dependencies:** they must be vendored under `charts/` first, with
  `helm dependency build`.
- **Directory's own schema:** a directory's own `values.schema.json` is ignored, so an existing
  wrapper is regenerated in place.
- **OCI pulls:** they use helm's registry client and credentials, as `helm registry login` sets
  them.

The schema goes to `--out`, or to stdout if `--out` is unset. `--report` writes every finding
(path and kind) as JSON. A one-line count per kind is always printed to stderr.

## Scaffold a wrapper chart

```sh
values-schema infer oci://ghcr.io/kyverno/charts/kyverno --version 3.9.2 \
  --wrapper helm/kyverno --chart-version 3.9.5
```

This writes `Chart.yaml`, `values.yaml` and `values.schema.json` into a directory that holds no
chart yet:
- `Chart.yaml` depends on the source, with the repository defaulting to the OCI reference minus its
  chart name (`--repository` for a directory or archive).
- `values.yaml` starts empty.
- `values.schema.json` is the schema inferred for the wrapper. Its root has `global` and the
  dependency's key, like kyverno-blueprint's hand-written one, with the dependency's values typed
  below it.

After editing the wrapper's `values.yaml`, regenerate:

```sh
helm dependency build helm/kyverno
values-schema infer helm/kyverno --out helm/kyverno/values.schema.json
```

Exit codes: 0 success, 1 the chart could not be inferred, 2 bad usage.
