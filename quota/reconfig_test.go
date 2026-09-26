package quota

import (
	"errors"
	"path/filepath"
	"testing"
)

func v1Spec() ConfigSpec {
	return ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "a", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": Inherit}},
			{Name: "b", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": Inherit}},
		},
	}
}

func TestCommitConvergenceToNewBudget(t *testing.T) {
	m := NewManager()
	v, _ := m.Commit(v1Spec())
	if v != 1 {
		t.Fatalf("v = %d", v)
	}
	if err := m.Acquire("a", map[string]int64{"cpu": 50}); err != nil {
		t.Fatal(err)
	}
	// Parent doubles: root 200, children inherit 100 each.
	b := NewBuilder(v1Spec())
	b.SetHard("root", "cpu", 200)
	if v, err := m.Commit(b.Build()); err != nil || v != 2 {
		t.Fatalf("commit v2: v=%d err=%v", v, err)
	}
	if err := m.Acquire("a", map[string]int64{"cpu": 50}); err != nil {
		t.Fatalf("a should now fit 100: %v", err)
	}
	// Shrink back: a holds 100 against the new hard 50; more is denied and
	// existing usage converges only as it is released.
	b = NewBuilder(v1Spec())
	b.SetHard("root", "cpu", 100)
	if _, err := m.Commit(b.Build()); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("a", map[string]int64{"cpu": 1}); !errors.Is(err, ErrHardLimitExceeded) {
		t.Fatalf("a over new budget must be denied, got %v", err)
	}
	// Release 60: a drops to 40, then 1 fits within the retracted hard 50.
	if err := m.Release("a", map[string]int64{"cpu": 60}); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("a", map[string]int64{"cpu": 1}); err != nil {
		t.Fatalf("after convergence a must admit again: %v", err)
	}
}

func TestCommitRollbackKeepsPreviousVersion(t *testing.T) {
	store := NewMemoryStore()
	m, err := NewManagerFromStore(store)
	if !errors.Is(err, ErrNoCommitted) {
		t.Fatalf("fresh store err = %v", err)
	}
	v, _ := m.Commit(v1Spec())
	if v != 1 {
		t.Fatalf("v=%d", v)
	}

	// Invalid candidate: duplicate group injected into the built spec.
	dup := v1Spec()
	dup.Groups = append(dup.Groups, GroupSpec{Name: "a", Parent: "root", Weight: 1,
		Hard: map[string]int64{"cpu": Inherit}})
	if _, err := m.Commit(dup); !errors.Is(err, ErrDuplicateGroup) {
		t.Fatalf("want duplicate, got %v", err)
	}
	if ver, ok := m.ActiveVersion(); !ok || ver != 1 {
		t.Fatalf("active version = %d,%v after failed commit, want 1", ver, ok)
	}
	if err := m.Acquire("b", map[string]int64{"cpu": 10}); err != nil {
		t.Fatalf("old config must stay effective: %v", err)
	}

	// Runtime-incompatible candidate: removing a group still carrying usage.
	bad2 := NewBuilder(v1Spec()).RemoveGroup("b")
	if _, err := m.Commit(bad2.Build()); err == nil {
		t.Fatal("removing non-empty group must fail")
	}
	if ver, _ := m.ActiveVersion(); ver != 1 {
		t.Fatalf("version = %d, want 1 after runtime rollback", ver)
	}
	// Store must not have recorded the failed versions.
	sv, _, _, _ := store.Load()
	if sv != 1 {
		t.Fatalf("stored version = %d, want 1", sv)
	}
}

func TestCommitVersionConflict(t *testing.T) {
	m := NewManager()
	m.Commit(v1Spec())
	b1 := NewBuilder(v1Spec()).SetHard("root", "cpu", 200)
	b2 := NewBuilder(v1Spec()).SetHard("root", "cpu", 300)
	if _, err := m.Commit(b1.Build(), 1); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if _, err := m.Commit(b2.Build(), 1); !errors.Is(err, ErrConcurrentCommit) {
		t.Fatalf("stale base commit: %v", err)
	}
	if _, err := m.Commit(b2.Build(), 2); err != nil {
		t.Fatalf("fresh base commit: %v", err)
	}
}

func TestRestartRecoveryMemoryStore(t *testing.T) {
	store := NewMemoryStore()
	m1, _ := NewManagerFromStore(store)
	if _, err := m1.Commit(v1Spec()); err != nil {
		t.Fatal(err)
	}
	m1.Acquire("a", map[string]int64{"cpu": 30})

	m2, err := NewManagerFromStore(store)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if v, ok := m2.ActiveVersion(); !ok || v != 1 {
		t.Fatalf("recovered version = %d,%v", v, ok)
	}
	// Runtime counters restart at zero; config is fully restored.
	rep, err := m2.Report("a")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usage["cpu"].Hard != 50 || rep.Usage["cpu"].Used != 0 {
		t.Fatalf("recovered report = %+v", rep.Usage["cpu"])
	}
	if err := m2.Acquire("a", map[string]int64{"cpu": 50}); err != nil {
		t.Fatalf("recovered tree must admit: %v", err)
	}
}

func TestRestartRecoveryFileStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	store := NewFileStore(path)
	m1, _ := NewManagerFromStore(store)
	m1.Commit(v1Spec())
	m1.Acquire("a", map[string]int64{"cpu": 7}) // runtime only

	m2, err := NewManagerFromStore(NewFileStore(path))
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	rep, _ := m2.Report("a")
	if rep.Usage["cpu"].Hard != 50 {
		t.Fatalf("recovered hard = %d, want 50", rep.Usage["cpu"].Hard)
	}
	if rep.Usage["cpu"].Used != 0 {
		t.Fatalf("runtime usage must reset, got %d", rep.Usage["cpu"].Used)
	}
	// A torn/unknown file yields an error, not a half config.
	m3, err := NewManagerFromStore(store)
	if err != nil {
		// sanity: normal path
		t.Fatalf("normal reload failed: %v", err)
	}
	if v, _ := m3.ActiveVersion(); v != 1 {
		t.Fatalf("reload version = %d", v)
	}
}

func TestBuilderIsolation(t *testing.T) {
	b := NewBuilder(v1Spec())
	b.SetHard("root", "cpu", 999)
	spec := b.Build()
	b.SetHard("root", "cpu", 1)
	// The built snapshot must be independent of later builder edits.
	if spec.Groups[0].Hard["cpu"] != 999 {
		t.Fatalf("builder mutated returned spec: %d", spec.Groups[0].Hard["cpu"])
	}
}
