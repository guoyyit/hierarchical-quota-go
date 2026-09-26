package quota

import (
	"fmt"
	"sort"
	"sync"
)

// Manager is the entry point of the quota service. It owns one active
// immutable snapshot and the per-resource runtime states.
type Manager struct {
	// mu protects the snap / states pointers. Reconfiguration takes it
	// exclusively, so an admission that read the pointers always finishes
	// against the same single snapshot version.
	mu     sync.RWMutex
	snap   *Snapshot
	states stateTable
	store  Store
}

// NewManager creates a manager with no committed configuration.
func NewManager() *Manager {
	return &Manager{}
}

// NewManagerFromStore creates a manager and recovers the last committed
// snapshot from store. Runtime counters start at zero after recovery.
func NewManagerFromStore(store Store) (*Manager, error) {
	m := &Manager{store: store}
	v, createdAt, spec, err := store.Load()
	if err != nil {
		if err == ErrNoCommitted {
			return m, ErrNoCommitted
		}
		return nil, err
	}
	snap, err := buildSnapshot(spec, v, createdAt)
	if err != nil {
		return nil, fmt.Errorf("quota: recovered configuration invalid: %w", err)
	}
	m.snap = snap
	m.states = newStateTable(snap.ResourceNames())
	return m, nil
}

// ActiveVersion returns the version of the currently active snapshot and
// true; false when nothing has been committed yet.
func (m *Manager) ActiveVersion() (Version, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.snap == nil {
		return 0, false
	}
	return m.snap.Version(), true
}

// AcquireResult reports the outcome of an admitted request.
type AcquireResult struct {
	Version Version
	// OverSoft lists resources whose group subtree crossed its effective
	// soft limit with this acquisition. Hard limits are still enforced.
	OverSoft []string
}

// Acquire atomically checks and registers consumption of several resources
// for group. Every resource is checked against the same snapshot version and
// all involved resource locks are held before any counter is touched, so
// the request is all-or-nothing: a denial never charges anything and an
// anomaly on one resource cannot corrupt accounting of the others.
func (m *Manager) Acquire(group string, req map[string]int64) error {
	_, err := m.AcquireWithInfo(group, req)
	return err
}

// AcquireWithInfo behaves like Acquire and additionally reports soft-limit
// crossings for the admitted request.
func (m *Manager) AcquireWithInfo(group string, req map[string]int64) (res AcquireResult, err error) {
	if len(req) == 0 {
		return res, ErrEmptyRequest
	}
	m.mu.RLock()
	snap, states := m.snap, m.states
	m.mu.RUnlock()
	if snap == nil {
		return res, ErrNoCommitted
	}
	if !snap.HasGroup(group) {
		return res, fmt.Errorf("%w: group %q", ErrUnknownGroup, group)
	}

	names := make([]string, 0, len(req))
	for r, amt := range req {
		if amt < 0 {
			return res, fmt.Errorf("%w: resource %q amount %d", ErrNegativeAmount, r, amt)
		}
		if !snap.HasResource(r) {
			return res, fmt.Errorf("%w: resource %q", ErrUnknownResource, r)
		}
		names = append(names, r)
	}
	sort.Strings(names)

	// Phase 1: acquire every per-resource lock in global order and check.
	// A single deferred unlock (idempotent) plus a panic guard guarantee
	// that locks are released exactly once and that an unexpected fault on
	// one resource surfaces as an error instead of corrupting other
	// resources' accounting.
	locked := make([]*resourceState, 0, len(names))
	released := false
	unlock := func() {
		if !released {
			released = true
			unlockAll(locked)
		}
	}
	defer unlock()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("quota: internal error while admitting request: %v", p)
		}
	}()
	for _, r := range names {
		rs := states[r]
		rs.mu.Lock()
		locked = append(locked, rs)
		if e := checkHard(snap, rs, r, group, req[r]); e != nil {
			unlock()
			return res, e
		}
	}

	// Phase 2: all checks passed against one version; register usage and
	// compute soft crossings. Mutations happen while all locks are held.
	overSoft := map[string]bool{}
	for _, r := range names {
		rs := states[r]
		rs.addUsage(group, req[r])
		soft, _ := snap.EffectiveSoft(group, r)
		if rs.subtreeUsage(snap.subtree[group]) > soft {
			overSoft[r] = true
		}
	}
	for r := range overSoft {
		res.OverSoft = append(res.OverSoft, r)
	}
	sort.Strings(res.OverSoft)
	res.Version = snap.Version()
	unlock()
	return res, nil
}

// checkHard validates one resource against the hard limits of the group and
// all its ancestors. The caller holds the resource lock.
func checkHard(snap *Snapshot, rs *resourceState, resource, group string, amount int64) error {
	chain := snap.ancestors[group]
	for _, g := range chain {
		if err := checkOneHard(snap, rs, resource, g, amount); err != nil {
			return err
		}
	}
	return checkOneHard(snap, rs, resource, group, amount)
}

