package utils

import (
	"fmt"

	"github.com/krateo-platformops/plumbing/maps"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	AnnotationKeyReconciliationGracefullyPaused = "krateo.io/gracefully-paused"
)

type Values map[string]any

type pluralizer interface {
	GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error)
}

// ValuesFromSpec returns an object's .spec as Helm values.
//
// A MISSING spec is "no values", exactly like `spec: {}` (#42). An object of a Kind whose schema
// declares no properties — a composition of a parameter-less blueprint — has no spec at all: the
// apiserver keeps an empty spec out of the object, and clients prune empty objects before they
// send it. Treating that as an error failed every reconcile of such a composition (chart-inspector
// answered 500, so the chart never rendered). The result is a non-nil map, because callers write
// into it (InjectGlobalValues). A spec that exists but is not a map is still an error.
func ValuesFromSpec(un *unstructured.Unstructured) (Values, error) {
	if un == nil {
		return nil, nil
	}

	spec, ok, err := maps.NestedMap(un.UnstructuredContent(), "spec")
	if err != nil {
		return nil, fmt.Errorf("spec field is not an object: %w", err)
	}
	if !ok {
		return Values{}, nil
	}

	return spec, nil
}

func (v Values) InjectGlobalValues(mg *unstructured.Unstructured, pluralizer pluralizer, krateoNamespace string) error {
	gvk := mg.GroupVersionKind()
	gvr, err := pluralizer.GVKtoGVR(gvk)
	if err != nil {
		return err
	}
	gracefullyPaused := "false"
	if val, found := mg.GetAnnotations()[AnnotationKeyReconciliationGracefullyPaused]; found && val == "true" {
		gracefullyPaused = "true"
	}

	gv := map[string]any{
		"compositionName":             mg.GetName(),
		"compositionNamespace":        mg.GetNamespace(),
		"compositionId":               string(mg.GetUID()),
		"compositionApiVersion":       mg.GetAPIVersion(),
		"compositionGroup":            gvr.Group,
		"compositionKind":             gvk.Kind,
		"compositionResource":         gvr.Resource,
		"compositionInstalledVersion": gvr.Version,
		"gracefullyPaused":            gracefullyPaused,
		"krateoNamespace":             krateoNamespace,
	}

	err = maps.SetNestedField(v, gv, "global")
	if err != nil {
		return fmt.Errorf("failed to set global values: %w", err)
	}
	return nil
}
