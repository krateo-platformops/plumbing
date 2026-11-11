package usernames

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReserved(t *testing.T) {
	for _, n := range []string{"admin", "administrator", "root", "masterclient", "nodeclient", "aksservice",
		"system", "systemd", "system:node", "kube-proxy", "kubelet", "kubelet-api", "cluster-autoscaler",
		"gke-node", "eks-admin", "aks-agent", "krateo:public", "krateo:anything"} {
		assert.True(t, Reserved(n), n)
	}
	for _, n := range []string{"alice", "admins", "my-admin", "kube", "cluster", "braghettos", "Admin", "krateo", "krateo-public"} {
		assert.False(t, Reserved(n), n)
	}
}

// The lists are returned as copies: a caller can't change the rules.
func TestTheListsAreCopies(t *testing.T) {
	ReservedNames()[0] = "x"
	ReservedPrefixes()[0] = "x"
	assert.True(t, Reserved("admin"))
	assert.True(t, Reserved("system:node"))
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"braghettos", nil},
		{"storun39673", nil},
		{"", ErrEmpty},
		{"Alice", ErrNotIssued},
		{"alice smith", ErrNotIssued},
		{"-alice", ErrNotIssued},
		{"admin", ErrReserved},
		{"kube-proxy", ErrReserved},
		{strings.Repeat("a", MaxLength+1), ErrTooLong},
		{strings.Repeat("a", MaxLength), nil},
	} {
		got := Check(tc.name)
		if tc.want == nil {
			assert.NoError(t, got, tc.name)
			continue
		}
		assert.True(t, errors.Is(got, tc.want), "%q: %v, want %v", tc.name, got, tc.want)
	}
}

// snowplow impersonates "krateo:public" for token-less public widgets, and its
// safety rests on no identity ever being issued that name: Check refuses it,
// and so does Reserved on its own, for a source that issues a name as-is.
func TestThePlatformColonNamesAreNeverIssued(t *testing.T) {
	assert.Error(t, Check("krateo:public"))
	assert.True(t, Reserved("krateo:public"), "reserved even where a source issues names unnormalized (LDAP uid)")
}
