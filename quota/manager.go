package quota

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// Manager 分层配额管理器。
// 生效配置以原子指针整体切换，任一时刻只存在某个已提交版本的完整快照。
type Manager struct {
	mu     sync.Mutex // 保护运行态（用量、借用），保证检查与扣减的原子性
	cfg    atomic.Pointer[Config]
	groups map[string]*groupState
	root   *groupState

	borrows      map[uint64]*BorrowRecord
	nextBorrowID uint64

	store Store
}

// NewManager 以给定初始配置创建管理器。
// 若 store 中存在最近一次已提交版本，则从该版本恢复（重启恢复语义）；
// 否则将 initial 作为首个已提交版本落盘。
// 进入已提交状态的配置一律深拷贝，与调用方持有的原对象彻底隔离。
func NewManager(initial *Config, store Store) (*Manager, error) {
	if err := Validate(initial); err != nil {
		return nil, err
	}
	committed := initial.DeepCopy()
	if store != nil {
		loaded, err := store.Load()
		if err != nil {
			return nil, fmt.Errorf("quota: load committed config: %w", err)
		}
		if loaded != nil {
			if err := Validate(loaded); err != nil {
				return nil, fmt.Errorf("quota: committed config invalid: %w", err)
			}
			committed = loaded.DeepCopy()
		} else if err := store.Save(committed); err != nil {
			return nil, fmt.Errorf("quota: persist initial config: %w", err)
		}
	}

	m := &Manager{
		groups:  make(map[string]*groupState),
		borrows: make(map[uint64]*BorrowRecord),
		store:   store,
	}
	m.root = m.buildTree(committed, nil)
	m.cfg.Store(committed)
	return m, nil
}

// Config 返回当前生效配置的独立深拷贝：
// 调用方修改返回值不会影响系统内正在生效的限额。
func (m *Manager) Config() *Config {
	return m.cfg.Load().DeepCopy()
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
// 版本号由系统强制维护：必须严格大于当前已提交版本，
// 重复或回退版本的提交被拒绝，旧配置保持完整生效。
// 校验、版本检查或持久化失败时整体回退到上一个已提交版本；
// 成功后新配置成为唯一生效快照，已登记用量按组名保留。
// 被接受的配置先深拷贝再生效与落盘，与调用方后续修改彻底隔离。
func (m *Manager) Reconfigure(c *Config) error {
	if err := Validate(c); err != nil {
		return err // 校验失败：整体回退，旧版本继续生效
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := m.cfg.Load()
	if c.Version <= cur.Version {
		return fmt.Errorf("quota: config version %d must be greater than current committed version %d",
			c.Version, cur.Version) // 版本重复或回退：拒绝，旧版本继续生效
	}
	committed := c.DeepCopy()
	if m.store != nil {
		if err := m.store.Save(committed); err != nil {
			return fmt.Errorf("quota: persist config: %w", err) // 落盘失败：整体回退
		}
	}
	prev := m.groups
	m.root = m.buildTree(committed, prev)
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
	m.cfg.Store(committed)
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

// ReclaimBorrows 回收借出容量，并在同一临界区内自动释放超额占用。
//
// 整个过程在单个临界区内完成，与并发新请求交错时，
// 新请求只会看到回收前或回收后的完整状态，按回缩后的剩余额度判定：
//  1. 按借用记录 ID 升序的确定顺序归还全部借出容量；
//  2. 按（组名升序、资源名升序）的确定顺序，自动释放各组超出其
//     有效硬上限的合计用量（先释放本组自身用量，再按名称升序深度
//     优先释放后代用量），多次触发结果可复现；
//  3. 返回的超额占用清单与实际释放量一一对应（Excess 即释放量）。
//
// 回收完成后任一组的合计用量不超过其有效硬上限；
// 再次触发回收（无新借用/新占用时）返回空清单，幂等。
// 单个资源的释放异常只影响该资源自身的清单条目，不影响其它资源与其它组。
func (m *Manager) ReclaimBorrows() []OverOccupancy {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 第一步：归还全部借出容量（记录 ID 升序，确定顺序）。
	for _, br := range m.sortedBorrowsLocked() {
		lender := m.groups[br.Lender]
		borrower := m.groups[br.Borrower]
		lender.lentOut[br.Resource] -= br.Amount
		borrower.borrowed[br.Resource] -= br.Amount
		delete(m.borrows, br.ID)
	}

	// 第二步：按确定顺序自动释放超额占用，清单条目与实际释放量一一对应。
	var out []OverOccupancy
	names := make([]string, 0, len(m.groups))
	for name := range m.groups {
		names = append(names, name)
	}
	sort.Strings(names)
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
			released := releaseSubtreeLocked(gs, r, excess)
			if released > 0 {
				out = append(out, OverOccupancy{Group: name, Resource: r, Excess: released})
			}
		}
	}
	return out
}

// releaseSubtreeLocked 在 gs 子树内按确定顺序（先 gs 自身，再按名称升序
// 深度优先遍历后代）释放至多 amount 的 resource 用量，返回实际释放量。
// 只扣减计数，不触碰其它资源与其它子树的记账。调用方须持有 m.mu。
func releaseSubtreeLocked(gs *groupState, res ResourceName, amount int64) int64 {
	if amount <= 0 {
		return 0
	}
	var released int64
	if u := gs.usedOwn[res]; u > 0 {
		take := minInt64(u, amount)
		gs.usedOwn[res] -= take
		released += take
	}
	for _, ch := range gs.children { // children 已按名称升序固定
		if released == amount {
			break
		}
		released += releaseSubtreeLocked(ch, res, amount-released)
	}
	return released
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
