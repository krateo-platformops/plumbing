package helm

import (
	"errors"
	"fmt"
	"strings"
	"text/template"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// helm's replacement text for a `fail`/`required` (pkg/engine/engine.go, cleanupExecError): the
// "at" form with a location, and the "in" form when the location could not be split out.
var templateExecutionPrefixes = []string{"execution error at (", "execution error in ("}

// classifyTemplateExecution wraps err in helmconfig.ErrTemplateExecution when it is a failure inside
// the chart's templates (see ErrTemplateExecution for the two forms helm returns), and returns every
// other error unchanged. Both wrapped errors stay in the chain (%w twice), so a caller still sees
// the original with errors.As.
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
		if strings.Contains(msg, p) {
			return fmt.Errorf("%w: %w", helmconfig.ErrTemplateExecution, err)
		}
	}
	return err
}
