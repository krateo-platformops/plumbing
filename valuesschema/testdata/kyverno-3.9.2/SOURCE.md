Extracted from upstream `kyverno` chart 3.9.2 (`helm pull kyverno --repo https://kyverno.github.io/kyverno/ --version 3.9.2`,
archive sha256 prefix f6fe7184544d5e2e), keeping only what inference reads: the 17 `Chart.yaml`, `values.yaml` and
`values.schema.json` files at every level. Templates and CRDs are omitted (the archive is 780 KB). Upstream ships no
root `values.schema.json`, which is why it is the motivating case for core-provider#568.
