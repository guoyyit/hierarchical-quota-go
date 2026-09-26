package quota

import (
	"fmt"
	"sort"
)

// Borrow moves idle capacity from lender to borrower for one resource. It is
// admitted only when the lender subtree still holds lendable headroom
// (effective hard minus its own occupied projection, including already lent
// capacity). Once lent, borrowers immediately gain the headroom in their own
// hard-limit admission checks while the lender's lendable headroom shrinks.
func (m *Manager) Borrow(resource, lender, borrower string, amount int64) (*BorrowRecord, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("%w: amount must be positive", ErrNegativeAmount)
	}
	m.mu.RLock()
	snap, states := m.snap, m.states
	m.mu.RUnlock()
	if snap == nil {
		return nil, ErrNoCommitted
	}
	if !snap.HasResource(resource) {
		return nil, fmt.Errorf("%w: resource %q", ErrUnknownResource, resource)
	}
	if !snap.HasGroup(lender) {
		return nil, fmt.Errorf("%w: lender %q", ErrUnknownGroup, lender)
	}
	if !snap.HasGroup(borrower) {
		return nil, fmt.Errorf("%w: borrower %q", ErrUnknownGroup, borrower)
	}
	if lender == borrower {
		return nil, fmt.Errorf("quota: lender and borrower must differ (%q)", lender)
	}

	rs := states[resource]
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.loanIndex(lender, borrower) >= 0 {
		return nil, fmt.Errorf("quota: outstanding loan from %q to %q already exists", lender, borrower)
	}
	if rs.loanIndex(borrower, lender) >= 0 {
		return nil, fmt.Errorf("quota: outstanding reverse loan between %q and %q already exists", lender, borrower)
	}

	headroom := lendableHeadroom(snap, rs, resource, lender)
	if amount > headroom {
		return nil, fmt.Errorf("%w: lender %q resource %q lendable %d < requested %d",
			ErrHardLimitExceeded, lender, resource, headroom, amount)
	}
	rec := rs.addLoan(resource, lender, borrower, amount)
	out := rec
	return &out, nil
}

// lendableHeadroom is the lender subtree's own, physically idle capacity at
// this instant: hard - subtree usage - already lent out, floored at zero.
// Borrowed-in capacity is deliberately NOT lendable again: allowing re-lending
// of borrowed capacity would create loan chains whose revocation could not be
// satisfied by force-releasing the borrower's own usage. With this rule the
// outstanding lent total never exceeds the lender's own hard, so a reclaim
// can always converge every borrower back to its own budget.
func lendableHeadroom(snap *Snapshot, rs *resourceState, resource, lender string) int64 {
	members := snap.subtree[lender]
	sub := rs.subtreeUsage(members)
	_, out := rs.borrowNet(members)
	hard := snap.hard[resource][lender]
	headroom := hard - sub - out
	if headroom < 0 {
		return 0
	}
	return headroom
}

// Repay moves outstanding borrowed capacity back from borrower to lender. It
// is voluntary: the borrower must currently fit the limit after the loan
// disappears, i.e. its over-limit amount is not larger than (loan-amount),
// otherwise the repayment is rejected with the offending size.
func (m *Manager) Repay(resource, lender, borrower string, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrNegativeAmount)
	}
	m.mu.RLock()
	snap, states := m.snap, m.states
	m.mu.RUnlock()
	if snap == nil {
		return ErrNoCommitted
	}
	rs, ok := states[resource]
	if !ok {
		return fmt.Errorf("%w: resource %q", ErrUnknownResource, resource)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()

	idx := rs.loanIndex(lender, borrower)
	if idx < 0 {
		return ErrNothingToReclaim
	}
	loan := rs.loans[idx].Amount
	take := amount
	if take > loan {
		take = loan
	}
	// Post-repay projection of the borrower subtree against its own hard:
	// usage + lent out - (borrowed in - take).
	members := snap.subtree[borrower]
	sub := rs.subtreeUsage(members)
	in, out := rs.borrowNet(members)
	projected := sub + out - (in - take)
	hard := snap.hard[resource][borrower]
	// Returning take removes that much borrowed headroom. Reject when the
	// borrower would cross its hard; voluntary Repay never force-releases
	// usage (Reclaim does).
	if projected > hard {
		return fmt.Errorf("%w: borrower %q would exceed hard by %d when returning %d",
			ErrInsufficientReturn, borrower, projected-hard, take)
	}
	if rs.reduceLoan(lender, borrower, take) != take {
		return ErrNothingToReclaim
	}
	return nil
}

