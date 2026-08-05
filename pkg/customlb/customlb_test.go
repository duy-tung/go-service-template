package customlb

import (
	"errors"
	"sync"
	"testing"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
)

type fakeSubConn struct {
	balancer.SubConn
	id int
}

func buildPicker(t *testing.T, n int) (balancer.Picker, []*fakeSubConn) {
	t.Helper()
	ready := make(map[balancer.SubConn]base.SubConnInfo, n)
	conns := make([]*fakeSubConn, 0, n)
	for i := range n {
		subConn := &fakeSubConn{id: i}
		conns = append(conns, subConn)
		ready[subConn] = base.SubConnInfo{}
	}
	return pickerBuilder{}.Build(base.PickerBuildInfo{ReadySCs: ready}), conns
}

func TestPolicyIsRegistered(t *testing.T) {
	if balancer.Get(Name) == nil {
		t.Fatalf("balancer %q is not registered", Name)
	}
}

func TestEmptySnapshotReturnsErrNoSubConnAvailable(t *testing.T) {
	picker, _ := buildPicker(t, 0)
	_, err := picker.Pick(balancer.PickInfo{})
	if !errors.Is(err, balancer.ErrNoSubConnAvailable) {
		t.Fatalf("Pick on empty picker = %v, want ErrNoSubConnAvailable", err)
	}
}

func TestSingleSubConnAlwaysPicked(t *testing.T) {
	picker, conns := buildPicker(t, 1)
	for range 32 {
		result, err := picker.Pick(balancer.PickInfo{})
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		if result.SubConn != conns[0] {
			t.Fatalf("Pick chose %v, want the only SubConn", result.SubConn)
		}
	}
}

func TestMultipleSubConnsAllReceiveTraffic(t *testing.T) {
	const picks = 3000
	picker, conns := buildPicker(t, 3)
	counts := make(map[*fakeSubConn]int, len(conns))
	for range picks {
		result, err := picker.Pick(balancer.PickInfo{})
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		counts[result.SubConn.(*fakeSubConn)]++
	}
	for _, subConn := range conns {
		// With 3000 uniform picks over 3 backends, each expects ~1000;
		// anything below 700 signals a broken distribution, not bad luck.
		if counts[subConn] < 700 {
			t.Errorf("subconn %d received %d/%d picks, want roughly uniform", subConn.id, counts[subConn], picks)
		}
	}
}

func TestConcurrentPicksAreSafe(t *testing.T) {
	picker, _ := buildPicker(t, 4)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				if _, err := picker.Pick(balancer.PickInfo{}); err != nil {
					t.Errorf("Pick: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
