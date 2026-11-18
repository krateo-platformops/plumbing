package helm

import (
	"errors"
	"fmt"
	"strings"
	"text/template"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// helm's replacement text for a `fail`/`required` (pkg/engine/engine.go, cleanupExecError): the
// "at" form with a location, and the "in" form when the location could not be split out. The "in"
// form is kept for fidelity to helm but is not reachable through a real render today:
// text/template's errorf always yields at least three ": "-separated tokens, so a mutant dropping it
// survives the real-helm test, and that is expected.
var templateExecutionPrefixes = []string{"execution error at (", "execution error in ("}

// classifyTemplateExecution wraps err in helmconfig.ErrTemplateExecution when it is a failure inside
// the chart's templates (see ErrTemplateExecution for the two forms helm returns), and returns every
// other error unchanged. Both wrapped errors stay in the chain (%w twice), so a caller still sees
// the original with errors.As.
//
// err MUST BE helm's own error, BEFORE plumbing wraps it ("install failed: ..."). helm returns the
// render error unwrapped (action/install.go:323-327, upgrade.go:166-167 and 277-278, renderResources
// in action.go:145-146), so helm's prefix is at byte 0 and is matched with HasPrefix. A Contains over
// a wrapped chain would also match the phrase QUOTED inside an unrelated apiserver or webhook error
// and turn it into a 400 TemplateRefused: #583's misattribution in reverse (review by authn).
func classifyTemplateExecution(err error) error {
	if err == nil || errors.Is(err, helmconfig.ErrTemplateExecution) {
		return err
	}
	var execErr template.ExecError
	if errors.As(err, &execErr) {
		return fmt.Errorf("%w: %w", helmconfig.ErrTemplateExecution, err)
	}
	msg := err.Error()
	for _, p := range templateExecutionPrefixes {
		if strings.HasPrefix(msg, p) {
			return fmt.Errorf("%w: %w", helmconfig.ErrTemplateExecution, err)
		}
	}
	return err
}
