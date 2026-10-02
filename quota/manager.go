package quota

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// ErrVersionConflict 在提交的配置版本号不能使系统版本严格单调递增
// （重复或回退，或非「当前版本 + 1」）时返回。
// 被拒绝时旧配置保持完整生效。
var ErrVersionConflict = errors.New("quota: config version conflict")

// Option 配置管理器的可选参数。
type Option func(*Manager)

// WithReclaimHook 注册容量回收时的归还通知回调（见 ReclaimHook）。
func WithReclaimHook(h ReclaimHook) Option {
	return func(m *Manager) { m.reclaimHook = h }
}

// Manager 分层配额管理器。
// 生效配置以原子指针整体切换，任一时刻只存在某个已提交版本的完整快照。
type Manager struct {
	mu     sync.Mutex // 保护运行态（用量、借用），保证检查与扣减的原子性
	cfg    atomic.Pointer[Config]
	groups map[string]*groupState
	root   *groupState

	borrows      map[uint64]*BorrowRecord
	nextBorrowID uint64

	store       Store
	reclaimHook ReclaimHook
}

// NewManager 以给定初始配置创建管理器。
// 若 store 中存在最近一次已提交版本，则从该版本恢复（重启恢复语义）；
// 否则将 initial 作为首个已提交版本（系统分配版本号 1）落盘。
// initial 会被深拷贝隔离：调用方此后修改原对象不影响系统生效配置。
func NewManager(initial *Config, store Store, opts ...Option) (*Manager, error) {
	if err := Validate(initial); err != nil {
		return nil, err
	}
	committed := cloneConfig(initial)
	// 首次提交版本号由系统分配为 1；store 非空且存在已提交版本时会被下面的恢复覆盖。
	committed.Version = 1
	if store != nil {
		loaded, err := store.Load()
		if err != nil {
			return nil, fmt.Errorf("quota: load committed config: %w", err)
		}
		if loaded != nil {
			if err := Validate(loaded); err != nil {
				return nil, fmt.Errorf("quota: committed config invalid: %w", err)
			}
			committed = cloneConfig(loaded)
		} else {
			// 首次提交落盘（版本已由系统分配为 1）。
			if err := store.Save(committed); err != nil {
				return nil, fmt.Errorf("quota: persist initial config: %w", err)
			}
		}
	}

	m := &Manager{
		groups:  make(map[string]*groupState),
		borrows: make(map[uint64]*BorrowRecord),
		store:   store,
	}
	for _, opt := range opts {
		opt(m)
	}
	m.root = m.buildTree(committed, nil)
	m.cfg.Store(committed)
	return m, nil
}

// Config 返回当前生效配置的完整快照（只读）。
// 返回的是独立深拷贝：调用方修改副本不会影响系统中正在生效的配置。
func (m *Manager) Config() *Config {
	return cloneConfig(m.cfg.Load())
}

// buildTree 由配置构建配额树并计算每个组的有效上限快照。
// prev 非空时按组名继承已登记用量（重配置时保留用量）。
func (m *Manager) buildTree(c *Config, prev map[string]*groupState) *groupState {
	groups := make(map[string]*groupState, len(c.Groups))
	for name, gc := range c.Groups {
		gs := &groupState{
			cfg:      gc,
			eff:      make(map[ResourceName]Limit),
			usedOwn:  make(map[ResourceName]int64),
			lentOut:  make(map[ResourceName]int64),
			borrowed: make(map[ResourceName]int64),
		}
		if old, ok := prev[name]; ok {
			gs.usedOwn = old.usedOwn
		}
		groups[name] = gs
	}
	var root *groupState
	for _, gs := range groups {
		if gs.cfg.Parent == "" {
			root = gs
			continue
		}
		p := groups[gs.cfg.Parent]
		gs.parent = p
		p.children = append(p.children, gs)
	}
	// 子组排序固定（按名称），保证后续遍历顺序确定。
	for _, gs := range groups {
		sort.Slice(gs.children, func(i, j int) bool {
			return gs.children[i].cfg.Name < gs.children[j].cfg.Name
		})
	}
	var computeEff func(gs *groupState)
	computeEff = func(gs *groupState) {
		gs.eff = computeEffective(gs.cfg, gs.parent)
		for _, ch := range gs.children {
			computeEff(ch)
		}
	}
	computeEff(root)
	m.groups = groups
	return root
}

