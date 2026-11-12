// Package content compares Kubernetes objects by content, in the one form the AUDIT-4 ruling
// defines (snowplow, 2026-11-11): what an object grants or holds, not how it happens to be written.
//
// It is shared so every writer that records "this write changed content" (plumbing's helm verdicts,
// a controller recording objectclient writes) answers the same way for the same two objects.
package content

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"k8s.io/apimachinery/pkg/runtime"
)

// Keys are the metadata keys that count as content: the annotations and labels authorization
// reads (audit.AuthorizationAnnotations, audit.AuthorizationLabels). Every other annotation and
// label is metadata.
type Keys struct {
	Annotations []string
	Labels      []string
}

// Differs reports whether before and after differ in content: every top-level field other
// than apiVersion, kind, metadata and status, and the annotations listed in contentAnnotations,
// compared canonically (see Canonical). It is DiffersWith with no content labels.
func Differs(before, after runtime.Object, contentAnnotations []string) (bool, error) {
	return DiffersWith(before, after, Keys{Annotations: contentAnnotations})
}

// DiffersWith is Differs counting keys.Labels as content too: a declared label changed, added or
// removed is a change; an undeclared one is not (snowplow, 2026-11-12: authn's session Secrets
// carry authorization-bearing labels that tenant onboarding selects on).
func DiffersWith(before, after runtime.Object, keys Keys) (bool, error) {
	b, err := CanonicalWith(before, keys)
	if err != nil {
		return false, fmt.Errorf("before: %w", err)
	}
	a, err := CanonicalWith(after, keys)
	if err != nil {
		return false, fmt.Errorf("after: %w", err)
	}
	return !reflect.DeepEqual(a, b), nil
}

// Canonical is obj's content in the form the AUDIT-4 ruling compares (snowplow, 2026-11-11):
//
// DELIBERATE, not a shortcut: krateo-platformops/installer#413 condition 2 says ".spec changed",
// and the AUDIT-4 lead ruled (2026-11-11, recorded on installer#410) that this means every
// top-level field other than apiVersion, kind, metadata and status. Many kinds keep their desired
// state outside .spec (ConfigMap and Secret data, RBAC rules and subjects, admission webhooks), and
// reading the condition literally would make a change to them never count.
//
//   - every top-level field except apiVersion, kind, metadata and status. That is .spec where there
//     is one; data and type for a Secret or ConfigMap; webhooks for an admission configuration.
//
//   - a Secret's stringData folded into data, as the apiserver does, so the same value written
//     either way is the same content. A changed type is a change.
//
//   - RBAC: a Role's or ClusterRole's rules, and a binding's subjects, as sets: order and
//     duplicates don't matter, and within a rule neither do the order or duplicates of its verbs,
//     apiGroups, resources, resourceNames and nonResourceURLs. A ClusterRole's
//     aggregationRule.clusterRoleSelectors compare as a set too. roleRef compares as it is.
//
//   - an object member whose value is null left out, at every depth, as audit's c1 form does (a
//     null list element is kept, and a map left empty stays an empty map).
//
//   - the annotations listed in contentAnnotations (UpgradeConfig.ContentAnnotations: the audit
//     schema's content_annotations, which authorization reads), under metadata.annotations, as
//     audit.ContentSHA256 keeps them: each one present with a non-null value, the rest dropped.
//
// Labels and every other annotation are metadata, so they never count here (CanonicalWith counts
// declared labels).
func Canonical(obj runtime.Object, contentAnnotations []string) (map[string]interface{}, error) {
	return CanonicalWith(obj, Keys{Annotations: contentAnnotations})
}

