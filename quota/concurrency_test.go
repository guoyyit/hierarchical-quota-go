package quota

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConcurrentAcquireReleaseNoLostUpdate(t *testing.T) {
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "g", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": Inherit}},
		},
	}
	m := NewManager()
	m.Commit(spec)

	const goroutines = 50
	const rounds = 200
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				if err := m.Acquire("g", map[string]int64{"cpu": 1}); err == nil {
					// Hold briefly then release every granted unit exactly once.
					m.Release("g", map[string]int64{"cpu": 1})
				}
			}
		}()
	}
	wg.Wait()
	rep, _ := m.Report("g")
	if rep.Usage["cpu"].Used != 0 {
		t.Fatalf("final used = %d, want 0 (lost update / double release)", rep.Usage["cpu"].Used)
	}
}

func TestConcurrentHardLimitNeverBreached(t *testing.T) {
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "g", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": Inherit}},
		},
	}
	m := NewManager()
	m.Commit(spec)

	const goroutines = 32
	var wg sync.WaitGroup
	var granted int64
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if err := m.Acquire("g", map[string]int64{"cpu": 1}); err == nil {
					atomic.AddInt64(&granted, 1)
				}
			}
		}()
	}
	wg.Wait()
	rep, _ := m.Report("g")
	if rep.Usage["cpu"].Used != granted {
		t.Fatalf("used %d != granted %d", rep.Usage["cpu"].Used, granted)
	}
	if rep.Usage["cpu"].Used > 100 {
		t.Fatalf("hard limit breached: used %d", rep.Usage["cpu"].Used)
	}
	if granted != 100 {
		t.Fatalf("granted = %d, want exactly 100 (each successful acquire held)", granted)
	}
}

func TestConcurrentMultiResourceAllOrNothing(t *testing.T) {
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}, {Name: "mem"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100, "mem": 100}},
			{Name: "g", Parent: "root", Weight: 1,
				Hard: map[string]int64{"cpu": Inherit, "mem": Inherit}},
		},
	}
	m := NewManager()
	m.Commit(spec)

	const goroutines = 16
	var wg sync.WaitGroup
	var cpuGranted, memGranted int64
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				err := m.Acquire("g", map[string]int64{"cpu": 1, "mem": 1})
				if err == nil {
					atomic.AddInt64(&cpuGranted, 1)
					atomic.AddInt64(&memGranted, 1)
				}
			}
		}()
	}
	wg.Wait()
	rep, _ := m.Report("g")
	if rep.Usage["cpu"].Used != rep.Usage["mem"].Used {
		t.Fatalf("resources diverged: cpu=%d mem=%d (all-or-nothing violated)",
			rep.Usage["cpu"].Used, rep.Usage["mem"].Used)
	}
	if rep.Usage["cpu"].Used != cpuGranted || rep.Usage["mem"].Used != memGranted {
		t.Fatalf("granted counters mismatch report: %+v", rep.Usage)
	}
	if rep.Usage["cpu"].Used > 100 || rep.Usage["mem"].Used > 100 {
		t.Fatalf("hard limit breached: %+v", rep.Usage)
	}
}

func TestResourceIsolation(t *testing.T) {
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}, {Name: "mem"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 5, "mem": 1000}},
			{Name: "g", Parent: "root", Weight: 1,
				Hard: map[string]int64{"cpu": Inherit, "mem": Inherit}},
		},
	}
	m := NewManager()
	m.Commit(spec)
	// Exhaust cpu.
	if err := m.Acquire("g", map[string]int64{"cpu": 5}); err != nil {
		t.Fatal(err)
	}
	// cpu denied, but mem billing must remain fully functional.
	if err := m.Acquire("g", map[string]int64{"cpu": 1}); !errors.Is(err, ErrHardLimitExceeded) {
		t.Fatalf("cpu should be exhausted: %v", err)
	}
	if err := m.Acquire("g", map[string]int64{"mem": 123}); err != nil {
		t.Fatalf("mem must work independently: %v", err)
	}
	if err := m.Release("g", map[string]int64{"mem": 123}); err != nil {
		t.Fatalf("mem release: %v", err)
	}
	// Unknown resource in a multi-resource request charges nothing.
	before, _ := m.Report("g")
	err := m.Acquire("g", map[string]int64{"mem": 10, "gpu": 1})
	if !errors.Is(err, ErrUnknownResource) {
		t.Fatalf("want unknown resource, got %v", err)
	}
	after, _ := m.Report("g")
	if after.Usage["mem"].Used != before.Usage["mem"].Used {
		t.Fatal("unknown resource request must not charge other resources")
	}
}

func TestGroupIsolation(t *testing.T) {
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "a", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": 20}},
			{Name: "b", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": 80}},
		},
	}
	m := NewManager()
	m.Commit(spec)
	if err := m.Acquire("a", map[string]int64{"cpu": 20}); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("a", map[string]int64{"cpu": 1}); !errors.Is(err, ErrHardLimitExceeded) {
		t.Fatalf("a over own hard: %v", err)
	}
	// a over budget does not touch b's availability.
	if err := m.Acquire("b", map[string]int64{"cpu": 80}); err != nil {
		t.Fatalf("b must keep its own full quota: %v", err)
	}
}

func TestReportSnapshotConsistencyUnderLoad(t *testing.T) {
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}, {Name: "mem"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 1000, "mem": 1000}},
			{Name: "g", Parent: "root", Weight: 1,
				Hard: map[string]int64{"cpu": Inherit, "mem": Inherit}},
		},
	}
	m := NewManager()
	m.Commit(spec)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // writer
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if m.Acquire("g", map[string]int64{"cpu": 1, "mem": 1}) == nil {
					m.Release("g", map[string]int64{"cpu": 1, "mem": 1})
				}
			}
		}
	}()
	done := make(chan struct{})
	go func() { // reader: repeated self-consistent snapshots
		defer wg.Done()
		defer close(done)
		for i := 0; i < 2000; i++ {
			tree, err := m.ReportTree("root")
			if err != nil || len(tree) != 2 {
				t.Errorf("report tree: %v len=%d", err, len(tree))
				return
			}
			var cpu, mem int64
			for _, r := range tree {
				cpu += r.Usage["cpu"].Used
				mem += r.Usage["mem"].Used
			}
			if cpu < 0 || cpu > 1000 || mem < 0 || mem > 1000 {
				t.Errorf("inconsistent snapshot cpu=%d mem=%d", cpu, mem)
				return
			}
		}
	}()
	<-done
	close(stop)
	wg.Wait()
}
