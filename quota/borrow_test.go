package quota

import (
	"errors"
	"testing"
)

func borrowSpec() ConfigSpec {
	return ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			// two equal-weight children -> 50 each
			{Name: "hi", Parent: "root", Weight: 1, Priority: 10, Hard: map[string]int64{"cpu": Inherit}},
			{Name: "lo", Parent: "root", Weight: 1, Priority: 1, Hard: map[string]int64{"cpu": Inherit}},
		},
	}
}

func TestBorrowExpandsAndShrinksHeadroom(t *testing.T) {
	m := NewManager()
	if _, err := m.Commit(borrowSpec()); err != nil {
		t.Fatal(err)
	}
	// lo is idle: hi borrows 40 of lo's 50.
	rec, err := m.Borrow("cpu", "lo", "hi", 40)
	if err != nil {
		t.Fatalf("borrow: %v", err)
	}
	if rec.Seq < 1 {
		t.Fatalf("loan seq = %d", rec.Seq)
	}
	// hi now fits up to 90: 50 own + 40 borrowed
	if err := m.Acquire("hi", map[string]int64{"cpu": 90}); err != nil {
		t.Fatalf("acquire 90 after borrow: %v", err)
	}
	// lending 41 more would exceed lo's lendable 50
	if _, err := m.Borrow("cpu", "lo", "hi", 11); err == nil {
		t.Fatal("second loan must be rejected or merged; duplicate pair expected error")
	}
	// root aggregate still capped at 100 (hi 90 + anything for lo)
	if err := m.Acquire("lo", map[string]int64{"cpu": 11}); !errors.Is(err, ErrHardLimitExceeded) {
		t.Fatalf("lo using 11 while 40 lent must breach root: %v", err)
	}
	if err := m.Acquire("lo", map[string]int64{"cpu": 10}); err != nil {
		t.Fatalf("lo using exactly its remaining 10 must pass: %v", err)
	}
	rep, _ := m.Report("lo")
	if rep.Usage["cpu"].LentOut != 40 {
		t.Fatalf("lo LentOut = %d, want 40", rep.Usage["cpu"].LentOut)
	}
	rep, _ = m.Report("hi")
	if rep.Usage["cpu"].BorrowedIn != 40 || rep.Usage["cpu"].Remaining != 0 {
		t.Fatalf("hi report = %+v", rep.Usage["cpu"])
	}
}

func TestRepayVoluntary(t *testing.T) {
	m := NewManager()
	m.Commit(borrowSpec())
	if _, err := m.Borrow("cpu", "lo", "hi", 40); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("hi", map[string]int64{"cpu": 60}); err != nil {
		t.Fatal(err)
	}
	// hi uses 60, own hard 50 -> over by 10; loan 40. After returning 30 the
	// loan is 10 while hi is over by 10 (fits exactly), so any further
	// voluntary return is impossible without releasing usage.
	if err := m.Repay("cpu", "lo", "hi", 30); err != nil {
		t.Fatalf("repay 30 should fit: %v", err)
	}
	if err := m.Repay("cpu", "lo", "hi", 1); !errors.Is(err, ErrInsufficientReturn) {
		t.Fatalf("want insufficient return, got %v", err)
	}
	// Reclaim can force the last 10 and releases the 10 over-usage.
	items, err := m.Reclaim("cpu", "lo", 10)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(items) != 1 || items[0].Over != 10 || items[0].Returned != 10 {
		t.Fatalf("unexpected reclaim items: %+v", items)
	}
	if len(m.Loans()) != 0 {
		t.Fatalf("all loans should be gone: %+v", m.Loans())
	}
	rep, _ := m.Report("hi")
	if rep.Usage["cpu"].Used != 50 {
		t.Fatalf("hi usage = %d, want 50 after forced release", rep.Usage["cpu"].Used)
	}
}

