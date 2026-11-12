package rbacgen

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"

	"github.com/krateo-platformops/plumbing/kubeutil/objectclient"
)

func TestBuildRoleForSecretShape(t *testing.T) {
	role := BuildRoleForSecret("ns1", "widget-abc123-secrets", []string{"db-creds", "api-token"}, []string{"get", "list", "watch"})

	if role.Kind != "Role" || role.APIVersion != "rbac.authorization.k8s.io/v1" {
		t.Fatalf("TypeMeta not set: kind=%q apiVersion=%q", role.Kind, role.APIVersion)
	}
	if role.Namespace != "ns1" || role.Name != "widget-abc123-secrets" {
		t.Fatalf("unexpected ObjectMeta: %+v", role.ObjectMeta)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("expected exactly one rule, got %d", len(role.Rules))
	}
	rule := role.Rules[0]
	if len(rule.APIGroups) != 1 || rule.APIGroups[0] != "" {
		t.Fatalf("expected core apiGroup, got %v", rule.APIGroups)
	}
	if len(rule.Resources) != 1 || rule.Resources[0] != "secrets" {
		t.Fatalf("expected secrets resource, got %v", rule.Resources)
	}
	if len(rule.ResourceNames) != 2 || rule.ResourceNames[0] != "db-creds" || rule.ResourceNames[1] != "api-token" {
		t.Fatalf("expected exact resourceNames scoping, got %v", rule.ResourceNames)
	}
}

func TestBuildRoleBindingForRoleShape(t *testing.T) {
	rb := BuildRoleBindingForRole("ns1", "widget-abc123-secrets", "widget-abc123-secrets",
		types.NamespacedName{Namespace: "rdc-system", Name: "rest-dynamic-controller"})

	if rb.Kind != "RoleBinding" || rb.APIVersion != "rbac.authorization.k8s.io/v1" {
		t.Fatalf("TypeMeta not set: kind=%q apiVersion=%q", rb.Kind, rb.APIVersion)
	}
	if rb.RoleRef.Kind != "Role" || rb.RoleRef.Name != "widget-abc123-secrets" || rb.RoleRef.APIGroup != rbacv1.GroupName {
		t.Fatalf("unexpected RoleRef: %+v", rb.RoleRef)
	}
	if len(rb.Subjects) != 1 {
		t.Fatalf("expected exactly one subject, got %d", len(rb.Subjects))
	}
	subj := rb.Subjects[0]
	if subj.Kind != rbacv1.ServiceAccountKind || subj.Name != "rest-dynamic-controller" || subj.Namespace != "rdc-system" {
		t.Fatalf("unexpected subject (must allow SA namespace != Role namespace): %+v", subj)
	}
	if rb.Namespace != "ns1" {
		t.Fatalf("RoleBinding must live in the Role's namespace, got %q", rb.Namespace)
	}
}

// TestApplyThroughDynamicClient proves the TypeMeta-must-be-set gotcha documented on the package: a
// Role/RoleBinding built here must survive conversion to unstructured and a dynamic-client Apply.
func TestApplyThroughDynamicClient(t *testing.T) {
	roleGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	scheme := runtime.NewScheme()
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{roleGVR: "RoleList"})

	role := BuildRoleForSecret("ns1", "widget-abc123-secrets", []string{"db-creds"}, []string{"get"})

	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(role)
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}
	u := &unstructured.Unstructured{Object: raw}
	if u.GetKind() == "" || u.GetAPIVersion() == "" {
		t.Fatalf("expected TypeMeta to survive unstructured conversion, got kind=%q apiVersion=%q", u.GetKind(), u.GetAPIVersion())
	}

	if _, err := objectclient.Apply(context.Background(), dyn, roleGVR, u, objectclient.ApplyOptions{}); err != nil {
		t.Fatalf("Apply failed — likely a missing-TypeMeta regression: %v", err)
	}
}
