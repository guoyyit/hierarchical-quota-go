package quota

import "sync"

// resourceState holds all mutable runtime data for a single resource. Every
// resource has its own mutex so that a problem on one resource never blocks
// or corrupts accounting of another resource. All decisions that read
// counters and update them for this resource happen while mu is held, which
// rules out lost updates, double deduction and over-admission.
type resourceState struct {
	mu sync.Mutex

	// used maps group -> current own consumption.
	used map[string]int64

	// loans are the outstanding loans for this resource.
	loans []BorrowRecord
	// seq is the per-resource monotonic loan sequence; it is the
	// tie-breaker for deterministic reclaim ordering.
	seq int64
}

func newResourceState() *resourceState {
	return &resourceState{used: map[string]int64{}}
}

// stateTable maps resource name to its isolated runtime state.
type stateTable map[string]*resourceState

// newStateTable creates a fresh locked state for every resource.
func newStateTable(resources []string) stateTable {
	st := make(stateTable, len(resources))
	for _, r := range resources {
		st[r] = newResourceState()
	}
	return st
}

// addUsage unconditionally adds delta (which may be negative) to a group's
// own usage. The caller must already hold the resource lock and must have
// validated limits; this function never observes intermediate values.
func (rs *resourceState) addUsage(group string, delta int64) int64 {
	rs.used[group] += delta
	return rs.used[group]
}

// getUsage returns own usage of a group (0 if never seen).
func (rs *resourceState) getUsage(group string) int64 {
	return rs.used[group]
}

// subtreeUsage sums own usage of all groups in members.
func (rs *resourceState) subtreeUsage(members map[string]struct{}) int64 {
	var sum int64
	for g := range members {
		sum += rs.used[g]
	}
	return sum
}

// borrowNet returns (borrowedInto, lentOutOf) across a subtree boundary.
// borrowedInto: loan amount where borrower is inside members and lender is
// outside. lentOutOf: the opposite direction. Internal loans cancel out and
// are counted by neither side.
func (rs *resourceState) borrowNet(members map[string]struct{}) (in, out int64) {
	for _, l := range rs.loans {
		_, bIn := members[l.Borrower]
		_, lIn := members[l.Lender]
		if bIn && !lIn {
			in += l.Amount
		}
		if lIn && !bIn {
			out += l.Amount
		}
	}
	return in, out
}

// addLoan appends a new loan with the next sequence number.
func (rs *resourceState) addLoan(resource, lender, borrower string, amount int64) BorrowRecord {
	rs.seq++
	rec := BorrowRecord{Seq: rs.seq, Resource: resource, Lender: lender, Borrower: borrower, Amount: amount}
	rs.loans = append(rs.loans, rec)
	return rec
}

// loanIndex returns the index of the loan matching (lender, borrower), or -1.
func (rs *resourceState) loanIndex(lender, borrower string) int {
	for i := range rs.loans {
		if rs.loans[i].Lender == lender && rs.loans[i].Borrower == borrower {
			return i
		}
	}
	return -1
}

// reduceLoan reduces the matched loan by amount, removing it when it reaches
// zero. Returns the actually reduced amount.
func (rs *resourceState) reduceLoan(lender, borrower string, amount int64) int64 {
	i := rs.loanIndex(lender, borrower)
	if i < 0 {
		return 0
	}
	taken := amount
	if taken > rs.loans[i].Amount {
		taken = rs.loans[i].Amount
	}
	rs.loans[i].Amount -= taken
	if rs.loans[i].Amount == 0 {
		rs.loans = append(rs.loans[:i], rs.loans[i+1:]...)
	}
	return taken
}

// snapshotLoans returns a copy of all outstanding loans.
func (rs *resourceState) snapshotLoans() []BorrowRecord {
	out := make([]BorrowRecord, len(rs.loans))
	copy(out, rs.loans)
	return out
}

// totalLent returns the total outstanding amount lent by lender.
func (rs *resourceState) totalLent(lender string) int64 {
	var sum int64
	for _, l := range rs.loans {
		if l.Lender == lender {
			sum += l.Amount
		}
	}
	return sum
}

// totalBorrowed returns the total outstanding amount borrowed by borrower.
func (rs *resourceState) totalBorrowed(borrower string) int64 {
	var sum int64
	for _, l := range rs.loans {
		if l.Borrower == borrower {
			sum += l.Amount
		}
	}
	return sum
}

// loansOfLender returns pointers/copies to lender's outstanding loans in
// creation order.
func (rs *resourceState) loansOfLender(lender string) []int {
	idx := make([]int, 0)
	for i := range rs.loans {
		if rs.loans[i].Lender == lender {
			idx = append(idx, i)
		}
	}
	return idx
}
