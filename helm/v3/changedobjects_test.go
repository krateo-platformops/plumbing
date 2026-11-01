package helm

import (
	"fmt"
	"testing"
)

// THE CASE THIS EXISTS FOR (core-provider#175). The counts said THAT something drifted; nothing said
// WHICH child. Diagnosing one real case took a cluster-wide differential across five compositions to
// establish a fact this list states outright.
func TestChangedObjectSet_NamesWhatChanged(t *testing.T) {
	c := newChangedObjectSet(10)
	c.add("compute.cnrm.cloud.google.com/v1beta1/ComputeInstance/demo-system/meetup-computeinstance")
	c.add("apps/v1/Deployment/demo-system/meetup-deployment")

	if len(c.names) != 2 {
		t.Fatalf("both children must be named, got %v", c.names)
	}
	if c.omitted != 0 {
		t.Errorf("nothing was dropped, omitted = %d", c.omitted)
	}
}

// THE CAP, which is part of the design rather than tidiness: the composition that prompted #175
// writes two helm revisions per minute, so an uncapped list would make this diagnostic into the same
// log-volume problem the issue is about.
func TestChangedObjectSet_CapsAndCountsTheRemainder(t *testing.T) {
	c := newChangedObjectSet(3)
	for i := 0; i < 7; i++ {
		c.add(fmt.Sprintf("v1/ConfigMap/ns/cm-%d", i))
	}

	if len(c.names) != 3 {
		t.Errorf("the cap must hold, got %d names: %v", len(c.names), c.names)
	}
	// The count is what lets a reader tell a genuinely short list from a truncated one. Without it,
	// three names is indistinguishable from "only three children drifted".
	if c.omitted != 4 {
		t.Errorf("omitted = %d, want 4 — a truncated list must say how much it dropped", c.omitted)
	}
}

// A child can be reached from more than one of the created/deleted/patched lists (a recreate), and
// naming it twice would suggest two children drifted when one did.
func TestChangedObjectSet_DeduplicatesWithoutInflatingOmitted(t *testing.T) {
	c := newChangedObjectSet(2)
	key := "v1/Namespace//demo-system"
	c.add(key)
	c.add(key)
	c.add(key)

	if len(c.names) != 1 {
		t.Errorf("a repeated identity must appear once, got %v", c.names)
	}
	// A duplicate is NOT something the reader is missing, so it must not be counted as omitted —
	// otherwise a recreate reports "1 shown, 2 omitted" for a single child.
	if c.omitted != 0 {
		t.Errorf("a duplicate is not an omission, omitted = %d", c.omitted)
	}
}

// Dedup must not silently consume cap space either: three distinct children under a cap of two
// should report one omitted, not two.
func TestChangedObjectSet_DedupDoesNotDistortTheCap(t *testing.T) {
	c := newChangedObjectSet(2)
	c.add("v1/ConfigMap/ns/a")
	c.add("v1/ConfigMap/ns/a") // duplicate
	c.add("v1/ConfigMap/ns/b")
	c.add("v1/ConfigMap/ns/c")

	if len(c.names) != 2 {
		t.Errorf("names = %v, want 2", c.names)
	}
	if c.omitted != 1 {
		t.Errorf("omitted = %d, want 1 — only `c` was dropped", c.omitted)
	}
}

// An empty key is not a child. infoKey builds its string from the object's GVK, namespace and name,
// so an empty result means the identity could not be formed — recording it would put a bare "///"
// in the log where a reader expects a resource.
func TestChangedObjectSet_IgnoresEmptyKeys(t *testing.T) {
	c := newChangedObjectSet(5)
	c.add("")
	c.add("v1/ConfigMap/ns/real")
	c.add("")

	if len(c.names) != 1 || c.names[0] != "v1/ConfigMap/ns/real" {
		t.Errorf("only the real identity may be recorded, got %v", c.names)
	}
	if c.omitted != 0 {
		t.Errorf("an empty key is not an omission, omitted = %d", c.omitted)
	}
}

// A set that collected nothing must present as nothing — a nil slice and a zero count, so a
// no-drift reconcile does not log an empty list as though it were a finding.
func TestChangedObjectSet_EmptyStaysEmpty(t *testing.T) {
	c := newChangedObjectSet(10)
	if len(c.names) != 0 || c.omitted != 0 {
		t.Errorf("a fresh set must be empty, got names=%v omitted=%d", c.names, c.omitted)
	}
}

// The cap constant itself: a zero or negative limit would silently record nothing while counting
// everything as omitted, which reads as "many children drifted, none nameable".
func TestMaxChangedObjects_IsAUsefulCap(t *testing.T) {
	if maxChangedObjects < 1 {
		t.Fatalf("maxChangedObjects = %d; a cap below 1 names no child at all", maxChangedObjects)
	}
	if maxChangedObjects > 50 {
		t.Errorf("maxChangedObjects = %d; the cap exists to bound a line logged twice a minute",
			maxChangedObjects)
	}
}
