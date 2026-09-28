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
