package quota

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestInitialConfigIsolatedFromCaller 提交后调用方继续修改原配置对象，
// 不影响系统中正在生效的配置。
func TestInitialConfigIsolatedFromCaller(t *testing.T) {
	cfg := baseTree(1, 100)
	m, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// 调用方在系统之外改动原对象（篡改版本、删除组、修改额度）。
	cfg.Version = 999
	delete(cfg.Groups, "a1")
	cfg.Groups["root"].Quotas["cpu"] = Limit{Soft: 1, Hard: 1}

	got := m.Config()
	if got.Version != 1 {
		t.Fatalf("effective version = %d, want 1 (caller mutation leaked)", got.Version)
	}
	if _, ok := got.Groups["a1"]; !ok {
		t.Fatal("group a1 disappeared from effective config after caller mutation")
	}
	if h := got.Groups["root"].Quotas["cpu"].Hard; h != 100 {
		t.Fatalf("root hard = %d, want 100 (caller mutation leaked)", h)
	}

	// 生效限额未变：a1 硬顶仍是 25。
	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if h := snap.Resources["cpu"].Hard; h != 25 {
		t.Fatalf("a1 hard = %d, want 25", h)
	}
}

// TestReconfigureIsolatedFromCaller 显式提交后，原对象的后续修改不进生效配置。
func TestReconfigureIsolatedFromCaller(t *testing.T) {
	m, err := NewManager(baseTree(1, 100), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	next := baseTree(2, 200)
	if err := m.Reconfigure(next); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}

	// 提交完成后篡改原对象。
	next.Groups["root"].Quotas["cpu"] = Limit{Soft: 9, Hard: 9}
	next.Version = 200

	got := m.Config()
	if got.Version != 2 {
		t.Fatalf("effective version = %d, want 2", got.Version)
	}
	if h := got.Groups["root"].Quotas["cpu"].Hard; h != 200 {
		t.Fatalf("root hard = %d, want 200 (post-commit mutation leaked)", h)
	}
}

