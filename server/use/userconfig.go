package use

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/http/response"
	"github.com/krateo-platformops/plumbing/jwtutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

// UserConfig builds a middleware that validates the incoming bearer token.
// keys resolves the RSA public key a token was signed with; in production this
// is a jwtutil.JWKSKeySource pointed at authn's /.well-known/jwks.json, which
// fetches and caches the key set so rotating authn's keypair needs no redeploy
// here.
func UserConfig(keys jwtutil.KeySource, authnNS string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(wri http.ResponseWriter, req *http.Request) {
			if keys == nil {
				response.InternalError(wri, fmt.Errorf("no JWT key source configured"))
				return
			}

			authHeader := req.Header.Get("Authorization")
			if authHeader == "" {
				response.Unauthorized(wri, fmt.Errorf("missing authorization header"))
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
				response.Unauthorized(wri, fmt.Errorf("invalid authorization header format"))
				return
			}

			userInfo, err := jwtutil.ValidateWithKeySource(keys, parts[1])
			if err != nil {
				// The key set being unreachable is our failure, not a bad
				// credential: answer 503 so the client retries instead of
				// re-authenticating against an authn that is simply down.
				if errors.Is(err, jwtutil.ErrKeyUnavailable) {
					response.ServiceUnavailable(wri, err)
					return
				}
				if errors.Is(err, jwtutil.ErrTokenExpired) {
					response.Unauthorized(wri, err)
				} else {
					response.Unauthorized(wri, err)
				}
				return
			}

			sarc, err := rest.InClusterConfig()
			if err != nil {
				response.InternalError(wri, fmt.Errorf("unable to create in cluster config: %w", err))
				return
			}

			ep, err := endpoints.FromSecret(context.Background(), sarc,
				endpoints.SecretName(userInfo.Username), authnNS)
			if err != nil {
				if apierrors.IsNotFound(err) {
					response.Unauthorized(wri, err)
					return
				}
				response.InternalError(wri, err)
				return
			}

			ctx := xcontext.BuildContext(req.Context(),
				xcontext.WithAccessToken(parts[1]),
				xcontext.WithUserInfo(userInfo),
				xcontext.WithUserConfig(ep),
			)

			next.ServeHTTP(wri, req.WithContext(ctx))
		}

		return http.HandlerFunc(fn)
	}
}
