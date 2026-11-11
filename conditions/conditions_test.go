package conditions

import "testing"

// The values are the contract (see the package comment), so the test pins the bytes rather than
// re-deriving them. A rename must fail here and be made together with core-provider,
// oasgen-provider and every consumer that matches the string.
func TestContractStrings(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{TypeControllerActive, "ControllerActive"},
		{ReasonScaledToZero, "ScaledToZero"},
		{ReasonRunning, "Running"},
	} {
		if c.got != c.want {
			t.Errorf("contract string changed: %q, want %q", c.got, c.want)
		}
	}
}
