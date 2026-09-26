package quota

import (
	"errors"
	"testing"
)

func testSpec() ConfigSpec {
	return ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}, {Name: "mem"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100, "mem": 1000}},
			{Name: "a", Parent: "root", Weight: 3, Hard: map[string]int64{"cpu": Inherit, "mem": 600}},
			{Name: "b", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": Inherit, "mem": Inherit}},
			{Name: "a1", Parent: "a", Weight: 1, Hard: map[string]int64{"cpu": Inherit, "mem": Inherit}},
		},
	}
}

func TestSmokeCommitAndLimits(t *testing.T) {
	m := NewManager()
	v, err := m.Commit(testSpec())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if v != 1 {
		t.Fatalf("version = %d, want 1", v)
	}
	m.mu.RLock()
	snap := m.snap
	m.mu.RUnlock()

	cpuA, _ := snap.EffectiveHard("a", "cpu")
	cpuB, _ := snap.EffectiveHard("b", "cpu")
	if cpuA != 75 || cpuB != 25 {
		t.Fatalf("inherited shares a=%d b=%d, want 75/25", cpuA, cpuB)
	}
	memA1, _ := snap.EffectiveHard("a1", "mem")
	if memA1 != 600 {
		t.Fatalf("a1 mem = %d, want 600 inherited from a", memA1)
	}
	// default soft == hard
	softA, _ := snap.EffectiveSoft("a", "cpu")
	if softA != 75 {
		t.Fatalf("default soft a cpu = %d, want 75", softA)
	}
}

func TestSmokeAcquireRelease(t *testing.T) {
	m := NewManager()
	if _, err := m.Commit(testSpec()); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("a1", map[string]int64{"cpu": 70, "mem": 100}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// a's cpu share is 75; 70+10 exceeds it.
	err := m.Acquire("a1", map[string]int64{"cpu": 10})
	if !errors.Is(err, ErrHardLimitExceeded) {
		t.Fatalf("want hard limit exceeded, got %v", err)
	}
	// all-or-nothing: mem must not have been charged either
	rep, err := m.Report("a1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usage["mem"].Used != 100 {
		t.Fatalf("mem used = %d, want 100 (denied request charged nothing)", rep.Usage["mem"].Used)
	}
	if err := m.Release("a1", map[string]int64{"cpu": 70}); err != nil {
		t.Fatalf("release: %v", err)
	}
	rep, _ = m.Report("a1")
	if rep.Usage["cpu"].Used != 0 {
		t.Fatalf("cpu used = %d, want 0", rep.Usage["cpu"].Used)
	}
}
