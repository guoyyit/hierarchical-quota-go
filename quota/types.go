// Package quota 提供分层资源配额管理能力：
// 配额组可嵌套成树，支持父子组按比例共享额度、组间借用与归还、
// 硬/软上限控制、原子重配置与重启恢复。
package quota

// ResourceName 资源名称，例如 "cpu"、"memory"。
type ResourceName string

// Limit 单资源的软/硬上限。
// Soft 触发后仍可继续消耗（仅作为告警水位）；
// Hard 在任何并发路径下都不得被突破。
type Limit struct {
	Soft int64 `json:"soft"`
	Hard int64 `json:"hard"`
}

// GroupConfig 单个配额组的静态配置。
type GroupConfig struct {
	Name     string `json:"name"`
	Parent   string `json:"parent"` // 空串表示根组
	Priority int    `json:"priority"`
	// ShareRatio 子组从父组有效额度中分得的比例，取值 (0,1]。
	ShareRatio float64                `json:"share_ratio"`
	Quotas     map[ResourceName]Limit `json:"quotas"`
}

// Config 一份完整的配额树配置。
// 任一时刻生效的限额必须是某个已提交 Config 的完整快照，
// 不允许出现半更新状态或字段级混合。
type Config struct {
	Version uint64                  `json:"version"`
	Groups  map[string]*GroupConfig `json:"groups"`
}
