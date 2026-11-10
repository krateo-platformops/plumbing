package utils

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// mockPluralizer implements the pluralizer interface for testing
type mockPluralizer struct {
	err error
	gvr schema.GroupVersionResource
}

func (m *mockPluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	if m.err != nil {
		return schema.GroupVersionResource{}, m.err
	}
	return m.gvr, nil
}

func TestValuesFromSpec(t *testing.T) {
	tests := []struct {
		name    string
		input   *unstructured.Unstructured
		want    Values
		wantErr bool
	}{
		{
			name: "Success - valid spec",
			input: &unstructured.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{"foo": "bar"},
				},
			},
			want:    Values{"foo": "bar"},
			wantErr: false,
		},
		{
			name:    "Nil input returns nil",
			input:   nil,
			want:    nil,
			wantErr: false,
		},
		{
			// A composition of a blueprint with no values has no spec at all (#42): no values,
			// not an error — and a map a caller can write into.
			name: "Missing spec means no values",
			input: &unstructured.Unstructured{
				Object: map[string]any{"kind": "E2eBp0928b"},
			},
			want:    Values{},
			wantErr: false,
		},
		{
			name: "Error - spec is not an object",
			input: &unstructured.Unstructured{
				Object: map[string]any{"spec": "not-a-map"},
			},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValuesFromSpec(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, got)
				if tt.input != nil {
					assert.NotNil(t, got, "a caller writes into the values; a nil map would panic")
				}
			}
		})
	}
}

func TestInjectGlobalValues(t *testing.T) {
	tests := []struct {
		name            string
		mg              *unstructured.Unstructured
		krateoNamespace string
		pluralizer      *mockPluralizer
		initialValues   Values
		wantErr         bool
		validate        func(t *testing.T, v Values)
	}{
		{
			name: "Success - inject all fields and handle annotation",
			mg: &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "stable.krateo.io/v1",
					"kind":       "Composition",
					"metadata": map[string]any{
						"name":      "my-comp",
						"namespace": "my-ns",
						"uid":       "12345",
						"annotations": map[string]any{
							AnnotationKeyReconciliationGracefullyPaused: "true",
						},
					},
				},
			},
			krateoNamespace: "krateo-system",
			pluralizer: &mockPluralizer{
				gvr: schema.GroupVersionResource{
					Group:    "stable.krateo.io",
					Version:  "v1",
					Resource: "compositions",
				},
			},
			initialValues: Values{},
			wantErr:       false,
			validate: func(t *testing.T, v Values) {
				global, ok := v["global"].(map[string]any)
				assert.True(t, ok)
				assert.Equal(t, "my-comp", global["compositionName"])
				assert.Equal(t, "true", global["gracefullyPaused"])
				assert.Equal(t, "krateo-system", global["krateoNamespace"])
				assert.Equal(t, "compositions", global["compositionResource"])
			},
		},
		{
			name: "Gracefully paused is false by default",
			mg: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{"name": "test"},
				},
			},
			pluralizer:    &mockPluralizer{},
			initialValues: Values{},
			wantErr:       false,
			validate: func(t *testing.T, v Values) {
				global := v["global"].(map[string]any)
				assert.Equal(t, "false", global["gracefullyPaused"])
			},
		},
		{
			name: "Error - pluralizer failure",
			mg:   &unstructured.Unstructured{},
			pluralizer: &mockPluralizer{
				err: fmt.Errorf("lookup error"),
			},
			initialValues: Values{},
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.initialValues.InjectGlobalValues(tt.mg, tt.pluralizer, tt.krateoNamespace)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				tt.validate(t, tt.initialValues)
			}
		})
	}
}

// #115. The composition's own spec.global keys reach the chart beside the injected ones. Before the
// fix InjectGlobalValues assigned the global map, so every user key was dropped: 4 of 85
// compositions on krateo-057 set global.imageRegistry and none of their helm releases had it.
func TestInjectGlobalValuesKeepsTheCompositionsOwnGlobals(t *testing.T) {
	mg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0",
		"kind":       "Frontend",
		"metadata":   map[string]any{"name": "frontend", "namespace": "krateo-system", "uid": "u-1"},
	}}
	pl := &mockPluralizer{gvr: schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "frontends"}}
	injected := []string{"compositionName", "compositionNamespace", "compositionId", "compositionApiVersion",
		"compositionGroup", "compositionKind", "compositionResource", "compositionInstalledVersion",
		"gracefullyPaused", "krateoNamespace"}

	t.Run("a user key survives beside every injected key", func(t *testing.T) {
		user := map[string]any{"imageRegistry": "mirror.local", "nested": map[string]any{"a": "b"}}
		v := Values{"global": user, "replicaCount": int64(2)}
		if err := v.InjectGlobalValues(mg, pl, "krateo-system"); err != nil {
			t.Fatal(err)
		}
		g := v["global"].(map[string]any)
		if g["imageRegistry"] != "mirror.local" {
			t.Errorf("global.imageRegistry = %v, want mirror.local (#115: user globals were dropped)", g["imageRegistry"])
		}
		if n, ok := g["nested"].(map[string]any); !ok || n["a"] != "b" {
			t.Errorf("global.nested = %v, want the user's map", g["nested"])
		}
		for _, k := range injected {
			if _, ok := g[k]; !ok {
				t.Errorf("injected key global.%s missing", k)
			}
		}
		if len(g) != len(injected)+2 {
			t.Errorf("global has %d keys, want %d (10 injected + 2 user): %v", len(g), len(injected)+2, g)
		}
		if v["replicaCount"] != int64(2) {
			t.Errorf("a non-global value changed: %v", v["replicaCount"])
		}
		// The caller's nested map is not mutated: the merge builds a fresh one.
		if len(user) != 2 {
			t.Errorf("the input spec.global was mutated: %v", user)
		}
	})

	t.Run("an injected key wins over a user key of the same name", func(t *testing.T) {
		v := Values{"global": map[string]any{"compositionName": "someone-else", "krateoNamespace": "elsewhere"}}
		if err := v.InjectGlobalValues(mg, pl, "krateo-system"); err != nil {
			t.Fatal(err)
		}
		g := v["global"].(map[string]any)
		if g["compositionName"] != "frontend" || g["krateoNamespace"] != "krateo-system" {
			t.Errorf("compositionName=%v krateoNamespace=%v, want the controller's facts", g["compositionName"], g["krateoNamespace"])
		}
	})

	t.Run("a null global is treated as absent", func(t *testing.T) {
		v := Values{"global": nil}
		if err := v.InjectGlobalValues(mg, pl, "krateo-system"); err != nil {
			t.Fatal(err)
		}
		if g := v["global"].(map[string]any); len(g) != len(injected) {
			t.Errorf("global = %v, want exactly the %d injected keys", g, len(injected))
		}
	})

	t.Run("a non-object global is an error, not a silent replacement", func(t *testing.T) {
		v := Values{"global": "registry.local"}
		if err := v.InjectGlobalValues(mg, pl, "krateo-system"); err == nil {
			t.Error("want an error for spec.global that is a string")
		}
	})
}
