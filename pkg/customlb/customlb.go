// Package customlb registers "order_random", a client-side gRPC
// load-balancing policy that picks a READY backend uniformly at random for
// every RPC. Importing this package (even blank) must happen before a
// service config referencing the policy is parsed, or grpc-go silently
// falls back to the next configured policy.
//
// The balancer/base API is marked experimental upstream, which is why
// google.golang.org/grpc is version-pinned in go.mod.
package customlb

import (
	"math/rand/v2"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
)

// Name is the policy name referenced by service config
// loadBalancingConfig entries.
const Name = "order_random"

func init() {
	balancer.Register(base.NewBalancerBuilder(Name, pickerBuilder{}, base.Config{HealthCheck: false}))
}

type pickerBuilder struct{}

var _ base.PickerBuilder = pickerBuilder{}

// Build snapshots the READY SubConns into an immutable slice. The base
// balancer rebuilds the picker whenever the ready set changes, so the
// snapshot never has to be mutated (or locked) after construction.
func (pickerBuilder) Build(info base.PickerBuildInfo) balancer.Picker {
	if len(info.ReadySCs) == 0 {
		return base.NewErrPicker(balancer.ErrNoSubConnAvailable)
	}
	ready := make([]balancer.SubConn, 0, len(info.ReadySCs))
	for subConn := range info.ReadySCs {
		ready = append(ready, subConn)
	}
	return &randomPicker{ready: ready}
}

type randomPicker struct {
	ready []balancer.SubConn // immutable after Build
}

var _ balancer.Picker = (*randomPicker)(nil)

// Pick implements balancer.Picker. It never blocks and performs no I/O;
// math/rand/v2's top-level functions are safe for concurrent use.
func (p *randomPicker) Pick(balancer.PickInfo) (balancer.PickResult, error) {
	return balancer.PickResult{SubConn: p.ready[rand.IntN(len(p.ready))]}, nil
}
