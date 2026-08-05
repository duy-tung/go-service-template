// Package grpcclient centralizes the native grpc-go client configuration —
// custom load-balancing policy plus retry service config — shared by
// cmd/client and the integration tests, so what the tests exercise is
// byte-identical to what the demo client ships.
//
// None of this applies to Connect clients: a Connect http.Client performs no
// client-side load balancing or automatic retries.
package grpcclient

import (
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	// Register the order_random balancer before any service config
	// referencing it is parsed; without this import grpc-go silently uses
	// the round_robin fallback.
	_ "github.com/acme/order-engine/pkg/customlb"
)

// DefaultTarget is the in-cluster target: DNS resolution against the
// headless Service yields one address per ready Pod, which is what gives
// the custom picker multiple backends to choose from. Dialing a normal
// ClusterIP Service would collapse the set to one virtual IP and the picker
// could not balance anything.
const DefaultTarget = "dns:///order-engine-headless.order-engine.svc.cluster.local:50051"

// DefaultServiceConfig is the production client policy: order_random with a
// registered round_robin fallback, conservative retry throttling, and a
// retry policy for CreateOrder.
//
// maxAttempts=3 means three total attempts (1 original + up to 2 retries).
// Only UNAVAILABLE is retried — business codes must never be — and retrying
// CreateOrder at all is safe solely because the server implements
// end-to-end idempotency per (account, idempotency_key).
var DefaultServiceConfig = ServiceConfigWithBackoff("0.1s", "1s")

// ServiceConfigWithBackoff renders the service config with custom backoff
// bounds. Integration tests shrink the backoffs to keep the suite fast while
// exercising the exact same policy, retryable codes and throttling.
func ServiceConfigWithBackoff(initialBackoff, maxBackoff string) string {
	return fmt.Sprintf(`{
  "loadBalancingConfig": [
    {"order_random": {}},
    {"round_robin": {}}
  ],
  "retryThrottling": {
    "maxTokens": 10,
    "tokenRatio": 0.1
  },
  "methodConfig": [{
    "name": [{
      "service": "order.v1.OrderService",
      "method": "CreateOrder"
    }],
    "retryPolicy": {
      "maxAttempts": 3,
      "initialBackoff": %q,
      "maxBackoff": %q,
      "backoffMultiplier": 2,
      "retryableStatusCodes": ["UNAVAILABLE"]
    }
  }]
}`, initialBackoff, maxBackoff)
}

// New builds a client connection to target with the default service config.
//
// Transport security: internal traffic is h2c by design (TLS terminates at
// the Gateway), so plaintext credentials are correct for in-cluster targets.
// Any client dialing the public hostname through the Gateway must use TLS
// credentials instead.
//
// grpc.WithDefaultServiceConfig is a fallback: a service config delivered by
// the resolver would take precedence. Forcing this exact config would take
// grpc.WithDisableServiceConfig() at the cost of ignoring server-pushed
// policy; DNS resolvers deliver none, so the fallback is authoritative here
// without giving that flexibility up.
func New(target string, extraOpts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if target == "" {
		return nil, errors.New("grpcclient: target must not be empty")
	}
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(DefaultServiceConfig),
	}, extraOpts...)
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("grpcclient: create client for %s: %w", target, err)
	}
	return conn, nil
}
