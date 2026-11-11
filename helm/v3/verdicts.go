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
// live holds their state before the merge (absent ones are missing from it). created, updated and
// deleted are helm's result.
//
// helm's result alone can't tell a failed write from a successful one: it appends a child to
// created BEFORE sending the create ("even if something fails") and every applied child to updated
// "regardless" of its patch's outcome (helm v3 pkg/kube/client.go, update). What does tell them
// apart is the object: helm refreshes it from the server (Refresh after a create or patch, Get
// after an empty patch) only when that succeeded. A rendered object never carries a
// resourceVersion, so a child of created or updated still without one is a failed write. kept, notDone and unknown are the infoKeys of intended deletions that helm
// declined (resource-policy keep), did not perform, or whose state could not be read
// (pendingDeletions), and current resolves them to their Infos.
type verdictInputs struct {
	target, created, updated, deleted, current kube.ResourceList
	live                                       map[string]runtime.Object
	absent                                     map[string]bool
	kept, notDone, unknown                     []string
	force                                      bool
	// contentAnnotations are the caller's UpgradeConfig.ContentAnnotations.
	contentAnnotations []string
}

// objectVerdicts builds the uncapped per-child verdict list (plumbing#106).
//
// A surviving child whose write helm confirmed is listed exactly when Reconcile counts it under
// PatchedUpdated: its stripped state changed across the merge, or the comparison failed. Both use
// writeConfirmed, so the verdicts and the counts agree by construction, a partial apply included:
// Created, Deleted and PatchedUpdated equal the created, deleted and other surviving-child
// verdicts other than the failed ones (createFailed, updateFailed), which no count includes.
//
// EXTRACTED for the same reason as classifyApply and keptDeletions: a decision reachable only
// through a full Reconcile is a decision nobody tests.
//
// complete is false when an intended deletion's key resolves to no Info in current, which the
// lists it came from should make impossible; the verdict is then missing, and the caller is told
// rather than left to read the gap as "nothing happened".
//
// notAttempted counts the targets helm never reached (in neither created nor updated).
func objectVerdicts(in verdictInputs) (verdicts []helmconfig.ObjectVerdict, complete bool, notAttempted int) {
	updateVerb := "patch"
	if in.force {
		updateVerb = "update" // helm replaces (PUT) under Force
	}
	var out []helmconfig.ObjectVerdict
	for _, info := range in.created {
		if !writeConfirmed(info) {
			out = append(out, verdictOf(info, info.Object, "create", helmconfig.ChangeCreateFailed))
			continue
		}
		out = append(out, verdictOf(info, info.Object, "create", helmconfig.ChangeCreated))
	}
	createdKeys := make(map[string]bool, len(in.created))
	for _, info := range in.created {
		createdKeys[infoKey(info)] = true
	}
	applied := make(map[string]bool, len(in.updated))
	for _, info := range in.updated {
		applied[infoKey(info)] = true
	}
	for _, info := range in.deleted {
		out = append(out, verdictOf(info, info.Object, "delete", helmconfig.ChangeDeleted))
	}
	for _, info := range in.target {
		key := infoKey(info)
		if in.absent[key] {
			if !createdKeys[key] {
				notAttempted++
			}
			continue // listed from created
		}
		before := in.live[key]
		if !writeConfirmed(info) {
			if applied[key] {
				// The uid is the live object's: the failed write left this one as rendered.
				out = append(out, verdictOf(info, before, updateVerb, helmconfig.ChangeUpdateFailed))
			} else {
				// Not applied: helm stopped before reaching it, so nothing was sent.
				notAttempted++
			}
			continue
		}
		changed, err := semanticallyChanged(before, info.Object)
		if err != nil {
			out = append(out, verdictOf(info, info.Object, updateVerb, helmconfig.ChangeCompareFailed))
			continue
		}
		if !changed {
			continue
		}
		contentChanged, err := contentDiffers(before, info.Object, in.contentAnnotations)
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
	complete = true
	for _, d := range []struct {
		keys   []string
		change helmconfig.ObjectChange
	}{
		{in.kept, helmconfig.ChangeKept},
		{in.notDone, helmconfig.ChangeDeleteNotDone},
		{in.unknown, helmconfig.ChangeDeleteUnknown},
	} {
		for _, k := range d.keys {
			info := byKey[k]
			if info == nil {
				complete = false
				continue
			}
			out = append(out, verdictOf(info, info.Object, "delete", d.change))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return verdictKey(out[i]) < verdictKey(out[j]) })
	return out, complete, notAttempted
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
// than apiVersion, kind, metadata and status, and the annotations listed in contentAnnotations,
// compared canonically (see canonicalContent).
func contentDiffers(before, after runtime.Object, contentAnnotations []string) (bool, error) {
	b, err := canonicalContent(before, contentAnnotations)
	if err != nil {
		return false, fmt.Errorf("before: %w", err)
	}
	a, err := canonicalContent(after, contentAnnotations)
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
//
//   - a Secret's stringData folded into data, as the apiserver does, so the same value written
//     either way is the same content. A changed type is a change.
//
//   - RBAC: a Role's or ClusterRole's rules, and a binding's subjects, as sets: order and
//     duplicates don't matter, and within a rule neither do the order or duplicates of its verbs,
//     apiGroups, resources, resourceNames and nonResourceURLs. A ClusterRole's
//     aggregationRule.clusterRoleSelectors compare as a set too. roleRef compares as it is.
//
//   - the annotations listed in contentAnnotations (UpgradeConfig.ContentAnnotations: the audit
//     schema's content_annotations, which authorization reads), under metadata.annotations, as
//     audit.ContentSHA256 keeps them: each one present with a non-null value, the rest dropped.
//
// Labels and every other annotation are metadata, so they never count here.
func canonicalContent(obj runtime.Object, contentAnnotations []string) (map[string]interface{}, error) {
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
	kept := keptAnnotations(u, contentAnnotations)
	for _, k := range []string{"apiVersion", "kind", "metadata", "status"} {
		delete(u, k)
	}
	if len(kept) > 0 {
		u["metadata"] = map[string]interface{}{"annotations": kept}
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

// keptAnnotations is u's metadata.annotations kept to keys, without the absent or null ones.
func keptAnnotations(u map[string]interface{}, keys []string) map[string]interface{} {
	meta, _ := u["metadata"].(map[string]interface{})
	all, _ := meta["annotations"].(map[string]interface{})
	out := map[string]interface{}{}
	for _, k := range keys {
		if v, ok := all[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}