func TestReclaimOrderAndForcedRelease(t *testing.T) {
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "p1", Parent: "root", Weight: 1, Priority: 5, Hard: map[string]int64{"cpu": Inherit}},
			{Name: "p2", Parent: "root", Weight: 1, Priority: 2, Hard: map[string]int64{"cpu": Inherit}},
			{Name: "p3", Parent: "root", Weight: 1, Priority: 8, Hard: map[string]int64{"cpu": Inherit}},
		},
	}
	m := NewManager()
	m.Commit(spec)
	// root's idle group? everyone shares root; lend from p1/p2/p3 to hi? Use
	// p3 as the reclaiming lender: first create loans p3->p1 and p3->p2.
	if _, err := m.Borrow("cpu", "p3", "p1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Borrow("cpu", "p3", "p2", 10); err != nil {
		t.Fatal(err)
	}
	// borrowers consume the extra: own hard each ~33 (100/3). p1 uses 43, p2 uses 43.
	if err := m.Acquire("p1", map[string]int64{"cpu": 43}); err != nil {
		t.Fatalf("p1 43: %v", err)
	}
	if err := m.Acquire("p2", map[string]int64{"cpu": 43}); err != nil {
		t.Fatalf("p2 43: %v", err)
	}
	// Reclaim all 20: p2 (priority 2) first, then p1 (priority 5).
	items, err := m.Reclaim("cpu", "p3", 20)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2: %+v", len(items), items)
	}
	if items[0].Borrower != "p2" || items[1].Borrower != "p1" {
		t.Fatalf("reclaim order = %s,%s want p2,p1", items[0].Borrower, items[1].Borrower)
	}
	if items[0].Over != 10 || items[1].Over != 10 {
		t.Fatalf("forced releases = %d,%d want 10,10", items[0].Over, items[1].Over)
	}
	rep1, _ := m.Report("p1")
	rep2, _ := m.Report("p2")
	// p1/p2 forced back to their own hard 33 (floor(100/3)).
	if rep1.Usage["cpu"].Used != 33 || rep2.Usage["cpu"].Used != 33 {
		t.Fatalf("usage after reclaim p1=%d p2=%d, want 33/33",
			rep1.Usage["cpu"].Used, rep2.Usage["cpu"].Used)
	}
	if len(m.Loans()) != 0 {
		t.Fatalf("loans after reclaim: %+v", m.Loans())
	}
	// New requests are judged against retracted limits: 34 must fail.
	if err := m.Acquire("p1", map[string]int64{"cpu": 1}); err != nil {
		// p1 at 33 of 33 -> rejected
	} else {
		t.Fatal("expected hard rejection after reclaim")
	}
}

func TestReclaimNothing(t *testing.T) {
	m := NewManager()
	m.Commit(borrowSpec())
	if _, err := m.Reclaim("cpu", "lo", 5); !errors.Is(err, ErrNothingToReclaim) {
		t.Fatalf("got %v, want ErrNothingToReclaim", err)
	}
}

func TestInternalLoanDoesNotDoubleCount(t *testing.T) {
	// root -> a -> a1. Lend within subtree a (a to a1) then check root/a.
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "a", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "a1", Parent: "a", Weight: 1, Hard: map[string]int64{"cpu": 30}},
		},
	}
	m := NewManager()
	m.Commit(spec)
	if _, err := m.Borrow("cpu", "a", "a1", 20); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("a1", map[string]int64{"cpu": 50}); err != nil {
		t.Fatalf("a1 30+20 borrowed = 50: %v", err)
	}
	rep, _ := m.Report("a")
	if rep.Usage["cpu"].SubtreeUsed != 50 || rep.Usage["cpu"].BorrowedIn != 0 || rep.Usage["cpu"].LentOut != 0 {
		t.Fatalf("internal loan must cancel at subtree boundary: %+v", rep.Usage["cpu"])
	}
}

func TestNoReLendingBorrowedCapacity(t *testing.T) {
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 300}},
			{Name: "a", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "b", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "c", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
		},
	}
	m := NewManager()
	m.Commit(spec)
	// a lends 100 to b (all of a's idle capacity).
	if _, err := m.Borrow("cpu", "a", "b", 100); err != nil {
		t.Fatal(err)
	}
	// b now has 100 borrowed + 100 own, but it must not re-lend the borrowed
	// part: at most its own idle 100 is lendable.
	if _, err := m.Borrow("cpu", "b", "c", 150); !errors.Is(err, ErrHardLimitExceeded) {
		t.Fatalf("re-lending borrowed capacity must be denied, got %v", err)
	}
	if _, err := m.Borrow("cpu", "b", "c", 100); err != nil {
		t.Fatalf("lending own idle capacity must be allowed: %v", err)
	}
	// Reverse-direction guard.
	if _, err := m.Borrow("cpu", "c", "b", 1); err == nil {
		t.Fatal("reverse outstanding loan must be rejected")
	}
}

func TestReclaimAcrossSubtree(t *testing.T) {
	// Descendant lends to an outside group; lender's subtree projection
	// includes the lend and revocation still converges.
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "p", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": 80}},
			{Name: "p1", Parent: "p", Weight: 1, Hard: map[string]int64{"cpu": 80}},
			{Name: "q", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": 20}},
		},
	}
	m := NewManager()
	m.Commit(spec)
	if _, err := m.Borrow("cpu", "p1", "q", 20); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("q", map[string]int64{"cpu": 40}); err != nil {
		t.Fatalf("q 20 own + 20 borrowed = 40: %v", err)
	}
	items, err := m.Reclaim("cpu", "p1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Over != 20 || items[0].Returned != 20 {
		t.Fatalf("items = %+v", items)
	}
	rep, _ := m.Report("q")
	if rep.Usage["cpu"].Used != 20 {
		t.Fatalf("q forced back to own hard: %d", rep.Usage["cpu"].Used)
	}
}
