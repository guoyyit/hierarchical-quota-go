package quota

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

// baseTree 返回 root(100) → a(0.5) → a1(0.5) 与 root → b(0.5) 的基础树。
func baseTree(version uint64, rootHard int64) *Config {
	return &Config{
		Version: version,
		Groups: map[string]*GroupConfig{
			"root": {Name: "root", Quotas: map[ResourceName]Limit{"cpu": {Soft: rootHard * 8 / 10, Hard: rootHard}}},
			"a":    {Name: "a", Parent: "root", ShareRatio: 0.5},
			"a1":   {Name: "a1", Parent: "a", ShareRatio: 0.5},
			"b":    {Name: "b", Parent: "root", ShareRatio: 0.5},
		},
	}
}

func TestReconfigureConvergesChildren(t *testing.T) {
	m, err := NewManager(baseTree(1, 100), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// 父组上调：root 100 → 200，子组收敛到新预算 a=100, a1=50。
	if err := m.Reconfigure(baseTree(2, 200)); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Hard; got != 50 {
		t.Fatalf("a1 hard after upscale = %d, want 50", got)
	}
	if got := snap.Version; got != 2 {
		t.Fatalf("snapshot version = %d, want 2", got)
	}

	// 父组下调：root 200 → 80，子组收敛 a=40, a1=20。
	if err := m.Reconfigure(baseTree(3, 80)); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	snap, err = m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Hard; got != 20 {
		t.Fatalf("a1 hard after downscale = %d, want 20", got)
	}
}

func TestReconfigurePreservesUsage(t *testing.T) {
	m, err := NewManager(baseTree(1, 100), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Acquire("a1", "cpu", 10); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Reconfigure(baseTree(2, 200)); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Used; got != 10 {
		t.Fatalf("used after reconfigure = %d, want 10 (preserved)", got)
	}
}

func TestReconfigureRollbackOnInvalid(t *testing.T) {
	m, err := NewManager(baseTree(1, 100), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	before := m.Config()

	// 非法配置：两个根。
	bad := baseTree(2, 200)
	bad.Groups["root2"] = &GroupConfig{Name: "root2", Quotas: map[ResourceName]Limit{"cpu": {Soft: 1, Hard: 1}}}
	if err := m.Reconfigure(bad); err == nil {
		t.Fatal("reconfigure with two roots should fail")
	}
	if got := m.Config(); !reflect.DeepEqual(got, before) {
		t.Fatalf("config changed after failed reconfigure: version %d", got.Version)
	}

	// 非法配置：成环。
	cyclic := baseTree(2, 200)
	cyclic.Groups["root"].Parent = "a1"
	if err := m.Reconfigure(cyclic); err == nil {
		t.Fatal("reconfigure with cycle should fail")
	}
	if got := m.Config(); !reflect.DeepEqual(got, before) {
		t.Fatal("config changed after cyclic reconfigure")
	}

	// 旧版本限额仍然完整生效。
	if err := m.Acquire("a1", "cpu", 25); err != nil {
		t.Fatalf("acquire under old config: %v", err)
	}
	if err := m.Acquire("a1", "cpu", 1); err == nil {
		t.Fatal("old hard limit 25 should still be in force")
	}
}

// failStore 在 fail 置位后 Save 注入失败，用于验证持久化失败导致整体回退。
type failStore struct {
	saved *Config
	fail  bool
}

func (f *failStore) Save(c *Config) error {
	if f.fail {
		return errors.New("injected save failure")
	}
	f.saved = c
	return nil
}
func (f *failStore) Load() (*Config, error) {
	if f.saved == nil {
		return nil, nil
	}
	return f.saved, nil
}

func TestReconfigureRollbackOnPersistFailure(t *testing.T) {
	store := &failStore{}
	m, err := NewManager(baseTree(1, 100), store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	store.fail = true
	if err := m.Reconfigure(baseTree(2, 200)); err == nil {
		t.Fatal("reconfigure should fail when persist fails")
	}
	if got := m.Config().Version; got != 1 {
		t.Fatalf("version = %d, want 1 (rolled back)", got)
	}
	// 旧限额依旧生效。
	if err := m.Acquire("a1", "cpu", 25); err != nil {
		t.Fatalf("acquire under old config: %v", err)
	}
	if err := m.Acquire("a1", "cpu", 1); err == nil {
		t.Fatal("old hard limit should still be in force")
	}
}

func TestRestartRecoveryFromCommitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	store := &FileStore{Path: path}

	m1, err := NewManager(baseTree(1, 100), store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m1.Reconfigure(baseTree(2, 200)); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}

	// 模拟重启：初始配置给的是 v1，但应恢复到最近提交的 v2。
	m2, err := NewManager(baseTree(1, 100), store)
	if err != nil {
		t.Fatalf("NewManager after restart: %v", err)
	}
	if got := m2.Config().Version; got != 2 {
		t.Fatalf("recovered version = %d, want 2", got)
	}
	snap, err := m2.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Hard; got != 50 {
		t.Fatalf("a1 hard after recovery = %d, want 50", got)
	}
}

func TestUncommittedChangesNotRecovered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	store := &FileStore{Path: path}

	m1, err := NewManager(baseTree(1, 100), store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// 提交失败的配置不得落盘。
	bad := baseTree(2, 200)
	bad.Groups["a"].ShareRatio = 5 // 非法比例
	if err := m1.Reconfigure(bad); err == nil {
		t.Fatal("reconfigure with invalid ratio should fail")
	}

	m2, err := NewManager(baseTree(9, 999), store)
	if err != nil {
		t.Fatalf("NewManager after restart: %v", err)
	}
	if got := m2.Config().Version; got != 1 {
		t.Fatalf("recovered version = %d, want 1 (uncommitted v2 must not persist)", got)
	}
}
