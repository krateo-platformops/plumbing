package utils

import (
	"bytes"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/kustomize/kyaml/kio"
)

// isKrateoAPIGroup reports whether an apiVersion belongs to a Krateo API group
// (krateo.io or any *.krateo.io subgroup, e.g. composition.krateo.io). Only Krateo
// controllers reconciling nested Krateo CRs read krateo.io/traceparent to continue the
// distributed trace, so the annotation is meaningful ONLY on Krateo resources.
//
// Stamping the per-reconcile traceparent on leaf Kubernetes resources (Service,
// Deployment, ConfigMap, ...) is not just inert, it is HARMFUL: the cdc re-applies every
// composition each reconcile, so the changing annotation patches every child object every
// cycle. For a type=LoadBalancer Service that update makes the cloud service-controller
// re-run EnsureLoadBalancer every reconcile — observed on GKE as a constant LB IP
// reserve/release thrash (EnsuringLoadBalancer firing ~1/min for days on every LB Service).
// Restricting the stamp to Krateo groups preserves cross-composition trace propagation while
// stopping the leaf-resource churn.
func isKrateoAPIGroup(apiVersion string) bool {
	group := apiVersion
	if i := strings.IndexByte(apiVersion, '/'); i >= 0 {
		group = apiVersion[:i]
	}
	return group == "krateo.io" || strings.HasSuffix(group, ".krateo.io")
}

type LabelsPostRender struct {
	UID                  types.UID
	CompositionGVR       schema.GroupVersionResource
	CompositionName      string
	CompositionNamespace string
	CompositionGVK       schema.GroupVersionKind
	KrateoNamespace      string
	// Traceparent/Tracestate, when set, are stamped as krateo.io/traceparent (+ tracestate)
	// annotations on every rendered child manifest, so the controller that reconciles the
	// child continues the distributed trace across the compose-of-compositions tree.
	Traceparent string
	Tracestate  string
}

func LabelPostRenderFromSpec(mg *unstructured.Unstructured, pluralizer pluralizer, krateoNamespace string) (*LabelsPostRender, error) {
	gvk := mg.GroupVersionKind()
	gvr, err := pluralizer.GVKtoGVR(gvk)
	if err != nil {
		return nil, fmt.Errorf("failed to get GVR from GVK: %w", err)
	}

	return &LabelsPostRender{
		UID:                  mg.GetUID(),
		CompositionGVR:       gvr,
		CompositionName:      mg.GetName(),
		CompositionNamespace: mg.GetNamespace(),
		CompositionGVK:       gvk,
		KrateoNamespace:      krateoNamespace,
	}, nil
}

// WithTraceparent sets the W3C traceparent (+ optional tracestate) to stamp as the
// krateo.io/traceparent annotation on every rendered child manifest. Returns r for chaining.
// Additive: callers that don't set it keep the existing label-only behavior.
func (r *LabelsPostRender) WithTraceparent(traceparent, tracestate string) *LabelsPostRender {
	r.Traceparent = traceparent
	r.Tracestate = tracestate
	return r
}

func (r *LabelsPostRender) Run(renderedManifests *bytes.Buffer) (modifiedManifests *bytes.Buffer, err error) {
	nodes, err := kio.FromBytes(renderedManifests.Bytes())
	if err != nil {
		return renderedManifests, fmt.Errorf("failed to parse rendered manifests: %w", err)
	}
	for _, v := range nodes {
		labels := v.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		// your labels
		labels["krateo.io/composition-id"] = string(r.UID)
		labels["krateo.io/composition-group"] = r.CompositionGVR.Group
		labels["krateo.io/composition-installed-version"] = r.CompositionGVR.Version
		labels["krateo.io/composition-resource"] = r.CompositionGVR.Resource
		labels["krateo.io/composition-name"] = r.CompositionName
		labels["krateo.io/composition-namespace"] = r.CompositionNamespace
		labels["krateo.io/composition-kind"] = r.CompositionGVK.Kind
		labels["krateo.io/krateo-namespace"] = r.KrateoNamespace
		v.SetLabels(labels)

		// Only stamp the trace context on Krateo resources (nested Composition CRs) whose
		// controllers actually continue the trace; never on leaf k8s resources, where a
		// per-reconcile annotation change churns the object (and re-ensures LoadBalancer
		// Services on every reconcile). See isKrateoAPIGroup.
		if r.Traceparent != "" && isKrateoAPIGroup(v.GetApiVersion()) {
			annotations := v.GetAnnotations()
			if annotations == nil {
				annotations = make(map[string]string)
			}
			annotations["krateo.io/traceparent"] = r.Traceparent
			if r.Tracestate != "" {
				annotations["krateo.io/tracestate"] = r.Tracestate
			}
			v.SetAnnotations(annotations)
		}
	}

	str, err := kio.StringAll(nodes)
	if err != nil {
		return renderedManifests, fmt.Errorf("failed to convert nodes to string: %w", err)
	}

	return bytes.NewBufferString(str), nil
}
