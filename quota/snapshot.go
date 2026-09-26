package quota

import (
	"fmt"
	"sort"
)

// GroupSnapshot 某一时间点单个组及其后代的自洽快照。
type GroupSnapshot struct {
	Group     string                 `json:"group"`
	Version   uint64                 `json:"version"` // 快照对应的配置版本
	Resources map[ResourceName]Usage `json:"resources"`
	Children  []*GroupSnapshot       `json:"children,omitempty"`
}

// Usage 单资源的使用与额度视图。
type Usage struct {
	Used      int64 `json:"used"`      // 本组自身用量
	Aggregate int64 `json:"aggregate"` // 含后代的合计用量
	Soft      int64 `json:"soft"`      // 有效软上限
	Hard      int64 `json:"hard"`      // 有效硬上限
	Borrowed  int64 `json:"borrowed"`  // 借入量
	LentOut   int64 `json:"lent_out"`  // 借出（被预留）量
	Remaining int64 `json:"remaining"` // 当前可再消耗量
}

// Snapshot 返回 group 及其后代在某一时间点的自洽快照。
// 快照在同一临界区内构造，高并发读写下结果可复现：
// 任一返回结果都对应某个确定时刻的完整状态。
func (m *Manager) Snapshot(group string) (*GroupSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	gs, ok := m.groups[group]
	if !ok {
		return nil, fmt.Errorf("quota: unknown group %q", group)
	}
	version := m.cfg.Load().Version
	return buildSnapshot(gs, version), nil
}

// buildSnapshot 递归构造子树快照。调用方须持有 m.mu。
func buildSnapshot(gs *groupState, version uint64) *GroupSnapshot {
	snap := &GroupSnapshot{
		Group:     gs.cfg.Name,
		Version:   version,
		Resources: make(map[ResourceName]Usage, len(gs.eff)),
	}
	resNames := make([]string, 0, len(gs.eff))
	for res := range gs.eff {
		resNames = append(resNames, string(res))
	}
	sort.Strings(resNames)
	for _, name := range resNames {
		res := ResourceName(name)
		lim := gs.eff[res]
		remaining := lim.Hard - gs.lentOut[res] + gs.borrowed[res] - subtreeUsed(gs, res)
		if remaining < 0 {
			remaining = 0
		}
		snap.Resources[res] = Usage{
			Used:      gs.usedOwn[res],
			Aggregate: subtreeUsed(gs, res),
			Soft:      lim.Soft,
			Hard:      lim.Hard,
			Borrowed:  gs.borrowed[res],
			LentOut:   gs.lentOut[res],
			Remaining: remaining,
		}
	}
	for _, ch := range gs.children {
		snap.Children = append(snap.Children, buildSnapshot(ch, version))
	}
	return snap
}
