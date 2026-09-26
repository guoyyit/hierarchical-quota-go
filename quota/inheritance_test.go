package quota

import (
	"errors"
	"testing"
)

func TestInheritanceProportionalShares(t *testing.T) {
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "x", Parent: "root", Weight: 2, Hard: map[string]int64{"cpu": Inherit}},
			{Name: "y", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": Inherit}},
			{Name: "z", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": 30}},
		},
	}
	m := NewManager()
	if _, err := m.Commit(spec); err != nil {
		t.Fatal(err)
	}
	snap := m.currentSnap()
	x, _ := snap.EffectiveHard("x", "cpu")
	y, _ := snap.EffectiveHard("y", "cpu")
	z, _ := snap.EffectiveHard("z", "cpu")
	// root=100, weight sum=4 -> 50/25, explicit z=30
	if x != 50 || y != 25 || z != 30 {
		t.Fatalf("shares x=%d y=%d z=%d, want 50/25/30", x, y, z)
	}
	if got := snap.ParentOf("x"); got != "root" {
		t.Fatalf("parent of x = %q", got)
	}
	if kids := snap.ChildrenOf("root"); len(kids) != 3 || kids[0] != "x" {
		t.Fatalf("root children = %v", kids)
	}
}

func TestInheritanceChained(t *testing.T) {
	// root 100 -> p (weight1, inherit) => 100 -> c (weight1, inherit) => 100
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "p", Parent: "root", Weight: 1, Hard: map[string]int64{"cpu": Inherit}},
			{Name: "c", Parent: "p", Weight: 1, Hard: map[string]int64{"cpu": Inherit}},
		},
	}
	snap, err := buildSnapshot(spec, 1, testNow())
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := snap.EffectiveHard("c", "cpu"); h != 100 {
		t.Fatalf("chained inherited hard = %d, want 100", h)
	}
	chain := snap.ancestors["c"]
	if len(chain) != 2 || chain[0] != "root" || chain[1] != "p" {
		t.Fatalf("ancestor chain c = %v, want [root p]", chain)
	}
	if _, ok := snap.subtree["root"]["c"]; !ok {
		t.Fatal("root subtree must contain deep descendant c")
	}
}

func TestExplicitSoftCap(t *testing.T) {
	spec := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups: []GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "g", Parent: "root", Weight: 1,
				Hard: map[string]int64{"cpu": Inherit},
				Soft: map[string]int64{"cpu": 40}},
		},
	}
	m := NewManager()
	if _, err := m.Commit(spec); err != nil {
		t.Fatal(err)
	}
	res, err := m.AcquireWithInfo("g", map[string]int64{"cpu": 41})
	if err != nil {
		t.Fatalf("soft breach must be allowed, got err %v", err)
	}
	if len(res.OverSoft) != 1 || res.OverSoft[0] != "cpu" {
		t.Fatalf("OverSoft = %v, want [cpu]", res.OverSoft)
	}
	rep, _ := m.Report("g")
	if !rep.Usage["cpu"].OverSoft {
		t.Fatal("report must flag OverSoft")
	}
	if err := m.Acquire("g", map[string]int64{"cpu": 59}); err != nil {
		t.Fatalf("up to hard 100 must pass: %v", err)
	}
	if err := m.Acquire("g", map[string]int64{"cpu": 1}); !errors.Is(err, ErrHardLimitExceeded) {
		t.Fatalf("hard breach must be rejected, got %v", err)
	}
}

func TestInvalidConfigs(t *testing.T) {
	base := ConfigSpec{
		Resources: []ResourceConfig{{Name: "cpu"}},
		Groups:    []GroupSpec{{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}}},
	}
	cases := []struct {
		name string
		mut  func(*ConfigSpec)
		want error
	}{
		{"no root", func(s *ConfigSpec) { s.Groups[0].Parent = "ghost" }, ErrRootMissing},
		{"missing parent", func(s *ConfigSpec) {
			s.Groups = append(s.Groups,
				GroupSpec{Name: "a", Parent: "ghost", Weight: 1})
		}, ErrParentMissing},
		{"cycle", func(s *ConfigSpec) {
			s.Groups = append(s.Groups,
				GroupSpec{Name: "a", Parent: "b", Weight: 1},
				GroupSpec{Name: "b", Parent: "a", Weight: 1})
		}, ErrCycle},
		{"duplicate group", func(s *ConfigSpec) {
			s.Groups = append(s.Groups,
				GroupSpec{Name: "root", Parent: "", Weight: 1, Hard: map[string]int64{"cpu": 1}})
		}, ErrDuplicateGroup},
		{"bad weight", func(s *ConfigSpec) {
			s.Groups = append(s.Groups, GroupSpec{Name: "a", Parent: "root", Weight: 0})
		}, ErrWeightNotPositive},
		{"root inherits", func(s *ConfigSpec) {
			s.Groups[0].Hard["cpu"] = Inherit
		}, ErrHardBelowInherit},
		{"unknown resource in map", func(s *ConfigSpec) {
			s.Groups = append(s.Groups, GroupSpec{Name: "a", Parent: "root", Weight: 1,
				Hard: map[string]int64{"gpu": 1}})
		}, ErrUnknownResource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			// fresh deep copy of groups per case
			s.Groups = append([]GroupSpec(nil), base.Groups...)
			tc.mut(&s)
			_, err := buildSnapshot(s, 1, testNow())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
