package quota

import (
	"reflect"
	"sync"
	"testing"
)

// walkSnapshots 遍历快照树并对每个资源视图调用 fn。
func walkSnapshots(s *GroupSnapshot, fn func(s *GroupSnapshot, res ResourceName, u Usage)) {
	for res, u := range s.Resources {
		fn(s, res, u)
	}
	for _, ch := range s.Children {
		walkSnapshots(ch, fn)
	}
}

// assertAllGroupsWithinHard 断言快照树中每组的合计用量都不超过有效硬顶。
func assertAllGroupsWithinHard(t *testing.T, m *Manager) {
	t.Helper()
	snap, err := m.Snapshot("root")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	walkSnapshots(snap, func(s *GroupSnapshot, res ResourceName, u Usage) {
		if u.Aggregate > u.Hard {
			t.Errorf("group %s res %s: aggregate %d exceeds hard %d", s.Group, res, u.Aggregate, u.Hard)
		}
		if u.Used < 0 || u.Aggregate < 0 {
			t.Errorf("group %s res %s: negative usage in %+v", s.Group, res, u)
		}
	})
}

// TestReclaimAutoReleaseMultiResource 回收动作内自动归还超额占用：
// 超额资源被释放回有效硬顶，未超额资源的记账与其它组额度不受波及。
func TestReclaimAutoReleaseMultiResource(t *testing.T) {
	cfg := &Config{
		Version: 1,
		Groups: map[string]*GroupConfig{
			"root": {Name: "root", Quotas: map[ResourceName]Limit{
				"cpu": {Soft: 300, Hard: 300},
				"mem": {Soft: 200, Hard: 200},
			}},
			"hi":  {Name: "hi", Parent: "root", ShareRatio: 1.0 / 3, Priority: 10},
			"lo1": {Name: "lo1", Parent: "root", ShareRatio: 1.0 / 3, Priority: 1},
		},
	}
	m, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// hi：cpu 100（自有）+ 50（借入）；mem 66（自有份额内，未超额）。
	if err := m.Acquire("hi", "cpu", 150); err != nil {
		t.Fatalf("hi acquire cpu: %v", err)
	}
	if err := m.Acquire("hi", "mem", 66); err != nil {
		t.Fatalf("hi acquire mem: %v", err)
	}

	over := m.ReclaimBorrows()
	if len(over) != 1 || over[0].Group != "hi" || over[0].Resource != "cpu" || over[0].Excess != 50 {
		t.Fatalf("over = %+v, want single entry hi/cpu/50", over)
	}

	snap, err := m.Snapshot("hi")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Aggregate; got != 100 {
		t.Fatalf("hi cpu aggregate = %d, want 100 (excess auto-released)", got)
	}
	if got := snap.Resources["mem"].Aggregate; got != 66 {
		t.Fatalf("hi mem aggregate = %d, want 66 (untouched by cpu release)", got)
	}
	// 借出组 lo1 两种资源都恢复全额。
	snapLo, err := m.Snapshot("lo1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snapLo.Resources["cpu"].Remaining; got != 100 {
		t.Fatalf("lo1 cpu remaining = %d, want 100", got)
	}
	if got := snapLo.Resources["mem"].Remaining; got != 66 {
		t.Fatalf("lo1 mem remaining = %d, want 66", got)
	}
	assertAllGroupsWithinHard(t, m)
}

