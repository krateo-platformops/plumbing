package endpoints

import "testing"

// HasAwsAuth had a DUPLICATED term: its fourth condition repeated AwsSecretKey instead of checking
// AwsService. `A && A` is `A`, so the typo was semantically invisible — nothing failed, no test
// caught it, and it survived until go 1.26's vet turned "redundant and" into a build error.
//
// These arms are a complete differential: each of the four SigV4 inputs is removed in turn and must
// flip the answer to false. A test that only checked the all-present case would have passed against
// the duplicated version, which is exactly how the defect lasted.
func TestHasAwsAuth_EachSigV4InputIsRequired(t *testing.T) {
	complete := func() Endpoint {
		return Endpoint{
			AwsAccessKey: "AKIAEXAMPLE",
			AwsSecretKey: "secret",
			AwsRegion:    "eu-west-1",
			AwsService:   "s3",
		}
	}

	// POSITIVE CONTROL first: if this fails, every negative arm below is uninterpretable, because a
	// predicate that is never true cannot be shown to depend on anything.
	if ep := complete(); !ep.HasAwsAuth() {
		t.Fatalf("a complete AWS configuration reports no AWS auth: %+v", ep)
	}

	for _, tc := range []struct {
		field string
		clear func(*Endpoint)
		why   string
	}{
		{"AwsAccessKey", func(e *Endpoint) { e.AwsAccessKey = "" },
			"it is the Credential= principal in the Authorization header (util.go:166)"},
		{"AwsSecretKey", func(e *Endpoint) { e.AwsSecretKey = "" },
			"it seeds the signing key (util.go:156)"},
		{"AwsRegion", func(e *Endpoint) { e.AwsRegion = "" },
			"it is in the credential scope and the key derivation (util.go:146, :157)"},
		{"AwsService", func(e *Endpoint) { e.AwsService = "" },
			"THE ONE THE DUPLICATE HID: credential scope and key derivation (util.go:146, :158). " +
				"Empty, the scope reads '.../<region>//aws4_request' and AWS answers " +
				"SignatureDoesNotMatch — so reporting true here is a false positive"},
	} {
		t.Run("missing_"+tc.field, func(t *testing.T) {
			ep := complete()
			tc.clear(&ep)
			if ep.HasAwsAuth() {
				t.Errorf("HasAwsAuth() is true with %s empty, but %s", tc.field, tc.why)
			}
		})
	}
}

// The sibling predicates are asserted here too, because the duplicated term was a copy-paste and
// the question "does any OTHER predicate repeat a field" is answered by exercising them rather than
// by reading them. HasCertAuth is the one with the same two-field shape the typo came from.
func TestHasAwsAuth_SiblingPredicatesStillDiscriminate(t *testing.T) {
	t.Run("HasCertAuth needs BOTH cert and key", func(t *testing.T) {
		both := Endpoint{ClientCertificateData: "cert", ClientKeyData: "key"}
		if !both.HasCertAuth() {
			t.Error("cert+key reports no cert auth")
		}
		certOnly := Endpoint{ClientCertificateData: "cert"}
		if certOnly.HasCertAuth() {
			t.Error("cert alone reports cert auth; the key term is not being checked")
		}
		keyOnly := Endpoint{ClientKeyData: "key"}
		if keyOnly.HasCertAuth() {
			t.Error("key alone reports cert auth; the cert term is not being checked")
		}
	})

	t.Run("the three auth modes are mutually distinguishable", func(t *testing.T) {
		// client.go counts these and REFUSES the request when more than one is set
		// ("only one of username/password, bearer token and AWS must be set"), so a predicate that
		// is true when it should not be does not merely mislabel a configuration — it blocks it.
		basic := Endpoint{Password: "p"}
		token := Endpoint{Token: "t"}
		aws := Endpoint{AwsAccessKey: "a", AwsSecretKey: "s", AwsRegion: "r", AwsService: "v"}

		if !basic.HasBasicAuth() || basic.HasTokenAuth() || basic.HasAwsAuth() {
			t.Errorf("a basic-auth endpoint is not cleanly basic-only: %+v", basic)
		}
		if !token.HasTokenAuth() || token.HasBasicAuth() || token.HasAwsAuth() {
			t.Errorf("a token endpoint is not cleanly token-only: %+v", token)
		}
		if !aws.HasAwsAuth() || aws.HasBasicAuth() || aws.HasTokenAuth() {
			t.Errorf("an AWS endpoint is not cleanly AWS-only: %+v", aws)
		}
	})
}
