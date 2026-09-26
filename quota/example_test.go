package quota_test

import (
	"errors"
	"fmt"

	"github.com/guoyyit/hierarchical-quota-go/quota"
)

func Example() {
	spec := quota.ConfigSpec{
		Resources: []quota.ResourceConfig{{Name: "cpu"}},
		Groups: []quota.GroupSpec{
			{Name: "root", Weight: 1, Hard: map[string]int64{"cpu": 100}},
			{Name: "hi", Parent: "root", Weight: 1, Priority: 10,
				Hard: map[string]int64{"cpu": quota.Inherit}},
			{Name: "lo", Parent: "root", Weight: 1, Priority: 1,
				Hard: map[string]int64{"cpu": quota.Inherit}},
		},
	}
	m := quota.NewManager()
	v, _ := m.Commit(spec)
	fmt.Println("committed version:", v)

	// hi gets an emergency burst by borrowing idle capacity from lo.
	if _, err := m.Borrow("cpu", "lo", "hi", 40); err != nil {
		panic(err)
	}
	if err := m.Acquire("hi", map[string]int64{"cpu": 90}); err != nil {
		panic(err)
	}
	rep, _ := m.Report("hi")
	fmt.Println("hi used / hard / borrowed:",
		rep.Usage["cpu"].Used, rep.Usage["cpu"].Hard, rep.Usage["cpu"].BorrowedIn)

	// One unit more breaches hi's 50+40 budget.
	err := m.Acquire("hi", map[string]int64{"cpu": 1})
	fmt.Println("over budget:", errors.Is(err, quota.ErrHardLimitExceeded))

	// lo reclaims its capacity: hi (the only borrower) is force-shrunk.
	items, _ := m.Reclaim("cpu", "lo", 40)
	fmt.Println("reclaimed, forced release:", items[0].Returned, items[0].Over)

	// Output:
	// committed version: 1
	// hi used / hard / borrowed: 90 50 40
	// over budget: true
	// reclaimed, forced release: 40 40
}
