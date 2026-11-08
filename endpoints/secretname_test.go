package endpoints

import "testing"

// SecretName is the one definition of a session Secret's name: the username
// made DNS-1123 compatible, plus "-clientconfig".
func TestSecretName(t *testing.T) {
	for username, want := range map[string]string{
		"alice":             "alice-clientconfig",
		"cdc-krateo-sa":     "cdc-krateo-sa-clientconfig",
		"Alice.Smith@x.com": "alicesmithxcom-clientconfig",
	} {
		if got := SecretName(username); got != want {
			t.Errorf("SecretName(%q) = %q, want %q", username, got, want)
		}
	}
}
