package usernames

import (
	"math/rand"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// The test vectors of acmp's tenant-policy design (§2.4, §2.5), which the
// chart's kyverno test pins too: the two must agree, byte for byte.
func TestBoundedVectors(t *testing.T) {
	a57 := strings.Repeat("a", 57)
	for _, tc := range []struct {
		u, want string
	}{
		{"johndoe", "johndoe"},
		{"john.doe", "john-doe--30f69670bba2"},
		{"a--b", "a--b--90827a2e5636"},
		{a57, strings.Repeat("a", 42) + "--f13b2d724659"},
		// Longer than a label: the chart's kyverno test pins the same vector,
		// so Go and the policy agree on the long path (acmp).
		{strings.Repeat("a", 70), strings.Repeat("a", 42) + "--6bd5e5034855"},
		{"braghettos", "braghettos"},
		{"edmonddantes21", "edmonddantes21"},
		{"cdc-krateo-system-selfservicekrateoes-v0-2-61", "cdc-krateo-system-selfservicekrateoes-v0-2-61"},
		{strings.Repeat("a", 56), strings.Repeat("a", 56)},
	} {
		if got := Bounded(tc.u, 56); got != tc.want {
			t.Errorf("Bounded(%q, 56) = %q, want %q", tc.u, got, tc.want)
		}
	}
	if got := "tenant-" + Bounded(a57, 56); len(got) != 63 {
		t.Errorf("a 57-character name's namespace is %d characters, want 63", len(got))
	}
}

// Over generated issued usernames (DNS-1123 subdomains, the only names authn
// issues, with dots, "--" and lengths around the bound): the result is always
// a label of at most maxLen; a plain-eligible name is kept; plain results
// never contain "--" and hashed ones always do; and no two names meet.
func TestBoundedGenerated(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	inner := "abcxyz0189-."
	seen := map[string]string{}
	for i := 0; i < 50000; i++ {
		n := r.Intn(90) + 1
		var b strings.Builder
		b.WriteByte("abz09"[r.Intn(5)])
		for j := 1; j < n-1; j++ {
			b.WriteByte(inner[r.Intn(len(inner))])
		}
		if n > 1 {
			b.WriteByte("abz09"[r.Intn(5)])
		}
		u := b.String()
		if len(validation.IsDNS1123Subdomain(u)) > 0 {
			continue // not a name authn issues
		}
		for _, maxLen := range []int{BoundedMinLen, 56, 63} {
			got := Bounded(u, maxLen)
			if errs := validation.IsDNS1123Label(got); len(errs) > 0 || len(got) > maxLen {
				t.Fatalf("Bounded(%q, %d) = %q isn't a label of at most %d: %v", u, maxLen, got, maxLen, errs)
			}
			plain := got == u
			eligible := len(u) <= maxLen && !strings.Contains(u, "--") && len(validation.IsDNS1123Label(u)) == 0
			if plain != eligible {
				t.Fatalf("Bounded(%q, %d) = %q: plain %v, eligible %v", u, maxLen, got, plain, eligible)
			}
			if strings.Contains(got, "--") == plain {
				t.Fatalf("Bounded(%q, %d) = %q: the -- marker must mark exactly the hashed results", u, maxLen, got)
			}
			if maxLen == 56 {
				if other, ok := seen[got]; ok && other != u {
					t.Fatalf("Bounded(%q) == Bounded(%q) = %q", u, other, got)
				}
				seen[got] = u
			}
		}
	}
}

// A plain username shaped like another user's hashed form can't take it: the
// marker keeps them apart.
func TestBoundedPlainCantEqualHashed(t *testing.T) {
	hashed := Bounded("john.doe", 56)
	lookalike := strings.Replace(hashed, "--", "-", 1) // john-doe-30f69670bba2, a plain username
	if Bounded(lookalike, 56) != lookalike {
		t.Fatalf("the premise: %q is plain", lookalike)
	}
	if Bounded(lookalike, 56) == hashed {
		t.Errorf("a plain username equals john.doe's hashed form")
	}
}

func TestBoundedRejectsABadBound(t *testing.T) {
	for _, maxLen := range []int{0, BoundedMinLen - 1, 64} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Bounded with maxLen %d didn't panic", maxLen)
				}
			}()
			Bounded("alice", maxLen)
		}()
	}
}