// Reclaim revokes up to amount of the lender's outstanding loans. Loans are
// processed in a deterministic order: the lowest-priority borrower first,
// then the oldest loan first (sequence number). Every touched loan yields a
// ReclaimItem; when revoking leaves the borrower over its effective hard,
// the excess usage is force-released by the same amount and reported as
// Over. The whole revocation and release happens under the single resource
// lock, so new requests arriving meanwhile are immediately judged against
// the retracted, smaller limits.
func (m *Manager) Reclaim(resource, lender string, amount int64) ([]ReclaimItem, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("%w: amount must be positive", ErrNegativeAmount)
	}
	m.mu.RLock()
	snap, states := m.snap, m.states
	m.mu.RUnlock()
	if snap == nil {
		return nil, ErrNoCommitted
	}
	rs, ok := states[resource]
	if !ok {
		return nil, fmt.Errorf("%w: resource %q", ErrUnknownResource, resource)
	}
	if !snap.HasGroup(lender) {
		return nil, fmt.Errorf("%w: lender %q", ErrUnknownGroup, lender)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()

	idxs := rs.loansOfLender(lender)
	if len(idxs) == 0 {
		return nil, ErrNothingToReclaim
	}
	// Snapshot loan identities first; ledger entries may be deleted as we
	// reclaim, so indices must be re-resolved per iteration.
	pending := make([]BorrowRecord, 0, len(idxs))
	for _, idx := range idxs {
		pending = append(pending, rs.loans[idx])
	}
	sort.SliceStable(pending, func(i, j int) bool {
		pi, pj := snap.PriorityOf(pending[i].Borrower), snap.PriorityOf(pending[j].Borrower)
		if pi != pj {
			return pi < pj // lower priority reclaimed first
		}
		return pending[i].Seq < pending[j].Seq
	})

	items := make([]ReclaimItem, 0, len(pending))
	remaining := amount
	for _, loan := range pending {
		if remaining == 0 {
			break
		}
		// Skip loans already fully consumed by earlier iterations.
		curIdx := rs.loanIndex(loan.Lender, loan.Borrower)
		if curIdx < 0 {
			continue
		}
		take := rs.loans[curIdx].Amount
		if take > remaining {
			take = remaining
		}

		// Determine how much the borrower exceeds its own budget after the
		// loan shrinks by take. borrowNet already counts the loan in `in`;
		// revoking take removes borrowed capacity, so the new projection is
		// occupied + take.
		members := snap.subtree[loan.Borrower]
		sub := rs.subtreeUsage(members)
		in, out := rs.borrowNet(members)
		occupied := sub + out - in
		hard := snap.hard[resource][loan.Borrower]
		over := occupied + take - hard
		forced := int64(0)
		if over > 0 {
			// Release excess from subtree members in deterministic order:
			// borrower itself first, then descendants by name.
			targets := []string{loan.Borrower}
			for g := range members {
				if g != loan.Borrower {
					targets = append(targets, g)
				}
			}
			sort.Strings(targets[1:])
			for _, g := range targets {
				if forced >= over {
					break
				}
				avail := rs.getUsage(g)
				need := over - forced
				rel := avail
				if rel > need {
					rel = need
				}
				if rel > 0 {
					rs.addUsage(g, -rel)
					forced += rel
				}
			}
		}

		returned := rs.reduceLoan(loan.Lender, loan.Borrower, take)
		items = append(items, ReclaimItem{
			BorrowRecord: BorrowRecord{
				Seq:      loan.Seq,
				Resource: resource,
				Lender:   loan.Lender,
				Borrower: loan.Borrower,
				Amount:   loan.Amount - returned,
			},
			Over:     forced,
			Returned: returned,
		})
		remaining -= returned
	}
	return items, nil
}

// Loans returns a snapshot of all outstanding loans across resources, in
// resource-name then sequence order.
func (m *Manager) Loans() []BorrowRecord {
	m.mu.RLock()
	snap, states := m.snap, m.states
	m.mu.RUnlock()
	if snap == nil {
		return nil
	}
	names := snap.ResourceNames()
	locked := lockAll(states, names)
	defer unlockAll(locked)
	var out []BorrowRecord
	for _, r := range names {
		out = append(out, states[r].snapshotLoans()...)
	}
	return out
}
