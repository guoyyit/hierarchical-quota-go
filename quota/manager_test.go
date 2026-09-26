package quota

import "testing"

// newTestManager 构造如下配额树：
//
//	root(cpu: soft 80 / hard 100, mem: hard 100)
//	├── a   (ratio 0.5 → cpu hard 50)
//	│   └── a1 (ratio 0.5 → cpu hard 25)
//	└── b   (ratio 0.5 → cpu hard 50)
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	cfg := &Config{
		Version: 1,
		Groups: map[string]*GroupConfig{
			"root": {Name: "root", Quotas: map[ResourceName]Limit{
				"cpu": {Soft: 80, Hard: 100},
				"mem": {Soft: 50, Hard: 100},
			}},
			"a":  {Name: "a", Parent: "root", ShareRatio: 0.5},
			"a1": {Name: "a1", Parent: "a", ShareRatio: 0.5},
			"b":  {Name: "b", Parent: "root", ShareRatio: 0.5},
		},
	}
	m, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestEffectiveLimitsInheritance(t *testing.T) {
	m := newTestManager(t)

	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Hard; got != 25 {
		t.Fatalf("a1 cpu hard = %d, want 25", got)
	}
	if got := snap.Resources["cpu"].Soft; got != 20 { // 0.5*0.5*80
		t.Fatalf("a1 cpu soft = %d, want 20", got)
	}

	snapA, err := m.Snapshot("a")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snapA.Resources["cpu"].Hard; got != 50 {
		t.Fatalf("a cpu hard = %d, want 50", got)
	}
}

func TestOwnCapNarrowsInheritedShare(t *testing.T) {
	cfg := &Config{
		Version: 1,
		Groups: map[string]*GroupConfig{
			"root": {Name: "root", Quotas: map[ResourceName]Limit{"cpu": {Soft: 100, Hard: 100}}},
			"c": {Name: "c", Parent: "root", ShareRatio: 0.8, Quotas: map[ResourceName]Limit{
				"cpu": {Soft: 30, Hard: 40}, // 自身硬顶 40 < 份额 80
			}},
		},
	}
	m, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	snap, err := m.Snapshot("c")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Hard; got != 40 {
		t.Fatalf("c cpu hard = %d, want 40", got)
	}
}

func TestAcquireUpToHardLimit(t *testing.T) {
	m := newTestManager(t)
	if err := m.Acquire("a1", "cpu", 25); err != nil {
		t.Fatalf("acquire 25: %v", err)
	}
	if err := m.Acquire("a1", "cpu", 1); err == nil {
		t.Fatal("acquire beyond hard limit should fail")
	}
}

func TestSoftLimitAllowsExceed(t *testing.T) {
	m := newTestManager(t)
	// a1 软顶 20、硬顶 25：超过软顶仍允许。
	if err := m.Acquire("a1", "cpu", 22); err != nil {
		t.Fatalf("acquire beyond soft but below hard should succeed: %v", err)
	}
}

func TestAncestorCapConstrainsDescendants(t *testing.T) {
	m := newTestManager(t)
	if err := m.Acquire("a", "cpu", 30); err != nil {
		t.Fatalf("a acquire 30: %v", err)
	}
	// a 子树合计 30+25=55 > a 的有效硬顶 50。
	if err := m.Acquire("a1", "cpu", 25); err == nil {
		t.Fatal("a1 acquire should fail: ancestor a budget exhausted")
	}
	if err := m.Acquire("a1", "cpu", 20); err != nil {
		t.Fatalf("a1 acquire 20 should succeed: %v", err)
	}
}

func TestReleaseAndReacquire(t *testing.T) {
	m := newTestManager(t)
	if err := m.Acquire("a1", "cpu", 25); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Release("a1", "cpu", 10); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := m.Acquire("a1", "cpu", 10); err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if err := m.Acquire("a1", "cpu", 1); err == nil {
		t.Fatal("should be at hard limit again")
	}
}

func TestOverReleaseClampsOnlyThatResource(t *testing.T) {
	m := newTestManager(t)
	if err := m.Acquire("a1", "cpu", 5); err != nil {
		t.Fatalf("acquire cpu: %v", err)
	}
	if err := m.Acquire("a1", "mem", 5); err != nil {
		t.Fatalf("acquire mem: %v", err)
	}
	// cpu 超量释放：报错并钳制到 0，mem 不受影响。
	if err := m.Release("a1", "cpu", 10); err == nil {
		t.Fatal("over-release should return error")
	}
	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Used; got != 0 {
		t.Fatalf("cpu used = %d, want 0 (clamped)", got)
	}
	if got := snap.Resources["mem"].Used; got != 5 {
		t.Fatalf("mem used = %d, want 5 (unaffected)", got)
	}
}

func TestSiblingIsolation(t *testing.T) {
	m := newTestManager(t)
	if err := m.Acquire("a", "cpu", 50); err != nil {
		t.Fatalf("a acquire: %v", err)
	}
	// a 用满不影响 b 的自有额度。
	if err := m.Acquire("b", "cpu", 50); err != nil {
		t.Fatalf("b acquire should succeed: %v", err)
	}
	if err := m.Acquire("b", "cpu", 1); err == nil {
		t.Fatal("b should now be at its own hard limit")
	}
}

func TestUnknownGroup(t *testing.T) {
	m := newTestManager(t)
	if err := m.Acquire("ghost", "cpu", 1); err == nil {
		t.Fatal("acquire on unknown group should fail")
	}
	if err := m.Release("ghost", "cpu", 1); err == nil {
		t.Fatal("release on unknown group should fail")
	}
	if _, err := m.Snapshot("ghost"); err == nil {
		t.Fatal("snapshot on unknown group should fail")
	}
}

func TestInvalidAmounts(t *testing.T) {
	m := newTestManager(t)
	if err := m.Acquire("a1", "cpu", 0); err == nil {
		t.Fatal("zero acquire should fail")
	}
	if err := m.Acquire("a1", "cpu", -5); err == nil {
		t.Fatal("negative acquire should fail")
	}
	if err := m.Release("a1", "cpu", 0); err == nil {
		t.Fatal("zero release should fail")
	}
}
