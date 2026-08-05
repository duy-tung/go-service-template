package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"

	"github.com/acme/order-engine/internal/grpcclient"
	"github.com/acme/order-engine/internal/testutil/testpg"
)

// trafficRecorder notes which backend served each call.
type trafficRecorder struct {
	mu       sync.Mutex
	sequence []int
}

func (r *trafficRecorder) record(serverID int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence = append(r.sequence, serverID)
}

func (r *trafficRecorder) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.sequence...)
}

func (r *trafficRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence = nil
}

func (r *trafficRecorder) interceptorFor(serverID int) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			r.record(serverID)
			return next(ctx, req)
		}
	})
}

// TestOrderRandomPickerSpreadsAcrossBackends runs two real servers over one
// database and a manual resolver exposing both addresses — the same shape a
// headless Service gives DNS in-cluster. It proves the picker integration,
// not just the algorithm: both backends receive traffic, and the arrival
// sequence shows random repeats rather than round_robin's strict
// alternation.
func TestOrderRandomPickerSpreadsAcrossBackends(t *testing.T) {
	t.Parallel()
	st := newStack(t, nil)
	recorder := &trafficRecorder{}
	server1 := startServer(t, st, recorder.interceptorFor(0))
	server2 := startServer(t, st, recorder.interceptorFor(1))
	testpg.CreateAccount(t, st.db, testAccount, "USD", 1_000_000)

	builder := manual.NewBuilderWithScheme("ordertest")
	builder.InitialState(resolver.State{Addresses: []resolver.Address{
		{Addr: server1.addr},
		{Addr: server2.addr},
	}})
	conn, err := grpcclient.New("ordertest:///order-engine", grpc.WithResolvers(builder))
	if err != nil {
		t.Fatalf("grpcclient.New: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	// Warm-up: keep calling until both subchannels are READY and serving, so
	// the measured window is a true two-backend picker (a one-backend warm-up
	// window would let round_robin produce repeats too).
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := grpcCreate(t, conn, testToken, fmt.Sprintf("warm-%d", time.Now().UnixNano()), 1, "USD"); err != nil {
			t.Fatalf("warm-up call: %v", err)
		}
		seen := map[int]bool{}
		for _, id := range recorder.snapshot() {
			seen[id] = true
		}
		if seen[0] && seen[1] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("both backends never became READY; traffic = %v", recorder.snapshot())
		}
	}

	recorder.reset()
	const calls = 40
	for i := range calls {
		if _, err := grpcCreate(t, conn, testToken, fmt.Sprintf("spread-%d", i), 1, "USD"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	sequence := recorder.snapshot()
	if len(sequence) != calls {
		t.Fatalf("recorded %d calls, want %d", len(sequence), calls)
	}
	counts := map[int]int{}
	repeats := 0
	for i, id := range sequence {
		counts[id]++
		if i > 0 && sequence[i-1] == id {
			repeats++
		}
	}
	if counts[0] == 0 || counts[1] == 0 {
		t.Errorf("traffic distribution %v: every backend must receive calls", counts)
	}
	// Strict alternation (zero repeats over 40 calls) has probability 2^-39
	// under a uniform random picker; observing it means the round_robin
	// fallback took over instead of order_random.
	if repeats == 0 {
		t.Errorf("sequence %v strictly alternates: order_random is not in effect", sequence)
	}
}
