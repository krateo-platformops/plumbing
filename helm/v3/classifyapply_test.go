package helm

// #276 — a self-heal apply that converges SOME children must not be reported as converging none.
//
// helm's update collects per-object errors and continues: pkg/kube/client.go appends the failure to
// updateErrors, appends the info to res.Updated "regardless" (its own comment), and returns a
// POPULATED Result alongside the joined error. ResourceList.Visit stops only on a non-nil return
// from the visitor, which that path never gives.
//
// Reconcile used to answer that with `return nil, err`. The populated result went on the floor, so
// the change detection never ran, the stored digest never advanced, and a composition reported
// TOTAL failure while most of it was healthy. Observed on a0: six orphaned Jobs from an earlier
// release (immutable spec.template, so unpatchable forever) wedged a Keystone composition
// permanently at Synced=False.
//
// THE ISSUE I FILED FOR THIS WAS WRONG ABOUT THE MECHANISM. It said "because this one error fails
// the whole reconcile, every other child of that composition stops being reconciled too". The
// siblings were always being reconciled — helm attempted every one of them on every pass. What
// broke was the REPORTING and the change detection. Written here because the next reader of that
// issue would otherwise implement a per-object apply loop that duplicates helm.

import (
	"errors"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/kube"
)

func TestClassifyApply(t *testing.T) {
	boom := errors.New(`cannot patch "keystone-bootstrap" with kind Job: ` +
		`Job.batch "keystone-bootstrap" is invalid: spec.template: Invalid value: field is immutable`)

	for _, tc := range []struct {
		name      string
		res       *kube.Result
		err       error
		wantAbort bool
		wantErr   bool
		because   string
	}{
		{
			name:      "clean apply",
			res:       &kube.Result{},
			err:       nil,
			wantAbort: false,
			wantErr:   false,
			because:   "nothing failed, so there is nothing to report",
		},
		{
			name:      "PARTIAL: a result alongside an error must NOT abort",
			res:       &kube.Result{}, // populated-ness is irrelevant here: non-nil is the signal
			err:       boom,
			wantAbort: false,
			wantErr:   true,
			because: "helm patched the children it could; aborting here discards that and reports " +
				"total failure for a composition that is mostly converged (#276)",
		},
		{
			name:      "TOTAL: no result means nothing is known about any child",
			res:       nil,
			err:       boom,
			wantAbort: true,
			wantErr:   true,
			because: "the call failed before producing a result, so there are no diagnostics to " +
				"keep and continuing would compute change detection over nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applyErr, abort := classifyApply(tc.res, tc.err)

			if abort != tc.wantAbort {
				t.Errorf("abort = %v, want %v: %s", abort, tc.wantAbort, tc.because)
			}
			if (applyErr != nil) != tc.wantErr {
				t.Fatalf("applyErr != nil = %v, want %v", applyErr != nil, tc.wantErr)
			}
			if !tc.wantErr {
				return
			}

			// The cause must survive wrapping. Without this, "returns an error" would be satisfied
			// by a generic message, and the children's names — which helm put in the error text and
			// which are the only way an operator learns WHICH child failed — would be lost.
			if !errors.Is(applyErr, tc.err) {
				t.Errorf("the underlying error must be wrapped with %%w, got %v", applyErr)
			}
			if !strings.Contains(applyErr.Error(), "keystone-bootstrap") {
				t.Errorf("the child's name must survive into the message, got %q", applyErr.Error())
			}

			// The two arms must be DISTINGUISHABLE in the message, not merely in the bool. An
			// operator reads the condition, not the return values.
			partialWording := strings.Contains(applyErr.Error(), "some children")
			if tc.wantAbort && partialWording {
				t.Error("a total failure must not be worded as a partial one")
			}
			if !tc.wantAbort && !partialWording {
				t.Error("a partial failure must say so: a message identical to the total-failure " +
					"case leaves the condition as misleading as it was before the fix")
			}
		})
	}
}
