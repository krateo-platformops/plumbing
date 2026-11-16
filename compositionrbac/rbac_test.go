package compositionrbac

import (
	"testing"
)

func TestInitRole(t *testing.T) {
	name := "test-role"
	namespace := "default"
	role := initRole(name, namespace)

	if role.ObjectMeta.Name != name {
		t.Errorf("expected role name %s, got %s", name, role.ObjectMeta.Name)
	}

	if len(role.Rules) != 0 {
		t.Errorf("expected role rules to be empty, got %d rules", len(role.Rules))
	}
}

func TestInitRoleBinding(t *testing.T) {
	name := "test-rolebinding"
	roleRefName := "test-role"
	namespace := "default"
	saName := "test-sa"
	saNamespace := "default"
	roleBinding := initRoleBinding(name, roleRefName, namespace, saName, saNamespace)

	if roleBinding.ObjectMeta.Name != name {
		t.Errorf("expected role binding name %s, got %s", name, roleBinding.ObjectMeta.Name)
	}

	if roleBinding.RoleRef.Name != roleRefName {
		t.Errorf("expected role ref name %s, got %s", roleRefName, roleBinding.RoleRef.Name)
	}

	if len(roleBinding.Subjects) != 1 {
		t.Errorf("expected 1 subject, got %d", len(roleBinding.Subjects))
	}

	subject := roleBinding.Subjects[0]
	if subject.Kind != "ServiceAccount" || subject.Name != saName || subject.Namespace != saNamespace {
		t.Errorf("expected subject to be ServiceAccount %s in namespace %s, got %s %s in namespace %s", saName, saNamespace, subject.Kind, subject.Name, subject.Namespace)
	}
}

func TestInitClusterRole(t *testing.T) {
	name := "test-clusterrole"
	clusterRole := initClusterRole(name)

	if clusterRole.ObjectMeta.Name != name {
		t.Errorf("expected cluster role name %s, got %s", name, clusterRole.ObjectMeta.Name)
	}

	if len(clusterRole.Rules) != 0 {
		t.Errorf("expected cluster role rules to be empty, got %d rules", len(clusterRole.Rules))
	}
}

func TestInitClusterRoleBinding(t *testing.T) {
	name := "test-clusterrolebinding"
	roleRefName := "test-clusterrole"
	saName := "test-sa"
	saNamespace := "default"
	clusterRoleBinding := initClusterRoleBinding(name, roleRefName, saName, saNamespace)

	if clusterRoleBinding.ObjectMeta.Name != name {
		t.Errorf("expected cluster role binding name %s, got %s", name, clusterRoleBinding.ObjectMeta.Name)
	}

	if clusterRoleBinding.RoleRef.Name != roleRefName {
		t.Errorf("expected role ref name %s, got %s", roleRefName, clusterRoleBinding.RoleRef.Name)
	}

	if len(clusterRoleBinding.Subjects) != 1 {
		t.Errorf("expected 1 subject, got %d", len(clusterRoleBinding.Subjects))
	}

	subject := clusterRoleBinding.Subjects[0]
	if subject.Kind != "ServiceAccount" || subject.Name != saName || subject.Namespace != saNamespace {
		t.Errorf("expected subject to be ServiceAccount %s in namespace %s, got %s %s in namespace %s", saName, saNamespace, subject.Kind, subject.Name, subject.Namespace)
	}
}
func TestCreateNamespace(t *testing.T) {
	name := "test-namespace"
	releaseName := "test-release"
	releaseNamespace := "test-namespace"
	namespace := createNamespace(name, releaseName, releaseNamespace)

	if namespace.ObjectMeta.Name != name {
		t.Errorf("expected namespace name %s, got %s", name, namespace.ObjectMeta.Name)
	}

	expectedLabel := "Helm"
	if namespace.ObjectMeta.Labels["app.kubernetes.io/managed-by"] != expectedLabel {
		t.Errorf("expected label app.kubernetes.io/managed-by to be %s, got %s", expectedLabel, namespace.ObjectMeta.Labels["app.kubernetes.io/managed-by"])
	}
	if namespace.ObjectMeta.Annotations["meta.helm.sh/release-name"] != releaseName {
		t.Errorf("expected annotation meta.helm.sh/release-name to be %s, got %s", releaseName, namespace.ObjectMeta.Annotations["meta.helm.sh/release-name"])
	}
	if namespace.ObjectMeta.Annotations["meta.helm.sh/release-namespace"] != releaseNamespace {
		t.Errorf("expected annotation meta.helm.sh/release-namespace to be %s, got %s", releaseNamespace, namespace.ObjectMeta.Annotations["meta.helm.sh/release-namespace"])
	}

	if len(namespace.ObjectMeta.Labels) != 1 {
		t.Errorf("expected 1 label, got %d labels", len(namespace.ObjectMeta.Labels))
	}
}
