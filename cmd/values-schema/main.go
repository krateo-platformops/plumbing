// Command values-schema infers a values.schema.json for a Helm chart that ships none, with
// plumbing's valuesschema library (core-provider#568, child 4). It is the offline half of D2: a
// person runs it, reviews the output, and commits it, either as a chart's own schema or as a
// wrapper chart's. It lives in plumbing so it moves in lockstep with the library: the schema it
// writes always carries the RulesVersion of the valuesschema it was built with.
//
// Install:
//
//	go install github.com/krateo-platformops/plumbing/cmd/values-schema@<plumbing version>
//
// Usage:
//
//	values-schema infer SOURCE [--version V] [--out FILE] [--report FILE]
//	values-schema infer SOURCE [--version V] --wrapper DIR --chart-version V [--name N] [--repository R] [--report FILE]
//
// SOURCE is a chart directory, a chart archive (.tgz), or an OCI reference (oci://host/path/name,
// with --version). A directory's dependencies must be vendored under charts/ (helm dependency
// build); a directory's own values.schema.json, if any, is ignored, so an existing wrapper is
// regenerated in place with:
//
//	helm dependency build helm/kyverno
//	values-schema infer helm/kyverno --out helm/kyverno/values.schema.json
//
// --wrapper scaffolds a NEW wrapper chart in DIR (which must not already hold a Chart.yaml): a
// Chart.yaml depending on SOURCE, an empty values.yaml, and the values.schema.json inferred for that
// wrapper, so the dependency's values sit under its key next to `global`. --repository is the
// dependency's repository; for an OCI SOURCE it defaults to the reference without its chart name.
//
// The schema goes to --out, or stdout. The report (every finding, as JSON) goes to --report; a
// one-line count per finding kind is always printed to stderr.
//
// Exit codes: 0 success, 1 the chart could not be inferred, 2 bad usage.
package main

import (
	"context"
	"os"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, helmPuller{}))
}
