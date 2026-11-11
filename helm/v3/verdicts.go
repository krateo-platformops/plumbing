package helm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/kube"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
)

// verdictInputs is what the self-heal merge left behind, as Reconcile holds it after step 5.
//
// target holds the children handed to the merge, each Object refreshed to its post-merge state;
// live holds their state before the merge (absent ones are missing from it). created and deleted
// are helm's result. kept and notDone are the infoKeys of intended deletions that helm declined
// (resource-policy keep) or did not perform, and current resolves them to their Infos.
type verdictInputs struct {
	target, created, deleted, current kube.ResourceList
	live                              map[string]runtime.Object
	absent                            map[string]bool
	kept, notDone                     []string
	force                             bool
}

// objectVerdicts builds the uncapped per-child verdict list (plumbing#106).
//
// A surviving child is listed exactly when Reconcile counts it under PatchedUpdated: its stripped
// state changed across the merge, or the comparison failed. So the verdicts and the counts can't
// disagree: created, deleted and patched equal the number of created, deleted and updated
// verdicts.
//
// EXTRACTED for the same reason as classifyApply and keptDeletions: a decision reachable only
// through a full Reconcile is a decision nobody tests.
func objectVerdicts(in verdictInputs) []helmconfig.ObjectVerdict {
	updateVerb := "patch"
	if in.force {
		updateVerb = "update" // helm replaces (PUT) under Force
	}
	var out []helmconfig.ObjectVerdict
	for _, info := range in.created {
		out = append(out, verdictOf(info, info.Object, "create", helmconfig.ChangeCreated))
	}
	for _, info := range in.deleted {
		out = append(out, verdictOf(info, info.Object, "delete", helmconfig.ChangeDeleted))
	}
	for _, info := range in.target {
		key := infoKey(info)
		if in.absent[key] {
			continue // listed from created
		}
		before := in.live[key]
		changed, err := semanticallyChanged(before, info.Object)
		if err != nil {
			out = append(out, verdictOf(info, info.Object, updateVerb, helmconfig.ChangeCompareFailed))
			continue
		}
		if !changed {
			continue
		}
		contentChanged, err := contentDiffers(before, info.Object)
		switch {
		case err != nil:
			out = append(out, verdictOf(info, info.Object, updateVerb, helmconfig.ChangeCompareFailed))
		case contentChanged:
			out = append(out, verdictOf(info, info.Object, updateVerb, helmconfig.ChangeSpec))
		default:
			out = append(out, verdictOf(info, info.Object, updateVerb, helmconfig.ChangeMetadataOnly))
		}
	}
	byKey := make(map[string]*resource.Info, len(in.current))
	for _, info := range in.current {
		byKey[infoKey(info)] = info
	}
	for _, k := range in.kept {
		if info := byKey[k]; info != nil {
			out = append(out, verdictOf(info, info.Object, "delete", helmconfig.ChangeKept))
		}
	}
	for _, k := range in.notDone {
		if info := byKey[k]; info != nil {
			out = append(out, verdictOf(info, info.Object, "delete", helmconfig.ChangeDeleteNotDone))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return verdictKey(out[i]) < verdictKey(out[j]) })
	return out
}

func verdictKey(v helmconfig.ObjectVerdict) string {
	return v.Group + "/" + v.Version + "/" + v.Resource + "/" + v.Namespace + "/" + v.Name + "/" + string(v.Change)
}

// verdictOf names info's child. The resource comes from the REST mapping helm used to send the
// request; the uid from obj when it carries one.
func verdictOf(info *resource.Info, obj runtime.Object, verb string, change helmconfig.ObjectChange) helmconfig.ObjectVerdict {
	var gvk schema.GroupVersionKind
	if obj != nil {
		gvk = obj.GetObjectKind().GroupVersionKind()
	}
	v := helmconfig.ObjectVerdict{
		Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind,
		Namespace: info.Namespace, Name: info.Name, Verb: verb, Change: change,
	}
	if info.Mapping != nil {
		v.Group, v.Version, v.Resource = info.Mapping.Resource.Group, info.Mapping.Resource.Version, info.Mapping.Resource.Resource
		if v.Kind == "" {
			v.Kind = info.Mapping.GroupVersionKind.Kind
		}
	}
	if obj != nil {
		if acc, err := apimeta.Accessor(obj); err == nil {
			v.UID = string(acc.GetUID())
		}
	}
	return v
}

// contentDiffers reports whether before and after differ in content: every top-level field other
// than apiVersion, kind, metadata and status, compared canonically (see canonicalContent).
func contentDiffers(before, after runtime.Object) (bool, error) {
	b, err := canonicalContent(before)
	if err != nil {
		return false, fmt.Errorf("before: %w", err)
	}
	a, err := canonicalContent(after)
	if err != nil {
		return false, fmt.Errorf("after: %w", err)
	}
	return !reflect.DeepEqual(a, b), nil
}

// canonicalContent is obj's content in the form the AUDIT-4 ruling compares (snowplow, 2026-11-11):
//
// DELIBERATE, not a shortcut: krateo-platformops/installer#413 condition 2 says ".spec changed",
// and the AUDIT-4 lead ruled (2026-11-11, recorded on installer#410) that this means every
// top-level field other than apiVersion, kind, metadata and status. Many kinds keep their desired
// state outside .spec (ConfigMap and Secret data, RBAC rules and subjects, admission webhooks), and
// reading the condition literally would make a change to them never count.
//
//   - every top-level field except apiVersion, kind, metadata and status. That is .spec where there
//     is one; data and type for a Secret or ConfigMap; webhooks for an admission configuration.
//   - a Secret's stringData folded into data, as the apiserver does, so the same value written
//     either way is the same content. A changed type is a change.
//   - RBAC: a Role's or ClusterRole's rules, and a binding's subjects, as sets: order and
//     duplicates don't matter, and within a rule neither do the order or duplicates of its verbs,
//     apiGroups, resources, resourceNames and nonResourceURLs. A ClusterRole's
//     aggregationRule.clusterRoleSelectors compare as a set too. roleRef compares as it is.
//
// Labels and annotations are metadata, so they never count here.
func canonicalContent(obj runtime.Object) (map[string]interface{}, error) {
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
	for _, k := range []string{"apiVersion", "kind", "metadata", "status"} {
		delete(u, k)
	}
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
