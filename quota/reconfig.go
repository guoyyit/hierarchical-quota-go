package quota

import (
	"fmt"
	"sort"
	"time"
)

// Builder incrementally constructs a ConfigSpec candidate. Local edits live
// only in the builder and are invisible to admission decisions until
// Manager.Commit succeeds atomically.
type Builder struct {
	spec ConfigSpec

	res map[string]struct{}
	grp map[string]int // group name -> index in spec.Groups
}

// NewBuilder starts a candidate. Without arguments it is empty; with a base
// spec it starts from a copy of an existing configuration (typically the
// active Snapshot.Spec()).
func NewBuilder(base ...ConfigSpec) *Builder {
	b := &Builder{
		res: map[string]struct{}{},
		grp: map[string]int{},
	}
	if len(base) > 0 {
		b.spec = cloneSpec(base[0])
		for _, r := range b.spec.Resources {
			b.res[r.Name] = struct{}{}
		}
		for i, g := range b.spec.Groups {
			b.grp[g.Name] = i
		}
	}
	return b
}

// AddResource adds a resource dimension. Adding an existing name is a no-op.
func (b *Builder) AddResource(name string) *Builder {
	if name == "" {
		return b
	}
	if _, ok := b.res[name]; ok {
		return b
	}
	b.res[name] = struct{}{}
	b.spec.Resources = append(b.spec.Resources, ResourceConfig{Name: name})
	return b
}

// SetGroup inserts a new group or replaces an existing one by name.
func (b *Builder) SetGroup(g GroupSpec) *Builder {
	if g.Name == "" {
		return b
	}
	if i, ok := b.grp[g.Name]; ok {
		b.spec.Groups[i] = g
		return b
	}
	b.grp[g.Name] = len(b.spec.Groups)
	b.spec.Groups = append(b.spec.Groups, g)
	return b
}

// RemoveGroup deletes a group from the candidate if present.
func (b *Builder) RemoveGroup(name string) *Builder {
	i, ok := b.grp[name]
	if !ok {
		return b
	}
	b.spec.Groups = append(b.spec.Groups[:i], b.spec.Groups[i+1:]...)
	delete(b.grp, name)
	// Rebuild index.
	for j := range b.spec.Groups {
		b.grp[b.spec.Groups[j].Name] = j
	}
	return b
}

// SetHard / SetSoft are convenience mutators for one group/resource.
func (b *Builder) SetHard(group, resource string, v int64) *Builder {
	i, ok := b.grp[group]
	if !ok {
		return b
	}
	if b.spec.Groups[i].Hard == nil {
		b.spec.Groups[i].Hard = map[string]int64{}
	}
	b.spec.Groups[i].Hard[resource] = v
	return b
}

// SetSoft sets one soft limit; v==Inherit restores inheritance.
func (b *Builder) SetSoft(group, resource string, v int64) *Builder {
	i, ok := b.grp[group]
	if !ok {
		return b
	}
	if b.spec.Groups[i].Soft == nil {
		b.spec.Groups[i].Soft = map[string]int64{}
	}
	b.spec.Groups[i].Soft[resource] = v
	return b
}

// Build returns the assembled candidate. The returned spec is an independent
// copy; further builder edits do not affect it.
func (b *Builder) Build() ConfigSpec { return cloneSpec(b.spec) }

// Commit validates candidate, persists it durably and only then swaps it in
// atomically. Any failure (invalid candidate, removed group still carrying
// usage or loans, store error, version conflict) leaves the previous
// complete version fully in effect.
//
// baseVersion optionally pins the expected current version: when provided
// and different from the active version, Commit returns ErrConcurrentCommit
// without changing anything.
func (m *Manager) Commit(spec ConfigSpec, baseVersion ...Version) (Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.snap != nil && len(baseVersion) > 0 && baseVersion[0] != m.snap.Version() {
		return 0, ErrConcurrentCommit
	}

	next := Version(1)
	if m.snap != nil {
		next = m.snap.Version() + 1
	}

	// Build the new immutable snapshot fully before any side effect.
	newSnap, err := buildSnapshot(spec, next, time.Now())
	if err != nil {
		return 0, err
	}

	// Runtime compatibility: groups/resources that disappear must be clean.
	if m.snap != nil && m.states != nil {
		for res, rs := range m.states {
			if !newSnap.HasResource(res) {
				rs.mu.Lock()
				bad := rs.snapshotLoans()
				var used int64
				for _, g := range m.snap.GroupNames() {
					if !newSnap.HasGroup(g) {
						used += rs.getUsage(g)
					}
				}
				rs.mu.Unlock()
				if len(bad) != 0 || used != 0 {
					return 0, fmt.Errorf("quota: cannot remove resource %q with outstanding usage or loans", res)
				}
				continue
			}
			rs.mu.Lock()
			for _, g := range m.snap.GroupNames() {
				if !newSnap.HasGroup(g) {
					if rs.getUsage(g) != 0 {
						rs.mu.Unlock()
						return 0, fmt.Errorf("%w: removed group %q still has usage on %q", ErrHardLimitExceeded, g, res)
					}
					for _, l := range rs.snapshotLoans() {
						if l.Lender == g || l.Borrower == g {
							rs.mu.Unlock()
							return 0, fmt.Errorf("quota: removed group %q still has loans on %q", g, res)
						}
					}
				}
			}
			rs.mu.Unlock()
		}
	}

	// Persist first. Until Save returns successfully, nothing visible
	// changes; a failure here means total rollback to the old snapshot.
	if m.store != nil {
		if err := m.store.Save(next, newSnap.CreatedAt(), newSnap.Spec()); err != nil {
			return 0, err
		}
	}

	// Swap the immutable snapshot pointer and extend the state table. The
	// state table keeps old entries but admission only walks resources that
	// exist in the new snapshot, and counters for removed groups are simply
	// unreachable (their emptiness was validated above).
	newStates := make(stateTable, len(newSnap.resources))
	for _, r := range newSnap.resources {
		if m.states != nil {
			if rs, ok := m.states[r.Name]; ok {
				newStates[r.Name] = rs
				continue
			}
		}
		newStates[r.Name] = newResourceState()
	}
	m.snap = newSnap
	m.states = newStates

	return next, nil
}

// sortedResourceNames helper (state tables are maps; deterministic ordering
// matters for multi-resource locking and reports).
func sortedNames(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
