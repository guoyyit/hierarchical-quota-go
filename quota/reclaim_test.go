package quota

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// newNestedBorrowManager 构造带子组的借用树：
//
//	root(cpu hard 300)
//	├── hi     (ratio 1/3 → 100, priority 10)
//	│   └── hi-sub (ratio 1 → 100, priority 10)
//	├── lo1    (ratio 1/3 → 100, priority 1)
//	└── lo2    (ratio 1/3 → 100, priority 2)
func newNestedBorrowManager(t *testing.T) *Manager {
	t.Helper()
	cfg := &Config{
		Version: 1,
		Groups: map[string]*GroupConfig{
			"root":   {Name: "root", Quotas: map[ResourceName]Limit{"cpu": {Soft: 300, Hard: 300}}},
			"hi":     {Name: "hi", Parent: "root", ShareRatio: 1.0 / 3, Priority: 10},
			"hi-sub": {Name: "hi-sub", Parent: "hi", ShareRatio: 1, Priority: 10},
			"lo1":    {Name: "lo1", Parent: "root", ShareRatio: 1.0 / 3, Priority: 1},
			"lo2":    {Name: "lo2", Parent: "root", ShareRatio: 1.0 / 3, Priority: 2},
		},
	}
	m, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// TestReclaimAutoReleasesExcessInAction 回收动作内自动归还，
// 无需调用方另行 Release：回收后任一组合计用量不得超过有效硬顶，
// 返回清单的 Released 与实际归还量一一对应。
func TestReclaimAutoReleasesExcessInAction(t *testing.T) {
	m := newBorrowManager(t)
	if err := m.Acquire("hi", "cpu", 100); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Acquire("hi", "cpu", 120); err != nil { // 借 lo1 100 + lo2 20
		t.Fatalf("acquire via borrow: %v", err)
	}

	over := m.ReclaimBorrows()
	if len(over) != 1 {
		t.Fatalf("over entries = %d, want 1", len(over))
	}
	if over[0].Excess != 120 || over[0].Released != 120 {
		t.Fatalf("excess/released = %d/%d, want 120/120", over[0].Excess, over[0].Released)
	}

	// 回收后没有任何组超过有效硬顶（无需调用方再释放）。
	for _, name := range []string{"root", "hi", "lo1", "lo2"} {
		snap, err := m.Snapshot(name)
		if err != nil {
			t.Fatalf("Snapshot %s: %v", name, err)
		}
		u := snap.Resources["cpu"]
		if u.Aggregate > u.Hard {
			t.Fatalf("group %s aggregate %d > hard %d after reclaim", name, u.Aggregate, u.Hard)
		}
	}
	snapHi, _ := m.Snapshot("hi")
	if got := snapHi.Resources["cpu"].Aggregate; got != 100 {
		t.Fatalf("hi aggregate after reclaim = %d, want 100 (at cap)", got)
	}
}

// TestReclaimAutoReleaseTraversesSubtree 超额用量登记在子组上时，
// 系统在回收动作内按确定的子树前序顺序自动归还子组用量。
func TestReclaimAutoReleaseTraversesSubtree(t *testing.T) {
	m := newNestedBorrowManager(t)
	// hi 自身用 40，hi-sub 用 60 → hi 子树合计 100（自有硬顶）。
	if err := m.Acquire("hi", "cpu", 40); err != nil {
		t.Fatalf("hi acquire: %v", err)
	}
	if err := m.Acquire("hi-sub", "cpu", 60); err != nil {
		t.Fatalf("hi-sub acquire: %v", err)
	}
	// hi 子树再借 80：lo1 出让 80（lo1 优先级最低）。
	if err := m.Acquire("hi", "cpu", 80); err != nil {
		t.Fatalf("hi acquire via borrow: %v", err)
	}

	over := m.ReclaimBorrows()
	// 回缩后 hi 子树合计 180，超出硬顶 80。
	// 后序遍历先扣 hi-sub 60，再扣 hi 自身 20（每个叶子登记组独立呈现）。
	if len(over) != 2 {
		t.Fatalf("over entries = %d, want 2 (hi and hi-sub)", len(over))
	}
	if over[0].Group != "hi-sub" || over[0].Released != 60 {
		t.Fatalf("entry0 = %+v, want hi-sub released 60", over[0])
	}
	if over[1].Group != "hi" || over[1].Released != 20 {
		t.Fatalf("entry1 = %+v, want hi released 20", over[1])
	}
	for _, o := range over {
		if o.Excess != 80 {
			t.Fatalf("entry %s excess = %d, want 80 (group-level over at reclaim time)", o.Group, o.Excess)
		}
	}

	snap, err := m.Snapshot("hi")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	u := snap.Resources["cpu"]
	if u.Aggregate != 100 {
		t.Fatalf("hi aggregate = %d, want 100 (at cap after auto-release)", u.Aggregate)
	}
	if u.Used != 100 {
		t.Fatalf("hi own used = %d, want 100 (120-20)", u.Used)
	}
	sub := snap.Children[0].Resources["cpu"]
	if sub.Used != 0 {
		t.Fatalf("hi-sub used = %d, want 0 (60 auto-released)", sub.Used)
	}
}

// TestReclaimMultiResourceIsolation 某一资源的归还异常只影响该资源，
// 不波及其它资源的记账与其它组的额度。
func TestReclaimMultiResourceIsolation(t *testing.T) {
	cfg := &Config{
		Version: 1,
		Groups: map[string]*GroupConfig{
			"root": {Name: "root", Quotas: map[ResourceName]Limit{
				"cpu": {Soft: 300, Hard: 300},
				"mem": {Soft: 300, Hard: 300},
			}},
			"hi":  {Name: "hi", Parent: "root", ShareRatio: 1.0 / 3, Priority: 10},
			"lo1": {Name: "lo1", Parent: "root", ShareRatio: 1.0 / 3, Priority: 1},
			"lo2": {Name: "lo2", Parent: "root", ShareRatio: 1.0 / 3, Priority: 2},
		},
	}
	cpuErr := errors.New("cpu reclaim hook failure")
	var mu sync.Mutex
	var hooks []string
	m, err := NewManager(cfg, nil, WithReclaimHook(func(group string, res ResourceName, amount int64) error {
		mu.Lock()
		hooks = append(hooks, fmt.Sprintf("%s/%s/%d", group, res, amount))
		mu.Unlock()
		if res == "cpu" {
			return cpuErr // 仅 cpu 注入异常
		}
		return nil
	}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// cpu 与 mem 各借满（hi 自有 100，再借 200：lo1 100、lo2 100）。
	for _, res := range []ResourceName{"cpu", "mem"} {
		if err := m.Acquire("hi", res, 100); err != nil {
			t.Fatalf("hi acquire %s 100: %v", res, err)
		}
		if err := m.Acquire("hi", res, 200); err != nil {
			t.Fatalf("hi acquire %s 200 via borrow: %v", res, err)
		}
	}

	over := m.ReclaimBorrows()
	// 资源名升序：cpu 先、mem 后；各 200 超额、各自动归还 200。
	if len(over) != 2 {
		t.Fatalf("over entries = %d, want 2 (cpu, mem)", len(over))
	}
	if over[0].Resource != "cpu" || over[0].Excess != 200 || over[0].Released != 200 {
		t.Fatalf("cpu entry = %+v", over[0])
	}
	if over[1].Resource != "mem" || over[1].Excess != 200 || over[1].Released != 200 {
		t.Fatalf("mem entry = %+v", over[1])
	}
	// cpu 异常只记录在 cpu 项；mem 项不受影响。
	if !errors.Is(over[0].ReclaimError, cpuErr) {
		t.Fatalf("cpu ReclaimError = %v, want injected cpuErr", over[0].ReclaimError)
	}
	if over[1].ReclaimError != nil {
		t.Fatalf("mem ReclaimError = %v, want nil (cpu failure must not spread)", over[1].ReclaimError)
	}

	// 账面归还对两种资源都已完成（回调异常不影响记账）。
	snap, _ := m.Snapshot("hi")
	for _, res := range []ResourceName{"cpu", "mem"} {
		if got := snap.Resources[res].Aggregate; got != 100 {
			t.Fatalf("hi %s aggregate = %d, want 100 (auto-released despite hook error)", res, got)
		}
	}
	// 借出方两种资源都恢复全额。
	for _, lender := range []string{"lo1", "lo2"} {
		ls, _ := m.Snapshot(lender)
		for _, res := range []ResourceName{"cpu", "mem"} {
			if got := ls.Resources[res].Remaining; got != 100 {
				t.Fatalf("%s %s remaining = %d, want 100", lender, res, got)
			}
		}
	}
}

// TestReclaimInterleavedWithConcurrentAcquires 回收与新申请并发交错：
// 任何时刻新请求都按回缩后的剩余额度判定，硬顶始终不被突破。
func TestReclaimInterleavedWithConcurrentAcquires(t *testing.T) {
	m := newBorrowManager(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 不断制造借用占用的写者。
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = m.Acquire("hi", "cpu", 10)
			}
		}()
	}
	// 释放者，制造额度回收空间。
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = m.Release("hi", "cpu", 10)
			}
		}()
	}
	// 反复触发回收：每次返回后硬顶不变量必须成立。
	for i := 0; i < 200; i++ {
		m.mu.Lock()
		over := m.reclaimBorrowsLocked()
		// 在回收尚未释放锁的同一时刻校验：没有任何组仍超其有效硬顶，
		// 且每个超额组的归还量与其回收时点超额头寸一致。
		for name, gs := range m.groups {
			used := subtreeUsed(gs, "cpu")
			capV := capOf(gs, "cpu")
			if used > capV {
				t.Fatalf("after reclaim #%d group %s used %d > cap %d", i, name, used, capV)
			}
		}
		// 按组聚合：每组 Released 之和必须等于其回收时点 Excess。
		releasedByGroup := map[string]int64{}
		for _, o := range over {
			releasedByGroup[o.Group] += o.Released
			if o.Released <= 0 {
				t.Fatalf("entry %+v must report positive released", o)
			}
		}
		seen := map[string]bool{}
		for _, o := range over {
			if seen[o.Group] {
				continue
			}
			seen[o.Group] = true
			if releasedByGroup[o.Group] != o.Excess {
				t.Fatalf("group %s released %d != excess %d", o.Group, releasedByGroup[o.Group], o.Excess)
			}
		}
		m.mu.Unlock()
	}
	close(stop)
	wg.Wait()

	// 收敛：最终再回收一次，所有组必须在硬顶之内。
	m.mu.Lock()
	m.reclaimBorrowsLocked()
	for name, gs := range m.groups {
		if used := subtreeUsed(gs, "cpu"); used > capOf(gs, "cpu") {
			t.Fatalf("final group %s used %d > cap %d", name, used, capOf(gs, "cpu"))
		}
	}
	m.mu.Unlock()
	for _, name := range []string{"root", "hi", "lo1", "lo2"} {
		snap, err := m.Snapshot(name)
		if err != nil {
			t.Fatalf("Snapshot %s: %v", name, err)
		}
		u := snap.Resources["cpu"]
		// 非借出预留口径：合计用量不得超过有效硬顶（root 无借入来源，恒成立）。
		if u.Aggregate > u.Hard {
			t.Fatalf("final group %s aggregate %d > effective hard %d", name, u.Aggregate, u.Hard)
		}
	}
}
