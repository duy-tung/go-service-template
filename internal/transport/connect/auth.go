// Package connecttransport exposes the order service over Connect, gRPC and
// gRPC-Web: handler, authentication interceptor, and error translation.
package connecttransport

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"connectrpc.com/connect"
)

// UserIdentity is the authenticated caller. The account ID always comes from
// verified credentials, never from a request body.
type UserIdentity struct {
	AccountID string
}

// TokenValidator authenticates bearer tokens. Implementations must be safe
// for concurrent use; they are shared across all in-flight requests.
type TokenValidator interface {
	Validate(ctx context.Context, token string) (UserIdentity, error)
}

// identityKey is private so only this package can inject identities.
type identityKey struct{}

// IdentityFromContext returns the identity injected by the auth interceptor.
func IdentityFromContext(ctx context.Context) (UserIdentity, bool) {
	identity, ok := ctx.Value(identityKey{}).(UserIdentity)
	return identity, ok
}

// NewAuthInterceptor returns a unary interceptor that authenticates the
// Authorization header with validator and attaches the resulting identity to
// the request context. Missing or invalid credentials yield
// connect.CodeUnauthenticated.
func NewAuthInterceptor(validator TokenValidator) (connect.UnaryInterceptorFunc, error) {
	if validator == nil {
		return nil, errors.New("connecttransport: NewAuthInterceptor requires a validator")
	}
	interceptor := func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			token, err := bearerToken(req.Header().Get("Authorization"))
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, err)
			}
			identity, err := validator.Validate(ctx, token)
			if err != nil {
				// The validator's reason stays server-side; clients only
				// learn that the credentials were rejected.
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
			}
			return next(context.WithValue(ctx, identityKey{}, identity), req)
		}
	}
	return connect.UnaryInterceptorFunc(interceptor), nil
}

// bearerToken extracts the token from an RFC 6750 Authorization header.
func bearerToken(header string) (string, error) {
	if header == "" {
		return "", errors.New("missing Authorization header")
	}
	const scheme = "Bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", errors.New("Authorization header must use the Bearer scheme")
	}
	token := strings.TrimSpace(header[len(scheme):])
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", errors.New("malformed bearer token")
	}
	return token, nil
}

// StaticTokenValidator accepts a single fixed token and maps it to a fixed
// account. It exists for development and tests only and is NOT production
// authentication; deployments inject a real TokenValidator implementation.
type StaticTokenValidator struct {
	Token     string
	AccountID string
}

// Validate implements TokenValidator.
func (v StaticTokenValidator) Validate(_ context.Context, token string) (UserIdentity, error) {
	if subtle.ConstantTimeCompare([]byte(token), []byte(v.Token)) != 1 {
		return UserIdentity{}, errors.New("unknown token")
	}
	return UserIdentity{AccountID: v.AccountID}, nil
}
