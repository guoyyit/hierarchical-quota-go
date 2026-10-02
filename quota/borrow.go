package quota

import "sort"

// BorrowRecord 一条借用记录：Lender 将 resource 的 Amount 额度借给 Borrower。
type BorrowRecord struct {
	ID       uint64       `json:"id"`
	Lender   string       `json:"lender"`
	Borrower string       `json:"borrower"`
	Resource ResourceName `json:"resource"`
	Amount   int64        `json:"amount"`
}

// OverOccupancy 容量回收后某组对某资源的超额占用项。
type OverOccupancy struct {
	Group    string       `json:"group"`
	Resource ResourceName `json:"resource"`
	// Excess 为回缩后超出其自有有效额度的部分。
	Excess int64 `json:"excess"`
	// Released 为回收动作内系统自动从该组该资源归还/释放的量，与 Excess 一一对应。
	Released int64 `json:"released"`
	// ReclaimError 记录本资源自动归还过程中出现的异常；
	// 该异常只影响本项，不影响其它资源的记账与其它组的额度。
	ReclaimError error `json:"-"`
}

// ReclaimHook 回收回调：回收动作内某组某资源被系统强制归还用量后调用，
// group/res/amount 与返回清单中的项一一对应。
// 回调返回的错误只记录在对应超额项的 ReclaimError 中：
// 账面用量已在系统侧释放完成，错误不影响其它资源与其它组。
type ReclaimHook func(group string, res ResourceName, amount int64) error

// groupState 配额树中一个组的运行态。
type groupState struct {
	cfg      *GroupConfig
	parent   *groupState
	children []*groupState

	// eff 为按继承规则算出的有效上限快照。
	eff map[ResourceName]Limit
	// usedOwn 该组自身登记的用量（不含后代）。
	usedOwn map[ResourceName]int64
	// lentOut 被其它组借走、需从本组可用额度中预留的量。
	lentOut map[ResourceName]int64
	// borrowed 本组从其它组借来的可用量。
	borrowed map[ResourceName]int64
}

// isAncestorOf 报告 a 是否为 b 的祖先（含 b 自身）。
func isAncestorOf(a, b *groupState) bool {
	for cur := b; cur != nil; cur = cur.parent {
		if cur == a {
			return true
		}
	}
	return false
}

// tryBorrowLocked 为 borrower 尝试从有剩余额度的组借用 shortfall 量。
// 成功返回 true 并已更新借出/借入视图；失败返回 false 且不产生任何记录。
//
// 借用规则：
//   - 只有优先级严格更高的组才能向其它组借用；
//   - 借出方不得与借入方互为祖先（避免额度在祖链上重复计算）；
//   - 借出方按（优先级升序，名称升序）的确定顺序依次出让剩余额度；
//   - 剩余额度不足 shortfall 时整体失败，不产生任何记录。
func (m *Manager) tryBorrowLocked(borrower *groupState, res ResourceName, shortfall int64) bool {
	if shortfall <= 0 {
		return true
	}
	// 已超额占用（超出自有有效额度）的组须先释放回自有额度之内，
	// 才允许再次借用：回收后的新请求按回缩后的剩余量判定。
	if subtreeUsed(borrower, res) > capOf(borrower, res) {
		return false
	}
	type candidate struct {
		gs      *groupState
		surplus int64
	}
	var cands []candidate
	for _, gs := range m.groups {
		if gs == borrower || isAncestorOf(gs, borrower) || isAncestorOf(borrower, gs) {
			continue
		}
		if gs.cfg.Priority >= borrower.cfg.Priority {
			continue
		}
		if s := capOf(gs, res) - subtreeUsed(gs, res); s > 0 {
			cands = append(cands, candidate{gs: gs, surplus: s})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].gs.cfg.Priority != cands[j].gs.cfg.Priority {
			return cands[i].gs.cfg.Priority < cands[j].gs.cfg.Priority
		}
		return cands[i].gs.cfg.Name < cands[j].gs.cfg.Name
	})

	var total int64
	for _, c := range cands {
		total += c.surplus
	}
	if total < shortfall {
		return false
	}

	remaining := shortfall
	for _, c := range cands {
		if remaining == 0 {
			break
		}
		take := c.surplus
		if take > remaining {
			take = remaining
		}
		m.nextBorrowID++
		m.borrows[m.nextBorrowID] = &BorrowRecord{
			ID:       m.nextBorrowID,
			Lender:   c.gs.cfg.Name,
			Borrower: borrower.cfg.Name,
			Resource: res,
			Amount:   take,
		}
		c.gs.lentOut[res] += take
		borrower.borrowed[res] += take
		remaining -= take
	}
	return true
}
