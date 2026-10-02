package quota

import "testing"

// TestSoftLimitFlagConsistentWithinSnapshot 软上限标记与同一快照内的
// Aggregate/Soft 严格自洽：越过置位并给出精确超出量，未越过不置位。
func TestSoftLimitFlagConsistentWithinSnapshot(t *testing.T) {
	m := newTestManager(t) // a1 cpu: soft 20 / hard 25

	// 未越过：聚合 10 <= soft 20。
	if err := m.Acquire("a1", "cpu", 10); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	snap, err := m.Snapshot("a1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	u := snap.Resources["cpu"]
	if u.SoftExceeded {
		t.Fatalf("soft_exceeded = true at aggregate %d <= soft %d", u.Aggregate, u.Soft)
	}
	if u.SoftOverflow != 0 {
		t.Fatalf("soft_overflow = %d, want 0 when not exceeded", u.SoftOverflow)
	}

	// 恰好等于软顶不算越过。
	if err := m.Acquire("a1", "cpu", 10); err != nil {
		t.Fatalf("acquire to soft: %v", err)
	}
	snap, _ = m.Snapshot("a1")
	u = snap.Resources["cpu"]
	if u.SoftExceeded || u.Aggregate != 20 || u.SoftOverflow != 0 {
		t.Fatalf("at soft boundary got exceeded=%v overflow=%d aggregate=%d soft=%d",
			u.SoftExceeded, u.SoftOverflow, u.Aggregate, u.Soft)
	}

	// 越过软顶但未到硬顶：标记置位，超出量精确为 2。
	if err := m.Acquire("a1", "cpu", 2); err != nil {
		t.Fatalf("acquire beyond soft: %v", err)
	}
	snap, _ = m.Snapshot("a1")
	u = snap.Resources["cpu"]
	if !u.SoftExceeded {
		t.Fatalf("soft_exceeded = false at aggregate %d > soft %d", u.Aggregate, u.Soft)
	}
	if u.SoftOverflow != 2 {
		t.Fatalf("soft_overflow = %d, want 2", u.SoftOverflow)
	}
	if u.Aggregate-u.Soft != u.SoftOverflow {
		t.Fatalf("overflow %d != aggregate %d - soft %d", u.SoftOverflow, u.Aggregate, u.Soft)
	}
	// 越过软顶不改变继续消耗规则：直到硬顶 25 仍允许，超过则拒。
	if err := m.Acquire("a1", "cpu", 3); err != nil {
		t.Fatalf("acquire up to hard should still succeed: %v", err)
	}
	if err := m.Acquire("a1", "cpu", 1); err == nil {
		t.Fatal("hard limit must remain unbreakable after soft is exceeded")
	}
}

// TestSoftLimitFlagIncludesBorrowedUsage 借用得到的额外用量计入软上限判定。
func TestSoftLimitFlagIncludesBorrowedUsage(t *testing.T) {
	m := newBorrowManager(t) // hi cpu: soft == hard == 100
	if err := m.Acquire("hi", "cpu", 100); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	snap, _ := m.Snapshot("hi")
	if got := snap.Resources["cpu"].Soft; got != 100 {
		t.Fatalf("hi soft = %d, want 100", got)
	}
	if snap.Resources["cpu"].SoftExceeded {
		t.Fatal("hi should not be over soft at 100")
	}

	// 借用 30：聚合 130 > 软顶 100，标记必须置位（借用用量参与判定）。
	if err := m.Acquire("hi", "cpu", 30); err != nil {
		t.Fatalf("acquire via borrow: %v", err)
	}
	snap, _ = m.Snapshot("hi")
	u := snap.Resources["cpu"]
	if !u.SoftExceeded {
		t.Fatalf("borrowed usage must count toward soft: aggregate %d soft %d", u.Aggregate, u.Soft)
	}
	if u.SoftOverflow != 30 {
		t.Fatalf("soft_overflow = %d, want 30", u.SoftOverflow)
	}

	// 回收并自动归还后回到硬顶 100，软上限标记随之清除。
	m.ReclaimBorrows()
	snap, _ = m.Snapshot("hi")
	u = snap.Resources["cpu"]
	if u.Aggregate != 100 || u.SoftExceeded || u.SoftOverflow != 0 {
		t.Fatalf("after reclaim aggregate=%d exceeded=%v overflow=%d, want 100/false/0",
			u.Aggregate, u.SoftExceeded, u.SoftOverflow)
	}
}

// TestSoftLimitFlagReflectsDescendants 父组软上限判定基于含后代的合计用量。
func TestSoftLimitFlagReflectsDescendants(t *testing.T) {
	m := newTestManager(t) // root cpu: soft 80 / hard 100；a,b 各 hard 50
	if err := m.Acquire("a", "cpu", 40); err != nil {
		t.Fatalf("a acquire: %v", err)
	}
	if err := m.Acquire("b", "cpu", 45); err != nil {
		t.Fatalf("b acquire: %v", err)
	}
	snap, err := m.Snapshot("root")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	u := snap.Resources["cpu"]
	if u.Aggregate != 85 {
		t.Fatalf("root aggregate = %d, want 85", u.Aggregate)
	}
	if !u.SoftExceeded || u.SoftOverflow != 5 {
		t.Fatalf("root soft: exceeded=%v overflow=%d, want true/5", u.SoftExceeded, u.SoftOverflow)
	}
	// 子组标记同样自洽：a=40 恰好等于继承软顶 40（未越过），b=45 越过 5。
	for _, ch := range snap.Children {
		cu := ch.Resources["cpu"]
		switch ch.Group {
		case "a":
			if cu.SoftExceeded || cu.SoftOverflow != 0 {
				t.Fatalf("a should be at soft (40/40), got %+v", cu)
			}
		case "b":
			if !cu.SoftExceeded || cu.SoftOverflow != 5 {
				t.Fatalf("b should exceed soft by 5, got %+v", cu)
			}
		}
	}
}
