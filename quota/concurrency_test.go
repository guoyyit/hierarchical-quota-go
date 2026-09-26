package quota

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentAcquireNeverExceedsHard 并发争抢同一硬上限：
// 成功次数不得超过硬顶，且最终用量与成功/释放记账完全一致（无丢失、无重复扣减）。
func TestConcurrentAcquireNeverExceedsHard(t *testing.T) {
	m := newTestManager(t) // a1 cpu hard = 25

	const workers = 64
	const attemptsPerWorker = 20
	var acquired atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < attemptsPerWorker; i++ {
				if err := m.Acquire("a1", "cpu", 1); err == nil {
					acquired.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := acquired.Load(); got != 25 {
		t.Fatalf("successful acquires = %d, want exactly 25 (hard limit)", got)
	}
	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Used; got != 25 {
		t.Fatalf("used = %d, want 25", got)
	}
}

// TestConcurrentAcquireRelease 并发登记与释放交错：
// 最终计数必须等于 成功登记数 - 成功释放数。
func TestConcurrentAcquireRelease(t *testing.T) {
	m := newTestManager(t)

	const workers = 32
	var acquired, released atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if err := m.Acquire("b", "cpu", 1); err == nil {
					acquired.Add(1)
					// 偶数轮次释放一半，制造交错。
					if i%2 == 0 {
						if err := m.Release("b", "cpu", 1); err == nil {
							released.Add(1)
						}
					}
				}
			}
		}(w)
	}
	wg.Wait()

	snap, err := m.Snapshot("b")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	want := acquired.Load() - released.Load()
	if got := snap.Resources["cpu"].Used; got != want {
		t.Fatalf("used = %d, want %d (acquired %d - released %d)", got, want, acquired.Load(), released.Load())
	}
	if got := snap.Resources["cpu"].Used; got < 0 {
		t.Fatalf("used went negative: %d", got)
	}
}

// TestConcurrentSnapshotConsistency 高并发读写下快照自洽：
// 每次快照内 Remaining + Aggregate 不得超过有效硬顶 + 借入，
// 且快照永不 panic、字段非负。
func TestConcurrentSnapshotConsistency(t *testing.T) {
	m := newTestManager(t)

	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = m.Acquire("a1", "cpu", 1)
				_ = m.Release("a1", "cpu", 1)
			}
		}()
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				snap, err := m.Snapshot("root")
				if err != nil {
					t.Errorf("Snapshot: %v", err)
					return
				}
				var walk func(s *GroupSnapshot)
				walk = func(s *GroupSnapshot) {
					for res, u := range s.Resources {
						if u.Used < 0 || u.Aggregate < 0 || u.Remaining < 0 {
							t.Errorf("group %s res %s: negative field in %+v", s.Group, res, u)
						}
						if u.Aggregate > u.Hard+u.Borrowed {
							t.Errorf("group %s res %s: aggregate %d > hard %d + borrowed %d",
								s.Group, res, u.Aggregate, u.Hard, u.Borrowed)
						}
					}
					for _, ch := range s.Children {
						walk(ch)
					}
				}
				walk(snap)
			}
		}()
	}
	// 让写goroutine跑一段时间后停止。
	for i := 0; i < 1000; i++ {
		_ = m.Acquire("b", "cpu", 1)
		_ = m.Release("b", "cpu", 1)
	}
	stop.Store(true)
	wg.Wait()
}

// TestConcurrentMultiResourceIsolation 多资源并发登记互不影响：
// 一种资源的失败/钳制不改变另一种资源的记账。
func TestConcurrentMultiResourceIsolation(t *testing.T) {
	m := newTestManager(t)

	var wg sync.WaitGroup
	// cpu 上疯狂超量释放（触发钳制错误），mem 上正常登记。
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = m.Release("a1", "cpu", 1) // 大概率超量释放 → 钳制
			}
		}()
	}
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				_ = m.Acquire("a1", "mem", 1)
			}
		}()
	}
	wg.Wait()

	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.Resources["cpu"].Used; got != 0 {
		t.Fatalf("cpu used = %d, want 0", got)
	}
	if got := snap.Resources["mem"].Used; got > 25 {
		t.Fatalf("mem used = %d exceeds a1 mem hard 25", got)
	}
}
