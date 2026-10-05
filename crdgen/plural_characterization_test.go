// Characterization test for the plural crdgen stamps into spec.names.plural.
//
// Not a correctness test -- there is no independent definition of correct. It records what the
// derivation currently PRODUCES, because that value is a published contract and a change to it is
// invisible at the point of change.
//
// Why it is a contract rather than an internal detail: core-provider reads the plural back out of
// the cluster (Pluralizer.GVKtoGVR -> kubeutil/plurals.Get -> ResolveAPINames, which takes el.Name
// from apiserver discovery). So whatever is written here becomes the resource path the whole
// platform uses, and the three blueprint catalogues -- krateo-aws-blueprint, krateo-gcp-blueprint,
// krateo-azure-blueprint, ~737 charts at first release through ONE shared generator core --
// reproduce it to build customform `resource:` paths and getCRD API paths.
//
// The failure mode is not a build break. It is a form that 404s against the apiserver while every
// other thing about the chart looks correct. A `flect` version bump -- an ordinary indirect
// dependency update, in this repo, reviewed by someone with no reason to think about blueprints --
// is enough to cause it.
//
// This asserts the EMITTED plural from a generated CRD, not the output of the formula. That
// distinction is load-bearing: a test comparing one formula against another cannot verify a fix,
// because both formulas keep producing their own answers forever regardless of what the generator
// does. Asking the generator what it actually emitted is the only question with a useful answer.
package crdgen

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func pluralFor(t *testing.T, kind string) (string, string) {
	t.Helper()
	out, err := Generate(Options{
		Group:        "composition.krateo.io",
		Version:      "v1alpha1",
		Kind:         kind,
		Managed:      true,
		SpecSchema:   []byte(`{"type":"object","properties":{"replicas":{"type":"integer"}}}`),
		StatusSchema: []byte(`{"type":"object","properties":{"ready":{"type":"boolean"}}}`),
	})
	if err != nil {
		t.Fatalf("Generate(%q): %v", kind, err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatalf("unmarshalling generated CRD for %q: %v", kind, err)
	}
	return crd.Spec.Names.Plural, crd.Name
}

func TestPlural_Characterization(t *testing.T) {
	// Values are OBSERVED -- generated and read back on 2026-10-05 -- not predicted. A predicted
	// table would encode the author's model of flect rather than flect, which is exactly the error
	// that produced the AzureSubscriptionAlias case below.
	cases := []struct{ kind, wantPlural string }{
		// Everyday shapes.
		{"AccessReview", "accessreviews"},
		{"BuilderPublish", "builderpublishes"},
		{"Gateway", "gateways"},

		// Irregulars, where flect versions are most likely to differ.
		{"Analysis", "analyses"},
		{"Policy", "policies"},
		{"Index", "indices"},
		{"Status", "statuses"},
		{"Box", "boxes"},
		{"Child", "children"},

		// "demoes", not "demos". Surprising, and the default Kind in several test harnesses.
		{"Demo", "demoes"},

		// Acronym that core-provider's Pascalize collapses: krateo-sse-proxy -> KrateoSseProxy.
		{"KrateoSseProxy", "krateosseproxies"},

		// Consecutive capitals DO survive into a Kind from the blueprint catalogues. 38 of 923
		// generated Kinds look like this, and all of them pluralize uneventfully -- recorded
		// because an earlier analysis (mine) wrongly blamed this shape for the case below.
		{"GcpSQLInstance", "gcpsqlinstances"},

		// The case that made this file exist. Lowercasing BEFORE pluralizing yields
		// "azuresubscriptionalias" -- unchanged, because the rule keyed on the word "alias" does
		// not fire on an unsegmentable lowercase compound. crdgen pluralizes first, so it is
		// correct here; a downstream generator modelling the opposite order shipped the wrong
		// plural into a customform that would have 404'd.
		//
		// No simple predicate separates this Kind from the safe ones. Two were proposed and both
		// refuted by data: "consecutive capitals" (38 such Kinds, none diverge) and "final word
		// ends in -s" (1 of 12 diverges). That is the argument for pinning rather than reasoning.
		{"AzureSubscriptionAlias", "azuresubscriptionaliases"},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			got, crdName := pluralFor(t, tc.kind)
			if got != tc.wantPlural {
				t.Errorf("plural for Kind %q = %q, want %q\n"+
					"This value is read back from apiserver discovery by core-provider and reproduced by the\n"+
					"AWS/GCP/Azure blueprint catalogues to build resource paths. Changing it renames resources\n"+
					"and breaks customforms that reference them. If this change is intended, update this table\n"+
					"AND tell those repositories.", tc.kind, got, tc.wantPlural)
			}
			// metadata.name is derived from the plural, so it drifts silently with it.
			if want := tc.wantPlural + ".composition.krateo.io"; crdName != want {
				t.Errorf("CRD metadata.name = %q, want %q", crdName, want)
			}
		})
	}
}
