# 分层资源配额控制（hierarchical-quota-go）

把工作负载组织成**可嵌套的配额组树**，在父子组之间按权重传递额度，允许高优先级组
向有余量的组借用容量，并提供原子重配置、故障回退与重启恢复。所有生效配置都是
**不可变快照**，任何一次配额判定都只对应一个已提交版本，不会出现半更新状态。

## 1. 核心概念

| 概念 | 说明 |
| --- | --- |
| 资源 Resource | 一个可计费维度，如 `cpu`、`mem`。各资源使用独立的锁与台账，互不阻塞、互不污染。 |
| 配额组 Group | 树上的一个节点，有唯一名称、父节点、权重、优先级、每资源的硬/软上限。 |
| 快照 Snapshot | 一次已提交的完整配置（版本号单调递增）。派生数据（父子链、祖先链、生效限额、子树集合）在提交时一次性算好，之后只读。 |
| 使用量 Used | 组自身的实时消耗；子树使用量 = 本组及全部后代使用量之和。 |
| 借用 Loan | 出借方把自身空闲容量让给借用方的一条台账记录。 |

## 2. 配置项（`ConfigSpec` / `GroupSpec`）

```go
spec := quota.ConfigSpec{
    Resources: []quota.ResourceConfig{{Name: "cpu"}, {Name: "mem"}},
    Groups: []quota.GroupSpec{
        {Name: "root", Weight: 1,
         Hard: map[string]int64{"cpu": 100, "mem": 1000}},
        {Name: "a", Parent: "root", Weight: 3, Priority: 10,
         Hard: map[string]int64{"cpu": quota.Inherit, "mem": 600},
         Soft: map[string]int64{"cpu": 200}},
        {Name: "b", Parent: "root", Weight: 1, Priority: 1,
         Hard: map[string]int64{"cpu": quota.Inherit, "mem": quota.Inherit}},
    },
}
```

| 字段 | 含义 |
| --- | --- |
| `Name` | 组唯一标识；必须存在且仅存在一个根组（`Parent == ""`）。 |
| `Parent` | 父组名；根组为空。父链必须连通且无环。 |
| `Weight` | 继承父额度时的分享权重，**必须 > 0**。 |
| `Priority` | 回收顺序依据，值越大优先级越高；容量被回收时优先从**低优先级**借用方收回。 |
| `Hard[r]` | 资源 `r` 的硬上限：非负显式值，或 `Inherit(-1)` 表示按权重继承。根组对每个资源必须给显式值。硬上限在任何并发路径下都不得突破。 |
| `Soft[r]` | 软上限：正整数为阈值；`0` 表示不设软阈值（此时默认等于生效硬上限）；`Inherit` 同样回落到生效硬上限。超过软上限**仍可消耗**，仅在结果里标记 `OverSoft`。 |

### 继承规则

- 某组对资源 `r` 未显式设置硬上限（缺省或 `Inherit`）时：

  ```
  effectiveHard(child) = floor(effectiveHard(parent) * child.Weight / Σ 兄弟权重)
  ```

  即兄弟组按权重比例切分**父组的生效额度**（不是父组被显式占用后的剩余）。
- 显式硬上限不会被子孙反推修改；由于按祖先链逐级判定，所有子孙消耗之和仍受父额度约束。
- 软上限缺省/继承时等于本组生效硬上限。
- 父组上调或下调额度并重新提交后，所有继承份额随新快照一次性收敛；收敛期间正在进行的
  判定要么基于旧版本、要么基于新版本，绝不会混用字段。

## 3. 提交与原子重配置

```go
m := quota.NewManager()
v, err := m.Commit(spec) // v == 1
```

- `Commit` 先在内存里构建并校验**完整**新快照，再校验运行时兼容性，然后持久化，
  **最后**才原子替换当前快照指针。任一步失败，旧版本完整保留，不产生半更新。
- 乐观并发控制：`m.Commit(spec, baseVersion)` 在基线版本过期时返回
  `ErrConcurrentCommit`，调用方应基于新版本重试。
- 运行时兼容规则：被删除的组必须使用量为 0 且没有未结借用；被删除的资源必须无任何
  使用量与借用。否则提交被拒绝并整体回退。
- 未提交的中间改动只存在于 `Builder` 中，对线上判定不可见，也不会落盘。

```go
b := quota.NewBuilder(oldSnap.Spec()) // 从当前版本派生
b.SetHard("root", "cpu", 200)
v2, err := m.Commit(b.Build(), oldSnap.Version())
```

## 4. 登记与释放

```go
err := m.Acquire("a", map[string]int64{"cpu": 10, "mem": 100})
m.Release("a", map[string]int64{"cpu": 10})
info, _ := m.AcquireWithInfo("a", map[string]int64{"cpu": 5}) // info.OverSoft
```

- **判定口径**：对申请组及其全部祖先逐级检查硬上限。对任意组 `g`，其“占用投影”为

  ```
  occupied(g) = 子树已用 + 向子树外借出 - 从子树外借入
  ```

  子树内部的借贷在边界处相互抵消，不会被重复计数。`occupied + 申请量 <= effectiveHard`
  对组自身及每个祖先都成立才放行。
