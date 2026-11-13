package kubeconfig

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"k8s.io/client-go/transport"
)

// newDebuggingRoundTripper logs each request a client config sends, at DEBUG, as METADATA ONLY:
// method, host, path, status (or the error) and duration. It never logs a header, a query or a body.
//
// It sits inside client-go's credential wrappers (rest.Config.Wrap is applied under the bearer and
// basic-auth round trippers), so the request it sees carries `Authorization: Bearer <token>`. A wire
// dump here (httputil.DumpRequestOut, which writes the headers whatever its body flag says) put the
// endpoint's credential into the log at DEBUG; reading only these fields leaves nothing to redact
// and nothing to forget.
func newDebuggingRoundTripper(log *slog.Logger, traceId string) transport.WrapperFunc {
	return func(rt http.RoundTripper) http.RoundTripper {
		return &debuggingRoundTripper{
			delegatedRoundTripper: rt,
			traceId:               traceId,
			log:                   log,
		}
	}
}

type debuggingRoundTripper struct {
	delegatedRoundTripper http.RoundTripper
	traceId               string
	log                   *slog.Logger
}

// WrappedRoundTripper lets client-go walk the chain.
func (rt *debuggingRoundTripper) WrappedRoundTripper() http.RoundTripper {
	return rt.delegatedRoundTripper
}

func (rt *debuggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	traceId := req.Header.Get(xcontext.LabelKrateoTraceId)
	if len(traceId) == 0 {
		traceId = rt.traceId // xcontext.TraceId(req.Context(), true)
	}
	req.Header.Set(xcontext.LabelKrateoTraceId, traceId)

	start := time.Now()
	resp, err := rt.delegatedRoundTripper.RoundTrip(req)
	attrs := []any{
		slog.String("method", req.Method),
		slog.String("host", req.URL.Host),
		slog.String("path", req.URL.Path),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	}
	if err != nil {
		rt.log.Debug("kubeconfig: request failed", append(attrs, slog.String("error_type", errorType(err)))...)
		return resp, err
	}
	rt.log.Debug("kubeconfig: request", append(attrs, slog.Int("status", resp.StatusCode))...)
	return resp, nil
}

// errorType names an error by its Go type, never its text: a transport error's text can quote the
// request (a URL with a query, a header).
func errorType(err error) string {
	return fmt.Sprintf("%T", err)
}
