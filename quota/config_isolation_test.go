package quota

import (
	"path/filepath"
	"testing"
)

// TestInitialConfigIsolatedFromCaller 初始配置被系统接受后，
// 调用方继续修改原对象不影响系统内正在生效的限额。
func TestInitialConfigIsolatedFromCaller(t *testing.T) {
	cfg := baseTree(1, 100)
	m, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// 提交后从外部篡改原对象：改限额、改比例、删组、改版本号。
	cfg.Groups["root"].Quotas["cpu"] = Limit{Soft: 1, Hard: 1}
	cfg.Groups["a1"].ShareRatio = 0.01
	delete(cfg.Groups, "b")
	cfg.Version = 999

	if got := m.Config().Version; got != 1 {
		t.Fatalf("version = %d, want 1 (caller mutation must not leak)", got)
	}
	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Hard; got != 25 {
		t.Fatalf("a1 hard = %d, want 25 (unaffected by caller mutation)", got)
	}
	if _, err := m.Snapshot("b"); err != nil {
		t.Fatalf("group b must still exist: %v", err)
	}
	// 原硬顶 25 依旧生效。
	if err := m.Acquire("a1", "cpu", 25); err != nil {
		t.Fatalf("acquire 25: %v", err)
	}
	if err := m.Acquire("a1", "cpu", 1); err == nil {
		t.Fatal("hard limit 25 should still be in force")
	}
}

// TestReconfigureIsolatedFromCaller 重配置被接受后，
// 调用方修改原配置对象不影响生效配置与已落盘内容。
func TestReconfigureIsolatedFromCaller(t *testing.T) {
	store := &failStore{}
	m, err := NewManager(baseTree(1, 100), store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	c2 := baseTree(2, 200)
	if err := m.Reconfigure(c2); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}

	// 提交后从外部篡改原对象。
	c2.Groups["root"].Quotas["cpu"] = Limit{Soft: 1, Hard: 1}
	delete(c2.Groups, "a1")
	c2.Version = 777

	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Hard; got != 50 {
		t.Fatalf("a1 hard = %d, want 50 (committed v2 unaffected)", got)
	}
	// 已落盘的内容同样未被污染。
	if got := store.saved.Groups["root"].Quotas["cpu"].Hard; got != 200 {
		t.Fatalf("persisted root hard = %d, want 200", got)
	}
	if _, ok := store.saved.Groups["a1"]; !ok {
		t.Fatal("persisted config lost group a1")
	}
}

// TestConfigGetterReturnsIndependentCopy 查询返回独立副本，
// 修改副本不影响系统内正在生效的限额。
func TestConfigGetterReturnsIndependentCopy(t *testing.T) {
	m, err := NewManager(baseTree(1, 100), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	c := m.Config()
	c.Groups["root"].Quotas["cpu"] = Limit{Soft: 1, Hard: 1}
	delete(c.Groups, "a1")
	c.Version = 42

	if got := m.Config().Version; got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}
	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Hard; got != 25 {
		t.Fatalf("a1 hard = %d, want 25 (copy mutation must not leak)", got)
	}
}

// TestDuplicateAndRegressiveVersionRejected 版本号由系统强制递增：
// 重复或回退版本的提交被拒绝，旧配置保持完整生效。
func TestDuplicateAndRegressiveVersionRejected(t *testing.T) {
	m, err := NewManager(baseTree(1, 100), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Reconfigure(baseTree(2, 200)); err != nil {
		t.Fatalf("Reconfigure v2: %v", err)
	}

	// 重复版本：拒绝。
	if err := m.Reconfigure(baseTree(2, 999)); err == nil {
		t.Fatal("duplicate version 2 should be rejected")
	}
	// 回退版本：拒绝。
	if err := m.Reconfigure(baseTree(1, 999)); err == nil {
		t.Fatal("regressive version 1 should be rejected")
	}
	// 旧配置（v2，root hard 200）保持完整生效。
	if got := m.Config().Version; got != 2 {
		t.Fatalf("version = %d, want 2 (rejected commits must not take effect)", got)
	}
	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Hard; got != 50 {
		t.Fatalf("a1 hard = %d, want 50 (v2 limits still in force)", got)
	}

	// 严格递增的版本成功，且版本号唯一对应该次提交的内容。
	if err := m.Reconfigure(baseTree(3, 300)); err != nil {
		t.Fatalf("Reconfigure v3: %v", err)
	}
	if got := m.Config().Version; got != 3 {
		t.Fatalf("version = %d, want 3", got)
	}
}

// TestVersionContinuesAfterRestart 重启后版本号从最近一次已提交版本继续递增，
// 不得出现重复或回退。
func TestVersionContinuesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	store := &FileStore{Path: path}

	m1, err := NewManager(baseTree(1, 100), store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m1.Reconfigure(baseTree(5, 200)); err != nil {
		t.Fatalf("Reconfigure v5: %v", err)
	}

	// 重启：恢复到 v5；重复/回退版本被拒绝，严格递增版本成功。
	m2, err := NewManager(baseTree(1, 100), store)
	if err != nil {
		t.Fatalf("NewManager after restart: %v", err)
	}
	if got := m2.Config().Version; got != 5 {
		t.Fatalf("recovered version = %d, want 5", got)
	}
	if err := m2.Reconfigure(baseTree(5, 300)); err == nil {
		t.Fatal("duplicate version 5 after restart should be rejected")
	}
	if err := m2.Reconfigure(baseTree(4, 300)); err == nil {
		t.Fatal("regressive version 4 after restart should be rejected")
	}
	if err := m2.Reconfigure(baseTree(6, 300)); err != nil {
		t.Fatalf("Reconfigure v6 after restart: %v", err)
	}

	// 再次重启：从 v6 继续。
	m3, err := NewManager(baseTree(1, 100), store)
	if err != nil {
		t.Fatalf("NewManager after second restart: %v", err)
	}
	if got := m3.Config().Version; got != 6 {
		t.Fatalf("recovered version = %d, want 6", got)
	}
	snap, err := m3.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Hard; got != 75 { // 300*0.5*0.5
		t.Fatalf("a1 hard = %d, want 75 (v6 content)", got)
	}
}