// CanonicalWith is Canonical keeping keys.Labels too, under metadata.labels, each one present with
// a non-null value, the rest dropped. With no labels declared it is exactly Canonical's form.
func CanonicalWith(obj runtime.Object, keys Keys) (map[string]interface{}, error) {
	if obj == nil {
		return nil, fmt.Errorf("no object")
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj.DeepCopyObject())
	if err != nil {
		return nil, err
	}
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Group == "" && gvk.Kind == "Secret" {
		foldStringData(u)
	}
	keptA := keptMeta(u, "annotations", keys.Annotations)
	keptL := keptMeta(u, "labels", keys.Labels)
	for _, k := range []string{"apiVersion", "kind", "metadata", "status"} {
		delete(u, k)
	}
	if len(keptA) > 0 || len(keptL) > 0 {
		meta := map[string]interface{}{}
		if len(keptA) > 0 {
			meta["annotations"] = keptA
		}
		if len(keptL) > 0 {
			meta["labels"] = keptL
		}
		u["metadata"] = meta
	}
	// A null member says what an absent one does, at every depth, as audit's c1 form reads it:
	// otherwise a verdict would call a write changed whose content_sha256 is the same. Before the
	// RBAC sets, so set elements compare without them.
	dropNullMembers(u)
	if gvk.Group == "rbac.authorization.k8s.io" {
		switch gvk.Kind {
		case "Role", "ClusterRole":
			if rules, ok := u["rules"].([]interface{}); ok {
				for i, r := range rules {
					if m, ok := r.(map[string]interface{}); ok {
						for _, f := range []string{"verbs", "apiGroups", "resources", "resourceNames", "nonResourceURLs"} {
							if l, ok := m[f].([]interface{}); ok {
								m[f] = asSet(l)
							}
						}
						rules[i] = m
					}
				}
				u["rules"] = asSet(rules)
			}
			if ar, ok := u["aggregationRule"].(map[string]interface{}); ok {
				if sel, ok := ar["clusterRoleSelectors"].([]interface{}); ok {
					ar["clusterRoleSelectors"] = asSet(sel)
				}
			}
		case "RoleBinding", "ClusterRoleBinding":
			if s, ok := u["subjects"].([]interface{}); ok {
				u["subjects"] = asSet(s)
			}
		}
	}
	// One JSON round trip so numbers and empty values compare the same however they were built.
	raw, err := json.Marshal(u)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	// An empty list and an absent one say the same thing, so a set that collapsed to nothing
	// compares equal to one that was never written.
	for k, v := range out {
		if l, ok := v.([]interface{}); v == nil || (ok && len(l) == 0) {
			delete(out, k)
		}
	}
	return out, nil
}

// foldStringData merges stringData into data on an unstructured Secret, as normalizeSecretStringData
// does for the render (stringData wins on a collision).
func foldStringData(u map[string]interface{}) {
	sd, ok := u["stringData"].(map[string]interface{})
	if !ok {
		return
	}
	data, _ := u["data"].(map[string]interface{})
	if data == nil {
		data = map[string]interface{}{}
	}
	for k, v := range sd {
		if s, ok := v.(string); ok {
			data[k] = encodeStd(s)
		}
	}
	u["data"] = data
	delete(u, "stringData")
}

// asSet sorts l by each element's JSON and drops duplicates.
func asSet(l []interface{}) []interface{} {
	type keyed struct {
		k string
		v interface{}
	}
	seen := map[string]bool{}
	var ks []keyed
	for _, v := range l {
		raw, err := json.Marshal(v)
		if err != nil {
			ks = append(ks, keyed{fmt.Sprint(v), v})
			continue
		}
		k := string(raw)
		if seen[k] {
			continue
		}
		seen[k] = true
		ks = append(ks, keyed{k, v})
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].k < ks[j].k })
	out := make([]interface{}, len(ks))
	for i, e := range ks {
		out[i] = e.v
	}
	return out
}

func encodeStd(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// keptMeta is u's metadata.<field> (annotations or labels) kept to keys, without the absent or null
// ones.
func keptMeta(u map[string]interface{}, field string, keys []string) map[string]interface{} {
	meta, _ := u["metadata"].(map[string]interface{})
	all, _ := meta[field].(map[string]interface{})
	out := map[string]interface{}{}
	for _, k := range keys {
		if v, ok := all[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

// dropNullMembers removes every object member whose value is null from v, at every depth, as
// audit's canonical form c1 leaves them out. A null list element is kept, as c1 writes it.
func dropNullMembers(v interface{}) {
	switch x := v.(type) {
	case map[string]interface{}:
		for k, e := range x {
			if e == nil {
				delete(x, k)
				continue
			}
			dropNullMembers(e)
		}
	case []interface{}:
		for _, e := range x {
			dropNullMembers(e)
		}
	}
}