// TestReclaimAutoReleaseDescendants 超额在子树层面释放：
// 先释放本组自身用量，不足部分按确定顺序释放后代用量。
func TestReclaimAutoReleaseDescendants(t *testing.T) {
	cfg := &Config{
		Version: 1,
		Groups: map[string]*GroupConfig{
			"root":   {Name: "root", Quotas: map[ResourceName]Limit{"cpu": {Soft: 400, Hard: 400}}},
			"hi":     {Name: "hi", Parent: "root", ShareRatio: 0.5, Priority: 10},
			"hi-sub": {Name: "hi-sub", Parent: "hi", ShareRatio: 0.5, Priority: 10},
			"lo1":    {Name: "lo1", Parent: "root", ShareRatio: 0.5, Priority: 1},
		},
	}
	m, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// hi-sub 用满自有 100（hi 子树合计 100 ≤ hi 硬顶 200）。
	if err := m.Acquire("hi-sub", "cpu", 100); err != nil {
		t.Fatalf("hi-sub acquire: %v", err)
	}
	// 重配置下调：root 400→160，hi 有效硬顶 80，hi-sub 有效硬顶 40。
	down := &Config{
		Version: 2,
		Groups: map[string]*GroupConfig{
			"root":   {Name: "root", Quotas: map[ResourceName]Limit{"cpu": {Soft: 160, Hard: 160}}},
			"hi":     {Name: "hi", Parent: "root", ShareRatio: 0.5, Priority: 10},
			"hi-sub": {Name: "hi-sub", Parent: "hi", ShareRatio: 0.5, Priority: 10},
			"lo1":    {Name: "lo1", Parent: "root", ShareRatio: 0.5, Priority: 1},
		},
	}
	if err := m.Reconfigure(down); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}

	// 回收：hi 超额 20（自身无用量，从后代 hi-sub 释放）；
	// 随后 hi-sub 仍超额 40，再释放自身 40。顺序确定：组名升序。
	over := m.ReclaimBorrows()
	want := []OverOccupancy{
		{Group: "hi", Resource: "cpu", Excess: 20},
		{Group: "hi-sub", Resource: "cpu", Excess: 40},
	}
	if !reflect.DeepEqual(over, want) {
		t.Fatalf("over = %+v, want %+v", over, want)
	}
	snap, err := m.Snapshot("hi-sub")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Aggregate; got != 40 {
		t.Fatalf("hi-sub aggregate = %d, want 40", got)
	}
	assertAllGroupsWithinHard(t, m)
}

// TestReclaimReproducibleAcrossRuns 相同前置状态下多次触发回收的结果可复现。
func TestReclaimReproducibleAcrossRuns(t *testing.T) {
	run := func() []OverOccupancy {
		m := newBorrowManager(t)
		if err := m.Acquire("hi", "cpu", 100); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if err := m.Acquire("hi", "cpu", 150); err != nil { // lo1 100 + lo2 50
			t.Fatalf("acquire via borrow: %v", err)
		}
		return m.ReclaimBorrows()
	}
	first := run()
	second := run()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("reclaim not reproducible: first %+v, second %+v", first, second)
	}
	want := []OverOccupancy{{Group: "hi", Resource: "cpu", Excess: 150}}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("reclaim = %+v, want %+v", first, want)
	}
}

// TestReclaimConcurrentWithAcquire 回收与并发申请交错：
// 新请求一律按回缩后的剩余额度判定；最终无组超额、无悬挂借用、计数非负。
func TestReclaimConcurrentWithAcquire(t *testing.T) {
	m := newBorrowManager(t)
	if err := m.Acquire("hi", "cpu", 100); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Acquire("hi", "cpu", 150); err != nil { // 超额占用 150
		t.Fatalf("acquire via borrow: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	groups := []string{"hi", "lo1", "lo2"}
	for w := 0; w < 12; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			g := groups[id%len(groups)]
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = m.Acquire(g, "cpu", 1)
				_ = m.Release(g, "cpu", 1)
			}
		}(w)
	}
	// 并发窗口内多次触发回收。
	for i := 0; i < 5; i++ {
		m.ReclaimBorrows()
	}
	close(stop)
	wg.Wait()

	// 收尾回收：清掉交错期间新产生的借用与超额。
	m.ReclaimBorrows()
	if got := len(m.Borrows()); got != 0 {
		t.Fatalf("borrow records after final reclaim = %d, want 0", got)
	}
	assertAllGroupsWithinHard(t, m)
	// 回收后再次触发：无超额，清单为空（幂等）。
	if over := m.ReclaimBorrows(); len(over) != 0 {
		t.Fatalf("reclaim after settling = %+v, want empty", over)
	}
}
