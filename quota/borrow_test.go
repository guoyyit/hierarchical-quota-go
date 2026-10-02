package quota

import (
	"testing"
)

// newBorrowManager 构造借用测试树：
//
//	root(cpu hard 300)
//	├── hi  (ratio 1/3 → 100, priority 10)
//	├── lo1 (ratio 1/3 → 100, priority 1)
//	└── lo2 (ratio 1/3 → 100, priority 2)
func newBorrowManager(t *testing.T) *Manager {
	t.Helper()
	cfg := &Config{
		Version: 1,
		Groups: map[string]*GroupConfig{
			"root": {Name: "root", Quotas: map[ResourceName]Limit{"cpu": {Soft: 300, Hard: 300}}},
			"hi":   {Name: "hi", Parent: "root", ShareRatio: 1.0 / 3, Priority: 10},
			"lo1":  {Name: "lo1", Parent: "root", ShareRatio: 1.0 / 3, Priority: 1},
			"lo2":  {Name: "lo2", Parent: "root", ShareRatio: 1.0 / 3, Priority: 2},
		},
	}
	m, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestBorrowWhenOwnQuotaExhausted(t *testing.T) {
	m := newBorrowManager(t)
	if err := m.Acquire("hi", "cpu", 100); err != nil {
		t.Fatalf("hi acquire 100: %v", err)
	}
	// 自有额度用尽，向低优先级组借用 50。
	if err := m.Acquire("hi", "cpu", 50); err != nil {
		t.Fatalf("hi acquire 50 via borrow: %v", err)
	}
	borrows := m.Borrows()
	if len(borrows) != 1 {
		t.Fatalf("borrow records = %d, want 1", len(borrows))
	}
	br := borrows[0]
	if br.Lender != "lo1" || br.Borrower != "hi" || br.Amount != 50 {
		t.Fatalf("unexpected borrow record: %+v", br)
	}
	// 借出方可用额度被预留压缩。
	snap, err := m.Snapshot("lo1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Remaining; got != 50 {
		t.Fatalf("lo1 remaining = %d, want 50", got)
	}
}

func TestBorrowDeterministicLenderOrder(t *testing.T) {
	m := newBorrowManager(t)
	if err := m.Acquire("hi", "cpu", 100); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// 需要借 120：先 lo1（优先级 1）出让 100，再 lo2（优先级 2）出让 20。
	if err := m.Acquire("hi", "cpu", 120); err != nil {
		t.Fatalf("hi acquire 120 via borrow: %v", err)
	}
	borrows := m.Borrows()
	if len(borrows) != 2 {
		t.Fatalf("borrow records = %d, want 2", len(borrows))
	}
	if borrows[0].Lender != "lo1" || borrows[0].Amount != 100 {
		t.Fatalf("first record = %+v, want lo1/100", borrows[0])
	}
	if borrows[1].Lender != "lo2" || borrows[1].Amount != 20 {
		t.Fatalf("second record = %+v, want lo2/20", borrows[1])
	}
}

func TestBorrowFailsWhenSurplusInsufficient(t *testing.T) {
	m := newBorrowManager(t)
	if err := m.Acquire("lo1", "cpu", 60); err != nil {
		t.Fatalf("lo1 acquire: %v", err)
	}
	if err := m.Acquire("lo2", "cpu", 60); err != nil {
		t.Fatalf("lo2 acquire: %v", err)
	}
	if err := m.Acquire("hi", "cpu", 100); err != nil {
		t.Fatalf("hi acquire 100: %v", err)
	}
	// 全树剩余可借 40+40=80 < 100，整体失败且不产生任何记录。
	if err := m.Acquire("hi", "cpu", 100); err == nil {
		t.Fatal("borrow should fail when total surplus insufficient")
	}
	if got := len(m.Borrows()); got != 0 {
		t.Fatalf("failed borrow left %d records, want 0", got)
	}
}

func TestLowPriorityCannotBorrowFromHigh(t *testing.T) {
	m := newBorrowManager(t)
	if err := m.Acquire("lo1", "cpu", 100); err != nil {
		t.Fatalf("lo1 acquire: %v", err)
	}
	// lo1 优先级 1 < hi 的 10，不能向 hi 借；lo2 优先级 2 也高于 1。
	if err := m.Acquire("lo1", "cpu", 1); err == nil {
		t.Fatal("low priority group should not borrow from higher/equal priority groups")
	}
	if got := len(m.Borrows()); got != 0 {
		t.Fatalf("unexpected borrow records: %d", got)
	}
}

func TestReclaimBorrowsProducesOverOccupancy(t *testing.T) {
	m := newBorrowManager(t)
	if err := m.Acquire("hi", "cpu", 100); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Acquire("hi", "cpu", 120); err != nil { // 借 lo1 100 + lo2 20
		t.Fatalf("acquire via borrow: %v", err)
	}

	over := m.ReclaimBorrows()
	if len(over) != 1 {
		t.Fatalf("over-occupancy entries = %d, want 1", len(over))
	}
	if over[0].Group != "hi" || over[0].Resource != "cpu" || over[0].Excess != 120 {
		t.Fatalf("unexpected over-occupancy: %+v", over[0])
	}
	if got := len(m.Borrows()); got != 0 {
		t.Fatalf("borrow records after reclaim = %d, want 0", got)
	}

	// 回收动作内已自动释放超额占用：hi 的用量回缩到自有有效硬顶 100，
	// 无需调用方另行释放；清单中的 Excess(120) 即系统实际释放量。
	snapHi, err := m.Snapshot("hi")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snapHi.Resources["cpu"].Aggregate; got != 100 {
		t.Fatalf("hi aggregate after reclaim = %d, want 100 (auto-released excess)", got)
	}
	// 借出方恢复全额。
	snap, err := m.Snapshot("lo1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Remaining; got != 100 {
		t.Fatalf("lo1 remaining after reclaim = %d, want 100", got)
	}
	// hi 已回到额度之内：可按正常规则继续消耗（此处经重新借用成功）。
	if err := m.Acquire("hi", "cpu", 1); err != nil {
		t.Fatalf("hi acquire after auto-release should succeed: %v", err)
	}
}

func TestReclaimOrderIsDeterministic(t *testing.T) {
	m := newBorrowManager(t)
	if err := m.Acquire("hi", "cpu", 100); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Acquire("hi", "cpu", 150); err != nil { // lo1 100, lo2 50
		t.Fatalf("acquire via borrow: %v", err)
	}
	first := m.ReclaimBorrows()
	second := m.ReclaimBorrows() // 幂等：无记录可还、无超额可释放，返回空清单
	if len(first) != 1 || first[0].Excess != 150 {
		t.Fatalf("first reclaim = %+v, want single excess 150", first)
	}
	if len(second) != 0 {
		t.Fatalf("second reclaim = %+v, want empty (excess already auto-released)", second)
	}
	// 自动释放后 hi 合计用量回缩到自有有效硬顶 100。
	snap, err := m.Snapshot("hi")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Aggregate; got != 100 {
		t.Fatalf("hi aggregate after reclaim = %d, want 100", got)
	}
}

func TestAncestorBottleneckLeavesNoBorrowRecords(t *testing.T) {
	// hi 下挂子组 hi-sub；hi 自身额度小，hi-sub 申请时瓶颈在 hi。
	cfg := &Config{
		Version: 1,
		Groups: map[string]*GroupConfig{
			"root":   {Name: "root", Quotas: map[ResourceName]Limit{"cpu": {Soft: 300, Hard: 300}}},
			"hi":     {Name: "hi", Parent: "root", ShareRatio: 1.0 / 3, Priority: 10},
			"hi-sub": {Name: "hi-sub", Parent: "hi", ShareRatio: 1, Priority: 10},
			"lo1":    {Name: "lo1", Parent: "root", ShareRatio: 1.0 / 3, Priority: 1},
		},
	}
	m, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Acquire("hi", "cpu", 100); err != nil { // hi 自身用满
		t.Fatalf("hi acquire: %v", err)
	}
	// hi-sub 申请：瓶颈在祖先 hi，借用无法解决，必须失败且不产生记录。
	if err := m.Acquire("hi-sub", "cpu", 1); err == nil {
		t.Fatal("acquire should fail: ancestor hi exhausted")
	}
	if got := len(m.Borrows()); got != 0 {
		t.Fatalf("ancestor bottleneck left %d borrow records, want 0", got)
	}
}
