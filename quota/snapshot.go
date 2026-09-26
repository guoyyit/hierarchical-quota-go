package quota

import (
	"fmt"
	"sort"
	"time"
)

// buildSnapshot validates spec and computes all derived quota data, producing
// one immutable snapshot. It never mutates the input spec.
func buildSnapshot(spec ConfigSpec, v Version, createdAt time.Time) (*Snapshot, error) {
	// Deep-copy inputs so later caller mutation cannot change the snapshot.
	resources := make([]ResourceConfig, len(spec.Resources))
	copy(resources, spec.Resources)
	groups := make([]GroupSpec, len(spec.Groups))
	for i, g := range spec.Groups {
		g.Hard = cloneInt64Map(g.Hard)
		g.Soft = cloneInt64Map(g.Soft)
		groups[i] = g
	}

	s := &Snapshot{
		version:     v,
		createdAt:   createdAt,
		resources:   resources,
		groups:      groups,
		parent:      map[string]string{},
		children:    map[string][]string{},
		hard:        map[string]map[string]int64{},
		soft:        map[string]map[string]int64{},
		ancestors:   map[string][]string{},
		subtree:     map[string]map[string]struct{}{},
		resourceSet: map[string]struct{}{},
		groupSet:    map[string]struct{}{},
	}

	for _, r := range resources {
		if r.Name == "" {
			return nil, fmt.Errorf("quota: resource with empty name")
		}
		if _, dup := s.resourceSet[r.Name]; dup {
			return nil, fmt.Errorf("quota: duplicate resource %q", r.Name)
		}
		s.resourceSet[r.Name] = struct{}{}
		s.hard[r.Name] = map[string]int64{}
		s.soft[r.Name] = map[string]int64{}
	}

	// Structural validation.
	rootCount := 0
	for _, g := range groups {
		if g.Name == "" {
			return nil, fmt.Errorf("quota: group with empty name")
		}
		if _, dup := s.groupSet[g.Name]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateGroup, g.Name)
		}
		s.groupSet[g.Name] = struct{}{}
		if g.Weight <= 0 {
			return nil, fmt.Errorf("%w: group %q", ErrWeightNotPositive, g.Name)
		}
		if g.Parent == "" {
			rootCount++
			s.root = g.Name
		} else {
			s.parent[g.Name] = g.Parent
		}
		for res, val := range g.Hard {
			if _, ok := s.resourceSet[res]; !ok {
				return nil, fmt.Errorf("%w: %q in group %q hard map", ErrUnknownResource, res, g.Name)
			}
			if val < Inherit {
				return nil, fmt.Errorf("%w: group %q resource %q", ErrHardBelowInherit, g.Name, res)
			}
		}
		for res, val := range g.Soft {
			if _, ok := s.resourceSet[res]; !ok {
				return nil, fmt.Errorf("%w: %q in group %q soft map", ErrUnknownResource, res, g.Name)
			}
			if val < Inherit {
				return nil, fmt.Errorf("%w: group %q resource %q", ErrSoftBelowZero, g.Name, res)
			}
		}
	}
	if rootCount == 0 {
		return nil, ErrRootMissing
	}
	if rootCount > 1 {
		return nil, fmt.Errorf("%w: multiple root groups", ErrRootMissing)
	}
	for _, g := range groups {
		if g.Parent != "" {
			if _, ok := s.groupSet[g.Parent]; !ok {
				return nil, fmt.Errorf("%w: %q (parent of %q)", ErrParentMissing, g.Parent, g.Name)
			}
		}
	}

	// Build children lists, then BFS from the root. Any group that BFS cannot
	// reach is an orphan or participates in a parent cycle.
	for _, g := range groups {
		if g.Parent != "" {
			s.children[g.Parent] = append(s.children[g.Parent], g.Name)
		}
	}
	for k := range s.children {
		sort.Strings(s.children[k])
	}
	order := make([]string, 0, len(groups))
	frontier := []string{s.root}
	for len(frontier) > 0 {
		cur := frontier[0]
		frontier = frontier[1:]
		order = append(order, cur)
		frontier = append(frontier, s.children[cur]...)
	}
	if len(order) != len(groups) {
		return nil, fmt.Errorf("%w: some groups are unreachable from root", ErrCycle)
	}

	// Ancestor chains (order is parents-before-children, so parent chains are
	// already available).
	for _, name := range order {
		if name == s.root {
			s.ancestors[name] = nil
			continue
		}
		p := s.parent[name]
		chain := make([]string, 0, len(s.ancestors[p])+1)
		chain = append(chain, s.ancestors[p]...)
		chain = append(chain, p)
		s.ancestors[name] = chain
	}

	// Effective limits per resource, top to bottom.
	byName := make(map[string]*GroupSpec, len(groups))
	for i := range groups {
		byName[groups[i].Name] = &groups[i]
	}
	for _, r := range resources {
		for _, name := range order {
			g := byName[name]
			hv, hardPresent := g.Hard[r.Name]
			if name == s.root {
				if !hardPresent || hv == Inherit {
					return nil, fmt.Errorf("%w: root must define explicit hard for resource %q", ErrHardBelowInherit, r.Name)
				}
				s.hard[r.Name][name] = hv
			} else if !hardPresent || hv == Inherit {
				// Inherited share: floor(parentHard * ownWeight / sumSiblingWeights).
				pname := s.parent[name]
				parentHard := s.hard[r.Name][pname]
				var sumW int64
				for _, sib := range s.children[pname] {
					sumW += weightOf(groups, sib)
				}
				share := parentHard * weightOf(groups, name) / sumW
				s.hard[r.Name][name] = share
			} else {
				s.hard[r.Name][name] = hv
			}

			sv, softPresent := g.Soft[r.Name]
			switch {
			case !softPresent || sv == Inherit:
				// Default soft threshold equals the effective hard share.
				s.soft[r.Name][name] = s.hard[r.Name][name]
			default:
				s.soft[r.Name][name] = sv
			}
		}
	}

	// Subtree sets: each group belongs to its own subtree and to the subtree
	// of every ancestor.
	for _, name := range order {
		st := map[string]struct{}{name: {}}
		s.subtree[name] = st
	}
	for _, name := range order {
		for _, a := range s.ancestors[name] {
			s.subtree[a][name] = struct{}{}
		}
	}

	return s, nil
}

