package connecttransport

import (
	"context"
	"errors"
	"sync"
	"testing"

	"connectrpc.com/connect"

	orderv1 "github.com/acme/order-engine/gen/order/v1"
)

func callThroughAuth(t *testing.T, authorization string) (UserIdentity, bool, error) {
	t.Helper()
	interceptor, err := NewAuthInterceptor(StaticTokenValidator{Token: "token-123", AccountID: "acct-demo"})
	if err != nil {
		t.Fatalf("NewAuthInterceptor: %v", err)
	}

	var (
		identity UserIdentity
		found    bool
	)
	next := connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		identity, found = IdentityFromContext(ctx)
		return connect.NewResponse(&orderv1.CreateOrderResponse{}), nil
	})

	req := connect.NewRequest(&orderv1.CreateOrderRequest{})
	if authorization != "" {
		req.Header().Set("Authorization", authorization)
	}
	_, err = interceptor(next)(context.Background(), req)
	return identity, found, err
}

func TestAuthInterceptorAcceptsValidBearerToken(t *testing.T) {
	identity, found, err := callThroughAuth(t, "Bearer token-123")
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if !found || identity.AccountID != "acct-demo" {
		t.Fatalf("identity = %+v (found=%v), want acct-demo", identity, found)
	}
}

func TestAuthInterceptorAcceptsCaseInsensitiveScheme(t *testing.T) {
	if _, _, err := callThroughAuth(t, "bearer token-123"); err != nil {
		t.Fatalf("lowercase scheme rejected: %v", err)
	}
}

func TestAuthInterceptorRejectsBadCredentials(t *testing.T) {
	cases := []struct {
		name          string
		authorization string
	}{
		{"missing header", ""},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"empty token", "Bearer   "},
		{"token with spaces", "Bearer to ken"},
		{"unknown token", "Bearer token-999"},
		{"scheme only", "Bearer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, found, err := callThroughAuth(t, tc.authorization)
			var connectErr *connect.Error
			if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnauthenticated {
				t.Fatalf("error = %v, want CodeUnauthenticated", err)
			}
			if found {
				t.Fatal("identity must not be injected on failed auth")
			}
		})
	}
}

func TestAuthInterceptorRejectsNilValidator(t *testing.T) {
	if _, err := NewAuthInterceptor(nil); err == nil {
		t.Fatal("nil validator: want error")
	}
}

func TestAuthInterceptorIsConcurrencySafe(t *testing.T) {
	interceptor, err := NewAuthInterceptor(StaticTokenValidator{Token: "token-123", AccountID: "acct-demo"})
	if err != nil {
		t.Fatalf("NewAuthInterceptor: %v", err)
	}
	next := connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if _, ok := IdentityFromContext(ctx); !ok {
			return nil, errors.New("missing identity")
		}
		return connect.NewResponse(&orderv1.CreateOrderResponse{}), nil
	})

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(authorized bool) {
			defer wg.Done()
			req := connect.NewRequest(&orderv1.CreateOrderRequest{})
			if authorized {
				req.Header().Set("Authorization", "Bearer token-123")
			} else {
				req.Header().Set("Authorization", "Bearer nope")
			}
			_, err := interceptor(next)(context.Background(), req)
			if authorized && err != nil {
				t.Errorf("authorized call failed: %v", err)
			}
			if !authorized && err == nil {
				t.Error("unauthorized call succeeded")
			}
		}(i%2 == 0)
	}
	wg.Wait()
}

func TestIdentityFromContextMissing(t *testing.T) {
	if _, ok := IdentityFromContext(context.Background()); ok {
		t.Fatal("empty context must not yield an identity")
	}
}
