package dynamicwatch

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The metadata-only variant's whole point is the object type that selects the informer. A fake
// controller cannot see it, so it is asserted here directly.
func TestWatchObjectSelectsTheInformer(t *testing.T) {
	full, ok := watchObject(watchKey{gvk: testGVK}).(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("a full watch must use *unstructured.Unstructured, got %T", watchObject(watchKey{gvk: testGVK}))
	}
	if full.GroupVersionKind() != testGVK {
		t.Errorf("full watch object GVK = %v, want %v", full.GroupVersionKind(), testGVK)
	}
	meta, ok := watchObject(watchKey{gvk: testGVK, metadataOnly: true}).(*metav1.PartialObjectMetadata)
	if !ok {
		t.Fatalf("a metadata watch must use *metav1.PartialObjectMetadata, got %T", watchObject(watchKey{gvk: testGVK, metadataOnly: true}))
	}
	if meta.GroupVersionKind() != testGVK {
		t.Errorf("metadata watch object GVK = %v, want %v", meta.GroupVersionKind(), testGVK)
	}
}

// A Kind watched both ways is two informers: neither registration may suppress the other, and each is
// still deduped against itself.
func TestMetadataAndFullWatchesAreDedupedSeparately(t *testing.T) {
	r := NewRegistry(nil)
	fc := &fakeController{}
	for i := 0; i < 2; i++ {
		if err := r.EnsureMetadataWatch(fc, testGVK, noopMapFunc); err != nil {
			t.Fatal(err)
		}
	}
	if fc.calls() != 1 || !r.WatchingMetadata(testGVK) || r.Watching(testGVK) {
		t.Fatalf("after two metadata registrations: %d Watch calls, metadata=%v full=%v; want 1, true, false",
			fc.calls(), r.WatchingMetadata(testGVK), r.Watching(testGVK))
	}
	if err := r.EnsureWatch(fc, testGVK, noopMapFunc); err != nil {
		t.Fatal(err)
	}
	if fc.calls() != 2 || !r.Watching(testGVK) {
		t.Fatalf("a full watch after a metadata one must register too: %d Watch calls, full=%v", fc.calls(), r.Watching(testGVK))
	}
}