// checkOneHard projects current subtree consumption across the subtree
// boundary (internal lends cancel, external loans count) and rejects if the
// new amount would cross the effective hard limit.
func checkOneHard(snap *Snapshot, rs *resourceState, resource, g string, amount int64) error {
	members := snap.subtree[g]
	sub := rs.subtreeUsage(members)
	in, out := rs.borrowNet(members)
	occupied := sub + out - in
	hard := snap.hard[resource][g]
	if occupied+amount > hard {
		return fmt.Errorf("%w: group %q resource %q (used %d + request %d > hard %d)",
			ErrHardLimitExceeded, g, resource, occupied, amount, hard)
	}
	return nil
}

// Release returns previously acquired capacity. Like Acquire it is
// all-or-nothing across resources: releasing more than the registered usage
// on any resource rejects the whole request without touching counters.
func (m *Manager) Release(group string, req map[string]int64) (err error) {
	if len(req) == 0 {
		return ErrEmptyRequest
	}
	m.mu.RLock()
	snap, states := m.snap, m.states
	m.mu.RUnlock()
	if snap == nil {
		return ErrNoCommitted
	}
	if !snap.HasGroup(group) {
		return fmt.Errorf("%w: group %q", ErrUnknownGroup, group)
	}
	names := make([]string, 0, len(req))
	for r, amt := range req {
		if amt < 0 {
			return fmt.Errorf("%w: resource %q amount %d", ErrNegativeAmount, r, amt)
		}
		if !snap.HasResource(r) {
			return fmt.Errorf("%w: resource %q", ErrUnknownResource, r)
		}
		names = append(names, r)
	}
	sort.Strings(names)

	locked := make([]*resourceState, 0, len(names))
	released := false
	unlock := func() {
		if !released {
			released = true
			unlockAll(locked)
		}
	}
	defer unlock()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("quota: internal error while releasing: %v", p)
		}
	}()
	for _, r := range names {
		rs := states[r]
		rs.mu.Lock()
		locked = append(locked, rs)
		if rs.getUsage(group) < req[r] {
			unlock()
			return fmt.Errorf("quota: release %d of %q exceeds acquired %d for group %q", req[r], r, rs.getUsage(group), group)
		}
	}
	for _, r := range names {
		states[r].addUsage(group, -req[r])
	}
	unlock()
	return nil
}

// Report returns a self-consistent point-in-time picture of one group. All
// resources are read while their locks are held in one global order, so the
// numbers reflect one instant and repeated reads under load are
// reproducible.
func (m *Manager) Report(group string) (*GroupReport, error) {
	m.mu.RLock()
	snap, states := m.snap, m.states
	m.mu.RUnlock()
	if snap == nil {
		return nil, ErrNoCommitted
	}
	if !snap.HasGroup(group) {
		return nil, fmt.Errorf("%w: group %q", ErrUnknownGroup, group)
	}
	return buildReport(snap, states, group), nil
}

// ReportTree returns reports for group and all its descendants, all read
// under one consistent set of locks.
func (m *Manager) ReportTree(group string) ([]*GroupReport, error) {
	m.mu.RLock()
	snap, states := m.snap, m.states
	m.mu.RUnlock()
	if snap == nil {
		return nil, ErrNoCommitted
	}
	if !snap.HasGroup(group) {
		return nil, fmt.Errorf("%w: group %q", ErrUnknownGroup, group)
	}
	names := snap.ResourceNames()
	locked := lockAll(states, names)
	defer unlockAll(locked)

	groups := make([]string, 0, len(snap.subtree[group]))
	for g := range snap.subtree[group] {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	out := make([]*GroupReport, 0, len(groups))
	for _, g := range groups {
		out = append(out, buildReportLocked(snap, states, g))
	}
	return out, nil
}

func buildReport(snap *Snapshot, states stateTable, group string) *GroupReport {
	names := snap.ResourceNames()
	locked := lockAll(states, names)
	defer unlockAll(locked)
	return buildReportLocked(snap, states, group)
}

func buildReportLocked(snap *Snapshot, states stateTable, group string) *GroupReport {
	rep := &GroupReport{
		Group:     group,
		Version:   snap.Version(),
		Parent:    snap.ParentOf(group),
		Children:  snap.ChildrenOf(group),
		Priority:  snap.PriorityOf(group),
		StartTime: snap.CreatedAt(),
		Usage:     map[string]Usage{},
	}
	for _, r := range snap.ResourceNames() {
		rs := states[r]
		used := rs.getUsage(group)
		sub := rs.subtreeUsage(snap.subtree[group])
		in, out := rs.borrowNet(snap.subtree[group])
		hard, _ := snap.EffectiveHard(group, r)
		soft, _ := snap.EffectiveSoft(group, r)
		occupied := sub + out - in
		remaining := hard - occupied
		if remaining < 0 {
			remaining = 0
		}
		rep.Usage[r] = Usage{
			Used:        used,
			SubtreeUsed: sub,
			Hard:        hard,
			Soft:        soft,
			BorrowedIn:  in,
			LentOut:     out,
			OverSoft:    sub > soft,
			Remaining:   remaining,
		}
	}
	return rep
}

func lockAll(states stateTable, names []string) []*resourceState {
	locked := make([]*resourceState, 0, len(names))
	for _, r := range names {
		rs := states[r]
		rs.mu.Lock()
		locked = append(locked, rs)
	}
	return locked
}

func unlockAll(locked []*resourceState) {
	for i := len(locked) - 1; i >= 0; i-- {
		locked[i].mu.Unlock()
	}
}