- **全有或全无**：一次多资源申请，按资源名全局排序依次加锁并全部检查通过后才统一记账；
  任一资源被拒绝，所有资源都不扣减。
- 计数与检查在同一把资源锁内完成，杜绝计数丢失、重复扣减与超额放行（`go test -race` 覆盖）。
- 释放同样是全有或全无：某资源释放量大于已登记量会整体拒绝，不会产生负计数。
- 单资源的异常被局限在该资源：每个资源独立加锁，内部异常转为错误返回，不影响其它资源计费。
- **硬上限不可突破**（包括软上限溢出、借用回收后的回缩判定）；**单组超预算不波及其它组**：
  缩容后已超额的组被冻结在超额状态，释放前无法再申请，其它组照常。

## 5. 借用、归还与回收

```go
rec, err := m.Borrow("cpu", lender="b", borrower="a", 30)
err = m.Repay("cpu", "b", "a", 30)                 // 自愿归还
items, err := m.Reclaim("cpu", lender="b", 30)     // 强制回收
```

- **可出借量**：`effectiveHard(lender) - 子树已用 - 已向子树外借出`，下限为 0。
  **借入的容量不得再次转借**（避免借贷链导致回收无法补平）；同一对组之间也不能存在
  反向未结贷款。
- 借用成立后，借用方子树立即获得相应硬上限空间，出借方的可出借量立即收缩。
- `Repay` 是自愿归还：归还后若借用方会超出自身硬上限，则拒绝（`ErrInsufficientReturn`），
  它不会替调用方强杀负载；需要强杀请用 `Reclaim`。
- `Reclaim` 按**确定顺序**收回：先按借用方 `Priority` 升序（低优先级先被回收），
  同优先级按贷款序号 `Seq` 升序（先借先收）。
- 回收导致借用方超出回缩后额度时，系统在同一把锁内**强制释放**多余使用量，释放范围按
  “借用方自身 → 后代组名升序”确定，并在返回的 `ReclaimItem.Over` 中给出超额占用清单，
  `Returned` 给出实际归还量。回收与释放是原子的，此后的新请求立即按回缩后的剩余量判定。

## 6. 持久化与重启恢复

```go
store := quota.NewFileStore("/var/lib/quota/state.json")
m, err := quota.NewManagerFromStore(store)
```

- 提供 `MemoryStore`（测试用）与 `FileStore`：`FileStore` 写临时文件 + `fsync` +
  原子 `rename`，崩溃时只会留下上一个完整文件或新完整文件，不会出现残缺 JSON。
- **只有已提交快照落盘**；`Builder` 的中间编辑永不持久化。重启后恢复最近一次已提交配置；
  运行时使用量属于易失状态，重启后从 0 开始（配置完整恢复）。
- 存储文件损坏会返回错误，而不是加载半截配置。

## 7. 查询

```go
rep, _ := m.Report("a")                 // 单组某时间点的自洽快照
tree, _ := m.ReportTree("root")         // 一组及其全部后代
loans := m.Loans()                      // 全部未结借用
```

`GroupReport.Usage[resource]` 包含：`Used`（自身）、`SubtreeUsed`（子树）、
`Hard`/`Soft`（生效上限）、`BorrowedIn`/`LentOut`（跨子树边界的借贷净额）、
`OverSoft`、`Remaining = max(0, Hard - SubtreeUsed - LentOut + BorrowedIn)`。

查询按资源名全局排序加锁读取，返回的是**同一时间点**的自洽快照；与高并发读写同时进行时
结果可复现、不会撕裂。

## 8. 并发模型小结

- 管理器读写锁只保护“当前快照指针 / 状态表指针”；`Commit` 取写锁做指针级原子切换，
  业务路径取读锁拿到指针后，全程只与这一个不可变快照打交道。
- 每个资源一把互斥锁；多资源操作一律按资源名排序加锁，避免死锁。
- 读多写少：所有派生结构在提交时预计算并只读共享。

## 9. 错误处理要点

判定类错误均为包级哨兵错误，建议用 `errors.Is`：
`ErrHardLimitExceeded`、`ErrUnknownGroup`、`ErrUnknownResource`、`ErrEmptyRequest`、
`ErrNegativeAmount`、`ErrInsufficientReturn`、`ErrNothingToReclaim`、
`ErrConcurrentCommit`、`ErrNoCommitted`，以及配置校验错误 `ErrRootMissing`、
`ErrDuplicateGroup`、`ErrParentMissing`、`ErrCycle`、`ErrWeightNotPositive` 等。

## 10. 运行测试

```bash
go test ./... -race -count=1
```

覆盖：比例继承与链式继承、显式/默认软上限、借用扩容与可出借量回缩、自愿归还、
确定性顺序回收与强制释放、跨子树借贷、并发登记/释放无丢失、硬上限绝不突破、
多资源全有或全无、资源/组隔离、重配置收敛与失败回退、版本冲突、内存/文件存储重启恢复。