// computeEffective 计算单组的有效上限：
// 根组取自身配置；子组为自身配置与「父组有效额度 × ShareRatio」的较小者，
// 未配置的资源直接继承父组份额。
func computeEffective(gc *GroupConfig, parent *groupState) map[ResourceName]Limit {
	eff := make(map[ResourceName]Limit)
	if parent == nil {
		for res, l := range gc.Quotas {
			eff[res] = l
		}
		return eff
	}
	for res, pl := range parent.eff {
		sharedHard := int64(float64(pl.Hard) * gc.ShareRatio)
		sharedSoft := int64(float64(pl.Soft) * gc.ShareRatio)
		if own, ok := gc.Quotas[res]; ok {
			hard := minInt64(own.Hard, sharedHard)
			eff[res] = Limit{Soft: minInt64(own.Soft, hard), Hard: hard}
		} else {
			eff[res] = Limit{Soft: minInt64(sharedSoft, sharedHard), Hard: sharedHard}
		}
	}
	return eff
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// subtreeUsed 返回组及其后代对某资源的合计用量。调用方须持有 m.mu。
func subtreeUsed(gs *groupState, res ResourceName) int64 {
	total := gs.usedOwn[res]
	for _, ch := range gs.children {
		total += subtreeUsed(ch, res)
	}
	return total
}

// capOf 组的自有可用上限：有效硬上限 - 借出预留（+ 借入，仅对借入组自身判定）。
func capOf(gs *groupState, res ResourceName) int64 {
	return gs.eff[res].Hard - gs.lentOut[res]
}

// Acquire 尝试为 group 登记 amount 的 resource 用量。
// 检查与扣减在同一临界区内完成，不会允许超额。
// 本组额度不足时按借用规则尝试向有剩余额度的组借用；
// 若瓶颈在祖先组则直接失败（借用无法解决，也不会产生残留记录）。
func (m *Manager) Acquire(group string, res ResourceName, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("quota: acquire amount must be positive, got %d", amount)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	gs, ok := m.groups[group]
	if !ok {
		return fmt.Errorf("quota: unknown group %q", group)
	}
	// 第一阶段：祖先链（借用不能缓解祖先瓶颈）。
	for cur := gs.parent; cur != nil; cur = cur.parent {
		if subtreeUsed(cur, res)+amount > capOf(cur, res) {
			return fmt.Errorf("quota: ancestor group %q resource %q hard limit exceeded (cap %d, used %d, want %d)",
				cur.cfg.Name, res, capOf(cur, res), subtreeUsed(cur, res), amount)
		}
	}
	// 第二阶段：本组（自有额度 + 已借入），不足时尝试借用。
	if subtreeUsed(gs, res)+amount > capOf(gs, res)+gs.borrowed[res] {
		shortfall := subtreeUsed(gs, res) + amount - (capOf(gs, res) + gs.borrowed[res])
		if !m.tryBorrowLocked(gs, res, shortfall) {
			return fmt.Errorf("quota: group %q resource %q hard limit exceeded (cap %d, used %d, want %d)",
				gs.cfg.Name, res, capOf(gs, res), subtreeUsed(gs, res), amount)
		}
		// 借用量恰好覆盖缺口，本组判定必然通过；祖先判定不受借用影响。
	}
	gs.usedOwn[res] += amount
	return nil
}

// Release 释放 group 的 amount 的 resource 用量。
// 释放超过已用量时返回错误并将该资源计数钳制到 0，不影响其它资源。
func (m *Manager) Release(group string, res ResourceName, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("quota: release amount must be positive, got %d", amount)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	gs, ok := m.groups[group]
	if !ok {
		return fmt.Errorf("quota: unknown group %q", group)
	}
	if gs.usedOwn[res] < amount {
		gs.usedOwn[res] = 0
		return fmt.Errorf("quota: group %q resource %q over-release (had %d, want %d); clamped to 0",
			group, res, gs.usedOwn[res], amount)
	}
	gs.usedOwn[res] -= amount
	return nil
}

// Reconfigure 原子地切换到新配置。
// 校验或持久化失败时整体回退到上一个已提交版本；
// 成功后新配置成为唯一生效快照，已登记用量按组名保留。
//
// 版本号由系统自动维护：新配置的版本必须恰好等于「当前已提交版本 + 1」，
// 任何重复、回退或跳跃的提交都以 ErrVersionConflict 拒绝，旧配置保持完整生效。
// 提交内容被深拷贝隔离，调用方此后修改原对象不影响系统生效配置。
func (m *Manager) Reconfigure(c *Config) error {
	if err := Validate(c); err != nil {
		return err // 校验失败：整体回退，旧版本继续生效
	}
	next := cloneConfig(c)
	m.mu.Lock()
	defer m.mu.Unlock()

	current := m.cfg.Load()
	if next.Version != current.Version+1 {
		return fmt.Errorf("%w: got version %d, want %d",
			ErrVersionConflict, next.Version, current.Version+1)
	}

	if m.store != nil {
		if err := m.store.Save(next); err != nil {
			return fmt.Errorf("quota: persist config: %w", err) // 落盘失败：整体回退
		}
	}
	prev := m.groups
	m.root = m.buildTree(next, prev)
	// 过滤掉已不存在组的借用记录，并重放剩余记录以重建借出/借入视图。
	kept := make(map[uint64]*BorrowRecord, len(m.borrows))
	for id, br := range m.borrows {
		if _, ok := m.groups[br.Lender]; !ok {
			continue
		}
		if _, ok := m.groups[br.Borrower]; !ok {
			continue
		}
		kept[id] = br
	}
	m.borrows = kept
	m.replayBorrowsLocked()
	m.cfg.Store(next)
	return nil
}

// replayBorrowsLocked 清零并按记录 ID 升序重放借用，重建确定性的借出/借入视图。
func (m *Manager) replayBorrowsLocked() {
	for _, gs := range m.groups {
		gs.lentOut = make(map[ResourceName]int64)
		gs.borrowed = make(map[ResourceName]int64)
	}
	for _, br := range m.sortedBorrowsLocked() {
		m.groups[br.Lender].lentOut[br.Resource] += br.Amount
		m.groups[br.Borrower].borrowed[br.Resource] += br.Amount
	}
}

// sortedBorrowsLocked 按记录 ID 升序返回借用记录，保证处理顺序确定。
func (m *Manager) sortedBorrowsLocked() []*BorrowRecord {
	ids := make([]uint64, 0, len(m.borrows))
	for id := range m.borrows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*BorrowRecord, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.borrows[id])
	}
	return out
}

