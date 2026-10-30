package helm

import (
	"errors"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestIsRESTMappingMiss(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("connection refused"), false},
		{"helm stringified no-matches", errors.New(`install failed: error while running post render on files: failed to build rendered manifests: resource mapping not found for name: "snowplow-crd" namespace: "krateo-system" from "": no matches for kind "SnowplowCrds" in version "composition.krateo.io/v1-9-0" ensure CRDs are installed first`), true},
		{"resource mapping not found", errors.New("resource mapping not found for name x"), true},
		{"typed NoKindMatchError wrapped", fmt.Errorf("render: %w", &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "composition.krateo.io", Kind: "SnowplowCrds"}, SearchedVersions: []string{"v1-9-0"}}), true},
	}
	for _, tc := range cases {
		if got := IsRESTMappingMiss(tc.err); got != tc.want {
			t.Errorf("%s: IsRESTMappingMiss=%v want %v", tc.name, got, tc.want)
		}
	}
}