func weightOf(groups []GroupSpec, name string) int64 {
	for i := range groups {
		if groups[i].Name == name {
			return groups[i].Weight
		}
	}
	return 1
}

func cloneInt64Map(in map[string]int64) map[string]int64 {
	if in == nil {
		return nil
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// Version returns the snapshot's monotonically increasing version number.
func (s *Snapshot) Version() Version { return s.version }

// CreatedAt returns the commit time of the snapshot.
func (s *Snapshot) CreatedAt() time.Time { return s.createdAt }

// Spec reconstructs the plain configuration backing the snapshot.
func (s *Snapshot) Spec() ConfigSpec {
	out := ConfigSpec{
		Resources: make([]ResourceConfig, len(s.resources)),
		Groups:    make([]GroupSpec, len(s.groups)),
	}
	copy(out.Resources, s.resources)
	for i, g := range s.groups {
		g.Hard = cloneInt64Map(g.Hard)
		g.Soft = cloneInt64Map(g.Soft)
		out.Groups[i] = g
	}
	return out
}

// HasGroup reports whether group exists.
func (s *Snapshot) HasGroup(group string) bool {
	_, ok := s.groupSet[group]
	return ok
}

// HasResource reports whether resource exists.
func (s *Snapshot) HasResource(resource string) bool {
	_, ok := s.resourceSet[resource]
	return ok
}

// ResourceNames returns all configured resources in sorted order.
func (s *Snapshot) ResourceNames() []string {
	names := make([]string, 0, len(s.resources))
	for _, r := range s.resources {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return names
}

// GroupNames returns all configured groups in sorted order.
func (s *Snapshot) GroupNames() []string {
	names := make([]string, 0, len(s.groups))
	for _, g := range s.groups {
		names = append(names, g.Name)
	}
	sort.Strings(names)
	return names
}

// ParentOf returns the parent group id ("" for root and unknown groups).
func (s *Snapshot) ParentOf(group string) string { return s.parent[group] }

// ChildrenOf returns direct child group ids in sorted order.
func (s *Snapshot) ChildrenOf(group string) []string {
	return cloneStringSlice(s.children[group])
}

// EffectiveHard returns the effective hard limit for group/resource.
func (s *Snapshot) EffectiveHard(group, resource string) (int64, bool) {
	rm, ok := s.hard[resource]
	if !ok {
		return 0, false
	}
	v, ok := rm[group]
	return v, ok
}

// EffectiveSoft returns the effective soft limit for group/resource.
func (s *Snapshot) EffectiveSoft(group, resource string) (int64, bool) {
	rm, ok := s.soft[resource]
	if !ok {
		return 0, false
	}
	v, ok := rm[group]
	return v, ok
}

// PriorityOf returns the group priority.
func (s *Snapshot) PriorityOf(group string) int {
	for i := range s.groups {
		if s.groups[i].Name == group {
			return s.groups[i].Priority
		}
	}
	return 0
}
