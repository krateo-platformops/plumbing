package request

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/http/response"
	"github.com/krateo-platformops/plumbing/http/util"
	"github.com/krateo-platformops/plumbing/ptr"
)

const maxUnstructuredResponseTextBytes = 2048

type RequestOptions struct {
	RequestInfo
	Endpoint        *endpoints.Endpoint
	ResponseHandler func(io.ReadCloser) error
	ErrorKey        string
	ContinueOnError bool
}

type RequestInfo struct {
	Path    string
	Verb    *string
	Headers []string
	Payload *string
}

func Do(ctx context.Context, opts RequestOptions) *response.Status {
	uri := strings.TrimSuffix(opts.Endpoint.ServerURL, "/")
	if len(opts.Path) > 0 {
		uri = fmt.Sprintf("%s/%s", uri, strings.TrimPrefix(opts.Path, "/"))
	}

	u, err := url.Parse(uri)
	if err != nil {
		return response.New(http.StatusInternalServerError, err)
	}

	verb := ptr.Deref(opts.Verb, http.MethodGet)

	var body io.Reader
	if s := ptr.Deref(opts.Payload, ""); len(s) > 0 {
		body = strings.NewReader(s)
	}

	call, err := http.NewRequestWithContext(ctx, verb, u.String(), body)
	if err != nil {
		return response.New(http.StatusInternalServerError, err)
	}
	// Additional headers for AWS Signature 4 algorithm
	if opts.Endpoint.HasAwsAuth() {
		headers, _, _, _, _, _ := ComputeAwsHeaders(opts.Endpoint, &opts.RequestInfo)
		opts.Headers = append(opts.Headers, headers...)
		opts.Headers = append(opts.Headers, xcontext.LabelKrateoTraceId+":"+xcontext.TraceId(ctx, true))
		// Set all headers to lower case for AWS signature
		for i := range opts.Headers {
			hParts := strings.Split(opts.Headers[i], ":")
			opts.Headers[i] = strings.ToLower(strings.Trim(hParts[0], " ")) + ":" + strings.Trim(hParts[1], " ")
		}
		sort.Strings(opts.Headers)
	} else {
		call.Header.Set(xcontext.LabelKrateoTraceId, xcontext.TraceId(ctx, true))
	}

	if len(opts.Headers) > 0 {
		for _, el := range opts.Headers {
			idx := strings.Index(el, ":")
			if idx <= 0 {
				continue
			}
			key := el[:idx]
			val := strings.TrimSpace(el[idx+1:])
			call.Header.Set(key, val)
		}
	}

	cli, err := HTTPClientForEndpoint(opts.Endpoint, &opts.RequestInfo)
	if err != nil {
		return response.New(http.StatusInternalServerError,
			fmt.Errorf("unable to create HTTP Client for endpoint: %w", err))
	}

	// Wrap the existing client in a RetryClient
	retryCli := util.NewRetryClient(cli)

	// Use RetryClient instead of the raw client  cli.Do(call)
	respo, err := retryCli.Do(call)
	if err != nil {
		return response.New(http.StatusInternalServerError, err)
	}
	defer respo.Body.Close()

	statusOK := respo.StatusCode >= 200 && respo.StatusCode < 300
	if !statusOK {
		// One byte past the limit tells a body that was cut from one that
		// fits; only the limit's worth is ever kept.
		dat, err := io.ReadAll(io.LimitReader(respo.Body, maxUnstructuredResponseTextBytes+1))
		if err != nil {
			return response.New(http.StatusInternalServerError, err)
		}
		truncated := len(dat) > maxUnstructuredResponseTextBytes
		if truncated {
			dat = dat[:maxUnstructuredResponseTextBytes]
		}

		// The upstream's error envelope, read as Do has always read it: its
		// kind, apiVersion, status, message, reason and code, and nothing
		// else. response.Status has since gained Details; an upstream's
		// "details" (an apiserver's carries the object's name and uid) is not
		// part of what Do returns, so it is taken here by a field that
		// shadows Status.Details, accepts any JSON value, and is dropped.
		// Without it a body with details would start returning them, and a
		// body whose "details" is not an object would stop decoding.
		var envelope struct {
			response.Status
			Details json.RawMessage `json:"details,omitempty"`
		}
		// A body is a Status only if it decodes AND carries a code: valid
		// JSON that isn't one ({"error":"boom"}, null) would otherwise come
		// back as an empty Status with code 0, which reads like success
		// (plumbing#174). It is reported like any other body that isn't a
		// Status, under the response's own code.
		if err := json.Unmarshal(dat, &envelope); err != nil || envelope.Code == 0 {
			return response.New(respo.StatusCode, errors.New(upstreamErrorText(respo.StatusCode, dat, truncated)))
		}

		return &envelope.Status
	}

	if ct := respo.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		return response.New(http.StatusNotAcceptable, fmt.Errorf("content type %q is not allowed", ct))
	}

	if opts.ResponseHandler != nil {
		if err := opts.ResponseHandler(respo.Body); err != nil {
			return response.New(http.StatusInternalServerError, err)
		}
		return response.New(http.StatusOK, nil)
	}

	return response.New(http.StatusNoContent, nil)
}

// upstreamErrorText is the message for an upstream's error body that isn't a
// Status (plumbing#175). A body that is JSON is never echoed raw: an upstream's
// JSON can carry what Do drops from a Status, such as an object's uid, and
// anything else beside its message. A JSON object answers its "message" when
// that is a non-empty string ({"message":"Bad credentials"}, which makes a
// third-party failure fixable); every other JSON body answers
// "upstream answered <code>". A body that isn't JSON (plain text, HTML) is the
// upstream's own words and is echoed, already cut to the size limit. A body cut
// at the limit no longer parses, so one that starts like JSON is taken for
// JSON: an object or array cut short still carries what it carried.
func upstreamErrorText(code int, body []byte, truncated bool) string {
	trimmed := bytes.TrimSpace(body)
	isJSON := json.Valid(trimmed) ||
		truncated && len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
	if !isJSON {
		return string(body)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(trimmed, &object) == nil {
		var message string
		if raw, ok := object["message"]; ok && json.Unmarshal(raw, &message) == nil && message != "" {
			return message
		}
	}
	return fmt.Sprintf("upstream answered %d", code)
}
