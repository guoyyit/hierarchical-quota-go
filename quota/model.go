// Package quota implements hierarchical resource quota control.
//
// Workloads are organized as a nested tree of quota groups. Each committed
// configuration is an immutable snapshot; every admission decision is made
// against exactly one snapshot version, so a service can never observe a
// half-updated configuration. See README.md for the full quota semantics.
package quota

import (
	"errors"
	"time"
)

// Inherit is the sentinel value used by GroupSpec.Hard / Soft to express that
// the limit is derived from the parent (see Snapshot.EffectiveHard). Explicit
// zero is a legal value meaning "no capacity", so a negative sentinel is
// required to distinguish inheritance from zero.
const Inherit int64 = -1

// Domain errors returned by the package. Callers should use errors.Is against
// these sentinels to distinguish denial reasons from transport errors.
var (
	// ErrUnknownGroup is returned when a group id is absent from the active
	// configuration snapshot.
	ErrUnknownGroup = errors.New("quota: unknown group")
	// ErrUnknownResource is returned for resources that no committed snapshot
	// defines. Such a request never touches counters of other resources.
	ErrUnknownResource = errors.New("quota: unknown resource")
	// ErrRootMissing is returned when a configuration has no root group.
	ErrRootMissing = errors.New("quota: configuration has no root group")
	// ErrDuplicateGroup is returned when a configuration defines the same
	// group id more than once.
	ErrDuplicateGroup = errors.New("quota: duplicate group id")
	// ErrParentMissing is returned when a non-root group references a parent
	// that does not exist.
	ErrParentMissing = errors.New("quota: missing parent group")
	// ErrCycle is returned when parent links form a cycle or a group is its
	// own ancestor.
	ErrCycle = errors.New("quota: parent cycle detected")
	// ErrSoftBelowZero is returned when an explicit non-inherited soft limit
	// is negative.
	ErrSoftBelowZero = errors.New("quota: soft limit must be >= 0 or Inherit")
	// ErrHardBelowInherit is returned when a hard limit is below Inherit.
	ErrHardBelowInherit = errors.New("quota: hard limit must be >= 0 or Inherit")
	// ErrWeightNotPositive is returned when a group weight is <= 0.
	ErrWeightNotPositive = errors.New("quota: weight must be positive")
	// ErrEmptyRequest is returned when acquire/release carries no resources.
	ErrEmptyRequest = errors.New("quota: empty request")
	// ErrNegativeAmount is returned when an acquire/release amount is < 0.
	ErrNegativeAmount = errors.New("quota: amount must not be negative")
	// ErrHardLimitExceeded is returned (wrapped per resource) when an
	// acquire would break a hard limit on some group/resource.
	ErrHardLimitExceeded = errors.New("quota: hard limit exceeded")
	// ErrNothingToReclaim is returned when a reclaim/revoke target has no
	// outstanding borrowings.
	ErrNothingToReclaim = errors.New("quota: no outstanding borrowings")
	// ErrInsufficientReturn is returned when a borrower cannot return a
	// revoked capacity because its usage leaves no headroom.
	ErrInsufficientReturn = errors.New("quota: borrower cannot return capacity")
	// ErrNoCommitted is returned by load/reload when no committed snapshot
	// can be recovered.
	ErrNoCommitted = errors.New("quota: no committed configuration")
	// ErrConcurrentCommit is returned when two reconfigurations race; the
	// loser must retry against the newer base version.
	ErrConcurrentCommit = errors.New("quota: concurrent configuration commit")
)

// ResourceConfig declares one globally tracked resource dimension, e.g. CPU
// or memory. Every group carries limits per resource name.
type ResourceConfig struct {
	// Name is the unique resource identifier ("cpu", "mem", ...).
	Name string
}

// GroupSpec is one node of a configuration tree.
type GroupSpec struct {
	// Name is the unique, immutable group identifier within a configuration.
	Name string
	// Parent is the parent group id; empty for the root group.
	Parent string
	// Priority orders groups for reclaim: larger value wins, i.e. capacity is
	// reclaimed from the lowest-priority borrower first.
	Priority int
	// Weight is the share used when a hard limit is inherited from the
	// parent; it must be strictly positive.
	Weight int64
	// Hard is the explicit hard limit per resource, Inherit to derive it from
	// the parent, or an explicit non-negative number. The hard limit can
	// never be exceeded by any concurrent admission path.
	Hard map[string]int64
	// Soft is the soft (advisory) limit per resource: Inherit, explicit 0
	// (no soft threshold), or a positive number. Consumption may continue
	// past a soft limit; reports mark the overflow.
	Soft map[string]int64
}

// ConfigSpec is a full configuration candidate. It is plain data, safe to
// build incrementally and to copy; it becomes authoritative only after a
// successful atomic commit.
type ConfigSpec struct {
	// Resources defines the resource universe of the configuration.
	Resources []ResourceConfig
	// Groups is the full set of quota groups forming one tree.
	Groups []GroupSpec
}

// Version uniquely identifies one committed snapshot. Versions are monotonic.
type Version int64

// Snapshot is one immutable, committed configuration. All derived data
// (parent links, ancestor chains, effective limits) is computed once at
// commit time; readers never see partially recomputed fields.
type Snapshot struct {
	version   Version
	createdAt time.Time
	resources []ResourceConfig
	groups    []GroupSpec

	parent   map[string]string
	children map[string][]string
	root     string

	// hard/soft: resource -> group -> effective limit
	hard map[string]map[string]int64
	soft map[string]map[string]int64
	// ancestors: group -> [root ... immediate parent], root has empty chain
	ancestors map[string][]string
	// subtree: group -> group itself + all descendants
	subtree map[string]map[string]struct{}
	// resourceSet: fast membership lookup
	resourceSet map[string]struct{}
	groupSet    map[string]struct{}
}

// Usage is one group's picture for one resource in a query report.
type Usage struct {
	// Used is the group's own current consumption.
	Used int64
	// SubtreeUsed aggregates Used over the group and all its descendants.
	SubtreeUsed int64
	// Hard / Soft are the effective limits for this group/resource.
	Hard int64
	Soft int64
	// BorrowedIn is capacity lent to this group by groups outside its subtree.
	BorrowedIn int64
	// LentOut is capacity this group (or descendants) lent to borrowers
	// outside the subtree.
	LentOut int64
	// OverSoft reports whether current consumption crossed the soft limit.
	OverSoft bool
	// Remaining is headroom against the hard limit: Hard - SubtreeUsed,
	// floored at zero. It already reflects outstanding lends/borrows.
	Remaining int64
}

// GroupReport is one group's slice of a query snapshot.
type GroupReport struct {
	Group     string
	Version   Version
	Usage     map[string]Usage
	Children  []string
	Parent    string
	Priority  int
	StartTime time.Time
}

// BorrowRecord describes one outstanding capacity loan.
type BorrowRecord struct {
	// Seq is the manager-wide monotonic sequence of the loan, used as the
	// tie-breaker for deterministic reclaim order.
	Seq int64
	// Resource is the lent resource dimension.
	Resource string
	// Lender / Borrower are group ids.
	Lender   string
	Borrower string
	// Amount currently outstanding under this loan.
	Amount int64
}

// ReclaimItem describes one over-committed occupancy discovered while a
// loan is revoked. It is emitted in the deterministic reclaim order.
type ReclaimItem struct {
	BorrowRecord
	// Over is the part of the usage that must be released for the borrower to
	// fit the retracted limit (0 when the borrower still fits after return).
	Over int64
	// Returned is the capacity that was actually moved back to the lender.
	Returned int64
}
