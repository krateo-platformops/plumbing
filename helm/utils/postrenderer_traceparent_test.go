package utils

import (
	"bytes"
	"strings"
	"testing"
)

func TestLabelsPostRender_Traceparent(t *testing.T) {
	const tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

	// Krateo resource (nested Composition CR): stamped, because a Krateo controller
	// reconciles it and continues the distributed trace.
	in := bytes.NewBufferString("apiVersion: composition.krateo.io/v0-1-6\nkind: KrateoSseProxy\nmetadata:\n  name: x\n")
	out, err := (&LabelsPostRender{CompositionName: "comp"}).
		WithTraceparent(tp, "vendor=state").
		Run(in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "krateo.io/traceparent: "+tp) {
		t.Fatalf("traceparent annotation missing on krateo resource:\n%s", s)
	}
	if !strings.Contains(s, "krateo.io/tracestate: vendor=state") {
		t.Fatalf("tracestate annotation missing on krateo resource:\n%s", s)
	}

	// Leaf Kubernetes resource (core group): NOT stamped. Stamping a per-reconcile
	// annotation on e.g. a type=LoadBalancer Service churns it every reconcile and makes
	// the cloud service-controller re-ensure the LB (constant IP reserve/release on GKE).
	inSvc := bytes.NewBufferString("apiVersion: v1\nkind: Service\nmetadata:\n  name: leaf\n")
	outSvc, err := (&LabelsPostRender{CompositionName: "comp"}).
		WithTraceparent(tp, "vendor=state").
		Run(inSvc)
	if err != nil {
		t.Fatalf("Run(service): %v", err)
	}
	if strings.Contains(outSvc.String(), "krateo.io/traceparent") {
		t.Fatalf("traceparent must NOT be stamped on a leaf k8s resource:\n%s", outSvc.String())
	}
	// ...but the composition labels still apply to every child, krateo or not.
	if !strings.Contains(outSvc.String(), "krateo.io/composition-name: comp") {
		t.Fatalf("composition labels must still be applied to leaf resources:\n%s", outSvc.String())
	}

	// Additive: without WithTraceparent there is no traceparent annotation.
	in2 := bytes.NewBufferString("apiVersion: composition.krateo.io/v0-1-6\nkind: KrateoSseProxy\nmetadata:\n  name: y\n")
	out2, err := (&LabelsPostRender{CompositionName: "comp"}).Run(in2)
	if err != nil {
		t.Fatalf("Run(no traceparent): %v", err)
	}
	if strings.Contains(out2.String(), "krateo.io/traceparent") {
		t.Fatalf("traceparent must be absent when unset:\n%s", out2.String())
	}
}

func TestIsKrateoAPIGroup(t *testing.T) {
	cases := map[string]bool{
		"composition.krateo.io/v0-1-6":   true,
		"core.krateo.io/v1alpha1":        true,
		"krateo.io/v1":                   true,
		"widgets.templates.krateo.io/v1": true,
		"v1":                             false, // core k8s (Service, ConfigMap)
		"apps/v1":                        false, // Deployment
		"networking.k8s.io/v1":           false, // Ingress
		"cert-manager.io/v1":             false, // not krateo
		"notkrateo.io/v1":                false, // suffix must be .krateo.io, not krateo.io substring
	}
	for apiVersion, want := range cases {
		if got := isKrateoAPIGroup(apiVersion); got != want {
			t.Errorf("isKrateoAPIGroup(%q) = %v, want %v", apiVersion, got, want)
		}
	}
}