// TestReturnedConfigIsIndependentCopy 查询返回的是独立副本，
// 修改副本不影响系统状态；不同次查询返回的副本也互不共享。
func TestReturnedConfigIsIndependentCopy(t *testing.T) {
	m, err := NewManager(baseTree(1, 100), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	c1 := m.Config()
	c2 := m.Config()
	c1.Groups["root"].Quotas["cpu"] = Limit{Soft: 1, Hard: 1}
	c1.Version = 42
	delete(c1.Groups, "b")

	if c2.Version != 1 {
		t.Fatalf("second copy version = %d, copies share state", c2.Version)
	}
	if _, ok := c2.Groups["b"]; !ok {
		t.Fatal("deleting group from one copy affected another copy")
	}
	live := m.Config()
	if live.Version != 1 || live.Groups["root"].Quotas["cpu"].Hard != 100 {
		t.Fatalf("mutating returned copy changed live config: %+v", live.Groups["root"].Quotas["cpu"])
	}
	if _, ok := live.Groups["b"]; !ok {
		t.Fatal("group b missing from live config after copy mutation")
	}
}

// TestVersionMustStrictlyIncrement 版本号由系统自动严格维护：
// 重复、回退、跳跃均被拒绝；被拒绝时旧配置完整生效，版本仍可继续正确递增。
func TestVersionMustStrictlyIncrement(t *testing.T) {
	m, err := NewManager(baseTree(1, 100), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	wantVersion := uint64(1)
	reject := func(c *Config, label string) {
		t.Helper()
		err := m.Reconfigure(c)
		if !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("%s: want ErrVersionConflict, got %v", label, err)
		}
		if v := m.Config().Version; v != wantVersion {
			t.Fatalf("%s: effective version = %d, want %d (rejected commit must not apply)",
				label, v, wantVersion)
		}
	}

	reject(baseTree(1, 200), "duplicate version 1")
	reject(baseTree(0, 200), "rollback version 0")
	reject(baseTree(3, 200), "skipped version 3")
	reject(baseTree(99, 200), "far future version 99")

	// 被拒绝的提交不影响旧限额：a1 硬顶仍是 25。
	snap, _ := m.Snapshot("a1")
	if h := snap.Resources["cpu"].Hard; h != 25 {
		t.Fatalf("a1 hard = %d, want 25 after rejected commits", h)
	}

	// 正确的下一个版本 2 被接受。
	if err := m.Reconfigure(baseTree(2, 200)); err != nil {
		t.Fatalf("Reconfigure v2: %v", err)
	}
	if v := m.Config().Version; v != 2 {
		t.Fatalf("effective version = %d, want 2", v)
	}
	wantVersion = 2
	// 再次重复提交 v2 仍被拒绝。
	reject(baseTree(2, 400), "duplicate version 2")
	// v3 继续严格递增。
	if err := m.Reconfigure(baseTree(3, 400)); err != nil {
		t.Fatalf("Reconfigure v3: %v", err)
	}
	if v := m.Config().Version; v != 3 {
		t.Fatalf("effective version = %d, want 3", v)
	}
}

// TestFirstCommitAssignedVersionOne 首次提交的版本号由系统分配为 1，
// 与调用方在初始配置中填写的版本号无关。
func TestFirstCommitAssignedVersionOne(t *testing.T) {
	for _, supplied := range []uint64{0, 1, 7, 999} {
		m, err := NewManager(baseTree(supplied, 100), nil)
		if err != nil {
			t.Fatalf("NewManager(%d): %v", supplied, err)
		}
		if v := m.Config().Version; v != 1 {
			t.Fatalf("supplied %d: effective version = %d, want 1", supplied, v)
		}
	}
}

// TestVersionContinuesAfterRestart 重启后版本号从最近一次已提交版本继续递增，
// 不重复、不回退；持久化保存的是系统分配的版本。
func TestVersionContinuesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	store := &FileStore{Path: path}

	m1, err := NewManager(baseTree(42, 100), store) // 调用方给 42，系统分配 1
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if v := m1.Config().Version; v != 1 {
		t.Fatalf("initial version = %d, want 1", v)
	}
	if err := m1.Reconfigure(baseTree(2, 200)); err != nil {
		t.Fatalf("Reconfigure v2: %v", err)
	}
	if err := m1.Reconfigure(baseTree(3, 300)); err != nil {
		t.Fatalf("Reconfigure v3: %v", err)
	}

	// 重启：恢复到最近提交 v3。
	m2, err := NewManager(baseTree(1, 100), store)
	if err != nil {
		t.Fatalf("NewManager after restart: %v", err)
	}
	if v := m2.Config().Version; v != 3 {
		t.Fatalf("recovered version = %d, want 3", v)
	}
	// 回退/重复提交在恢复后依旧被拒绝。
	if err := m2.Reconfigure(baseTree(3, 400)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("duplicate v3 after restart: want ErrVersionConflict, got %v", err)
	}
	if err := m2.Reconfigure(baseTree(2, 400)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("rollback v2 after restart: want ErrVersionConflict, got %v", err)
	}
	// 从 v3 继续递增到 v4。
	if err := m2.Reconfigure(baseTree(4, 400)); err != nil {
		t.Fatalf("Reconfigure v4 after restart: %v", err)
	}
	if v := m2.Config().Version; v != 4 {
		t.Fatalf("version after restart+commit = %d, want 4", v)
	}

	// 再次重启，版本仍为 4，无重复或回退。
	m3, err := NewManager(baseTree(1, 100), store)
	if err != nil {
		t.Fatalf("NewManager second restart: %v", err)
	}
	if v := m3.Config().Version; v != 4 {
		t.Fatalf("version after second restart = %d, want 4", v)
	}
	if err := m3.Reconfigure(baseTree(5, 500)); err != nil {
		t.Fatalf("Reconfigure v5: %v", err)
	}
}

// TestRejectedCommitNotPersisted 被拒绝的配置绝不进入持久化存储。
func TestRejectedCommitNotPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	store := &FileStore{Path: path}
	m, err := NewManager(baseTree(1, 100), store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Reconfigure(baseTree(1, 200)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("duplicate commit: want ErrVersionConflict, got %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != 1 {
		t.Fatalf("persisted version = %d, want 1 (rejected commit must not persist)", loaded.Version)
	}
}
