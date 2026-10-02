package quota

import "testing"

// assertSoftConsistent 断言软上限标记与同快照内的用量、软上限严格自洽。
func assertSoftConsistent(t *testing.T, u Usage) {
	t.Helper()
	wantExceeded := u.Aggregate > u.Soft
	if u.SoftExceeded != wantExceeded {
		t.Fatalf("SoftExceeded = %v, want %v (aggregate %d, soft %d)",
			u.SoftExceeded, wantExceeded, u.Aggregate, u.Soft)
	}
	wantExcess := u.Aggregate - u.Soft
	if wantExcess < 0 {
		wantExcess = 0
	}
	if u.SoftExcess != wantExcess {
		t.Fatalf("SoftExcess = %d, want %d (aggregate %d, soft %d)",
			u.SoftExcess, wantExcess, u.Aggregate, u.Soft)
	}
}

// TestSoftExceededFlagAndExcess 软上限标记随用量越过/回落精确变化，
// 且越过软上限不改变继续消耗的规则（硬顶内仍可登记）。
func TestSoftExceededFlagAndExcess(t *testing.T) {
	m := newTestManager(t) // a1: cpu soft 20 / hard 25

	snapOf := func() Usage {
		t.Helper()
		snap, err := m.Snapshot("a1")
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		u := snap.Resources["cpu"]
		assertSoftConsistent(t, u)
		return u
	}

	// 未越过：标记未置位，超出量为 0。
	if err := m.Acquire("a1", "cpu", 20); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if u := snapOf(); u.SoftExceeded || u.SoftExcess != 0 {
		t.Fatalf("at soft limit: got exceeded=%v excess=%d, want false/0", u.SoftExceeded, u.SoftExcess)
	}

	// 越过软顶 3：标记置位，超出量精确为 3；越过后仍可继续登记到硬顶。
	if err := m.Acquire("a1", "cpu", 3); err != nil {
		t.Fatalf("acquire beyond soft: %v", err)
	}
	if u := snapOf(); !u.SoftExceeded || u.SoftExcess != 3 {
		t.Fatalf("beyond soft: got exceeded=%v excess=%d, want true/3", u.SoftExceeded, u.SoftExcess)
	}
	if err := m.Acquire("a1", "cpu", 2); err != nil {
		t.Fatalf("acquire up to hard after crossing soft: %v", err)
	}
	if u := snapOf(); !u.SoftExceeded || u.SoftExcess != 5 {
		t.Fatalf("at hard: got exceeded=%v excess=%d, want true/5", u.SoftExceeded, u.SoftExcess)
	}

	// 释放回落到软顶之下：标记清除，超出量归 0。
	if err := m.Release("a1", "cpu", 10); err != nil {
		t.Fatalf("release: %v", err)
	}
	if u := snapOf(); u.SoftExceeded || u.SoftExcess != 0 {
		t.Fatalf("below soft after release: got exceeded=%v excess=%d, want false/0",
			u.SoftExceeded, u.SoftExcess)
	}
}

// TestSoftExceededCountsBorrowedUsage 通过借用获得的额外用量同样计入软上限判定；
// 回收自动归还超额后，标记随用量回落而清除。
func TestSoftExceededCountsBorrowedUsage(t *testing.T) {
	m := newBorrowManager(t) // hi: cpu soft 100 / hard 100
	if err := m.Acquire("hi", "cpu", 100); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Acquire("hi", "cpu", 50); err != nil { // 借入 50
		t.Fatalf("acquire via borrow: %v", err)
	}

	snap, err := m.Snapshot("hi")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	u := snap.Resources["cpu"]
	assertSoftConsistent(t, u)
	if u.Aggregate != 150 || u.Borrowed != 50 {
		t.Fatalf("aggregate %d borrowed %d, want 150/50", u.Aggregate, u.Borrowed)
	}
	if !u.SoftExceeded || u.SoftExcess != 50 {
		t.Fatalf("borrowed usage must count: got exceeded=%v excess=%d, want true/50",
			u.SoftExceeded, u.SoftExcess)
	}

	// 借出组自身未消耗：合计用量 0，标记不置位。
	snapLo, err := m.Snapshot("lo1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if u := snapLo.Resources["cpu"]; u.SoftExceeded || u.SoftExcess != 0 {
		t.Fatalf("lender should not be soft-exceeded: got exceeded=%v excess=%d",
			u.SoftExceeded, u.SoftExcess)
	}

	// 回收自动归还超额（50）后：用量回到软顶，标记清除。
	m.ReclaimBorrows()
	snap, err = m.Snapshot("hi")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	u = snap.Resources["cpu"]
	assertSoftConsistent(t, u)
	if u.SoftExceeded || u.SoftExcess != 0 {
		t.Fatalf("after reclaim: got exceeded=%v excess=%d, want false/0", u.SoftExceeded, u.SoftExcess)
	}
}

// TestSoftFlagConsistentUnderConcurrency 并发读写下任意时刻读取的快照，
// 软上限标记都与同快照内的用量、软上限自洽。
func TestSoftFlagConsistentUnderConcurrency(t *testing.T) {
	m := newTestManager(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			_ = m.Acquire("a1", "cpu", 1)
			_ = m.Release("a1", "cpu", 1)
		}
	}()
	for i := 0; i < 2000; i++ {
		snap, err := m.Snapshot("root")
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		walkSnapshots(snap, func(s *GroupSnapshot, res ResourceName, u Usage) {
			if u.SoftExceeded != (u.Aggregate > u.Soft) {
				t.Errorf("group %s res %s: flag %v inconsistent with aggregate %d soft %d",
					s.Group, res, u.SoftExceeded, u.Aggregate, u.Soft)
			}
		})
	}
	<-done
}
