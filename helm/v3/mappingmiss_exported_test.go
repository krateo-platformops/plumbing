package helm

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The detector is exported for core-provider#265, so its contract is now something another
// repository depends on. These arms pin the two properties a consumer needs and the one it must
// NOT infer.
func TestIsRESTMappingMiss_ExportedContract(t *testing.T) {
	typed := &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "kagent.dev", Kind: "RemoteMCPServer"},
		SearchedVersions: []string{"v1alpha2"},
	}

	// NOTE the name: this arm tests that WRAPPING DEPTH does not defeat detection. It does NOT
	// test the typed branch -- the mutant that disabled meta.IsNoMatchError left this arm passing,
	// because NoKindMatchError's own message contains "no matches for kind" and the string
	// fallback sees it through any amount of wrapping. The typed branch is covered by the
	// NoResourceMatchError arm below, which is the only input that isolates it.
	t.Run("wrapping depth does not defeat detection", func(t *testing.T) {
		// The real chain: cli-runtime wraps the typed error with %w, plumbing's
		// postrender_validation wraps that, and helm wraps that again. A consumer sees the
		// outermost error, so the detector must look through all of it.
		wrapped := fmt.Errorf("install failed: %w",
			fmt.Errorf("error while running post render on files: %w",
				fmt.Errorf("failed to build rendered manifests: %w", typed)))
		if !IsRESTMappingMiss(wrapped) {
			t.Error("a %w-wrapped NoKindMatchError must be detected through the full chain, or the " +
				"consumer sees only the stringified fallback and depends on helm's wording")
		}
	})

	// THE ARM THAT MAKES THE TYPED BRANCH LOAD-BEARING. A surviving compiling mutant proved the
	// arm above does NOT test it: NoKindMatchError.Error() itself contains "no matches for kind",
	// so the string fallback catches it and disabling meta.IsNoMatchError changes nothing. Both
	// arms were testing the same mechanism under different labels.
	//
	// NoResourceMatchError is the discriminating input. Its message is "no matches for %v" with no
	// "kind", so it matches NONE of the three fallback substrings and is detected only by the typed
	// check. If that check is ever removed, this is the arm that notices.
	t.Run("typed-only error the string fallback cannot see", func(t *testing.T) {
		resErr := &meta.NoResourceMatchError{
			PartialResource: schema.GroupVersionResource{
				Group: "kagent.dev", Version: "v1alpha2", Resource: "remotemcpservers",
			},
		}
		for _, s := range []string{"no matches for kind", "resource mapping not found", "ensure CRDs are installed first"} {
			if strings.Contains(resErr.Error(), s) {
				t.Fatalf("PREMISE BROKEN: NoResourceMatchError's message %q now contains %q, so the "+
					"string fallback would catch it and this arm no longer isolates the typed check",
					resErr.Error(), s)
			}
		}
		if !IsRESTMappingMiss(fmt.Errorf("failed to build rendered manifests: %w", resErr)) {
			t.Error("a NoResourceMatchError must be detected via the typed check; the string " +
				"fallback cannot see it")
		}
	})

	t.Run("stringified form is detected when the chain is broken", func(t *testing.T) {
		// Not every layer uses %w. The string fallback is what makes the detector work anyway,
		// and it is the half most likely to rot — so it is asserted explicitly rather than
		// assumed to be covered by the typed arm.
		for _, s := range []string{
			`no matches for kind "RemoteMCPServer" in version "kagent.dev/v1alpha2"`,
			`resource mapping not found for name: "x" namespace: "y" from ""`,
			`ensure CRDs are installed first`,
		} {
			if !IsRESTMappingMiss(errors.New(s)) {
				t.Errorf("stringified form not detected: %q", s)
			}
		}
	})

	t.Run("unrelated errors are not misclassified", func(t *testing.T) {
		// The control. A detector that returned true for everything would pass both arms above,
		// and a consumer turning this into an HTTP status would then classify every failure as a
		// blueprint problem.
		for _, e := range []error{
			nil,
			errors.New("connection refused"),
			errors.New("release: not found"),
			errors.New("timed out waiting for the condition"),
			fmt.Errorf("wrapped: %w", errors.New("unauthorized")),
		} {
			if IsRESTMappingMiss(e) {
				t.Errorf("misclassified as a mapping miss: %v", e)
			}
		}
	})

	// THE PROPERTY A CONSUMER MUST NOT INFER, asserted so the doc comment is not the only place
	// it is recorded. A CRD that exists-but-is-uncached and a CRD that is not served produce the
	// SAME error, so the detector cannot tell a transient miss from a permanent one. core-provider
	// #265 turns this into an HTTP status, and it is only entitled to do so because its client is
	// built WithCRDInformer — which resets the mapper on CRD arrival, making a surviving miss
	// evidence of absence. That reasoning lives in the caller; nothing here supplies it.
	t.Run("cannot distinguish transient from permanent", func(t *testing.T) {
		transient := fmt.Errorf("failed to build rendered manifests: %w", &meta.NoKindMatchError{
			GroupKind: schema.GroupKind{Group: "composition.krateo.io", Kind: "JustCreated"},
		})
		permanent := fmt.Errorf("failed to build rendered manifests: %w", &meta.NoKindMatchError{
			GroupKind: schema.GroupKind{Group: "kagent.dev", Kind: "NeverInstalled"},
		})
		if IsRESTMappingMiss(transient) != IsRESTMappingMiss(permanent) {
			t.Fatal("the detector appears to distinguish transient from permanent misses. If that " +
				"is now true the doc comment is wrong and core-provider#265's justification needs " +
				"revisiting; if it is accidental, a consumer will come to rely on it")
		}
	})
}