// ReclaimBorrows 回收借出容量，并在回收动作内自动归还全部超额占用。
//
// 处理在同一临界区内完成，顺序完全确定、多次触发结果可复现：
//  1. 先按借用记录 ID 升序撤销全部借出登记（借出方恢复额度、借入组失去借入量）；
//  2. 再按「组名升序、资源名升序、子树后序（叶子优先、本组最后）」的确定顺序，
//     将每个组超出其有效硬上限的合计用量从其自身/后代的已登记用量中自动扣减；
//  3. 每个产生实际扣减的（组, 资源）返回一条 OverOccupancy，
//     Released 与系统实际归还的量一一对应，并在回收时点反映该组的超额明细。
//
// 回收完成后任一组的合计用量都不超过其有效硬上限；无需调用方另行释放。
// 回收与新请求在同一互斥临界区内交错，新请求一律按回缩后的剩余额度判定。
// 某资源归还回调（见 WithReclaimHook）报错只记录在对应项的 ReclaimError 中，
// 账面归还照常完成，不波及其它资源的记账与其它组的额度。
func (m *Manager) ReclaimBorrows() []OverOccupancy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reclaimBorrowsLocked()
}

// reclaimBorrowsLocked 为回收主体，调用方须持有 m.mu。
// 单独抽出以便在同一临界区内完成「回收 + 校验」，避免与新请求交错。
func (m *Manager) reclaimBorrowsLocked() []OverOccupancy {
	for _, br := range m.sortedBorrowsLocked() {
		lender := m.groups[br.Lender]
		borrower := m.groups[br.Borrower]
		lender.lentOut[br.Resource] -= br.Amount
		borrower.borrowed[br.Resource] -= br.Amount
		delete(m.borrows, br.ID)
	}

	// 回缩并自动归还：按组名升序处理超额的组，保证结果可复现。
	names := make([]string, 0, len(m.groups))
	for name := range m.groups {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []OverOccupancy
	for _, name := range names {
		gs := m.groups[name]
		resNames := make([]string, 0, len(gs.eff))
		for res := range gs.eff {
			resNames = append(resNames, string(res))
		}
		sort.Strings(resNames)
		for _, res := range resNames {
			r := ResourceName(res)
			excess := subtreeUsed(gs, r) - capOf(gs, r)
			if excess <= 0 {
				continue
			}
			released := m.forceReleaseLocked(gs, r, excess)
			for _, item := range released {
				// 该（组, 资源）的超额头寸一致：Excess 为回收时点超出量，
				// Released 为实际归还量；released 各项之和恰为 excess。
				item.Excess = excess
				if m.reclaimHook != nil {
					item.ReclaimError = m.reclaimHook(item.Group, item.Resource, item.Released)
				}
				out = append(out, item)
			}
		}
	}
	return out
}

// forceReleaseLocked 在不依赖调用方释放的前提下，按确定顺序从 gs 子树中
// 强制扣减 want 用量，使 subtreeUsed(gs,res) 回到有效硬上限之内。
// 遍历顺序为子树后序（先子组后本组，子组按名称升序），优先释放叶子用量、
// 最后才扣减本组自身，使每个具体登记组的超额头寸都能独立呈现，
// 返回每个实际被扣减的（组, 资源）一条记录（Released 为实际扣减量）。
// 调用方须持有 m.mu。
func (m *Manager) forceReleaseLocked(gs *groupState, res ResourceName, want int64) []OverOccupancy {
	var out []OverOccupancy
	var walk func(node *groupState)
	walk = func(node *groupState) {
		for _, ch := range node.children {
			walk(ch)
		}
		if want > 0 && node.usedOwn[res] > 0 {
			take := node.usedOwn[res]
			if take > want {
				take = want
			}
			node.usedOwn[res] -= take
			want -= take
			out = append(out, OverOccupancy{
				Group:    node.cfg.Name,
				Resource: res,
				Released: take,
			})
		}
	}
	walk(gs)
	return out
}

// Borrows 返回当前借用记录的只读副本（按 ID 升序）。
func (m *Manager) Borrows() []*BorrowRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.sortedBorrowsLocked()
	out := make([]*BorrowRecord, 0, len(src))
	for _, br := range src {
		cp := *br
		out = append(out, &cp)
	}
	return out
}
