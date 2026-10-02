package quota

import (
	"errors"
	"fmt"
)

// Validate 校验整份配置的一致性（父子关系、比例、上限等）。
// 任何一项不合法都返回错误，调用方据此整体拒绝该配置。
func Validate(c *Config) error {
	if c == nil {
		return errors.New("quota: nil config")
	}
	if len(c.Groups) == 0 {
		return errors.New("quota: config has no groups")
	}

	roots := 0
	for key, g := range c.Groups {
		if g == nil {
			return fmt.Errorf("quota: group %q is nil", key)
		}
		if g.Name == "" {
			return errors.New("quota: group with empty name")
		}
		if key != g.Name {
			return fmt.Errorf("quota: group key %q != name %q", key, g.Name)
		}
		if g.Parent == "" {
			roots++
		} else {
			if _, ok := c.Groups[g.Parent]; !ok {
				return fmt.Errorf("quota: group %q has unknown parent %q", g.Name, g.Parent)
			}
			if g.ShareRatio <= 0 || g.ShareRatio > 1 {
				return fmt.Errorf("quota: group %q share ratio %v out of (0,1]", g.Name, g.ShareRatio)
			}
		}
		for res, l := range g.Quotas {
			if res == "" {
				return fmt.Errorf("quota: group %q has empty resource name", g.Name)
			}
			if l.Hard < 0 || l.Soft < 0 {
				return fmt.Errorf("quota: group %q resource %q has negative limit", g.Name, res)
			}
			if l.Soft > l.Hard {
				return fmt.Errorf("quota: group %q resource %q soft %d > hard %d", g.Name, res, l.Soft, l.Hard)
			}
		}
	}
	if roots != 1 {
		return fmt.Errorf("quota: config must have exactly one root, got %d", roots)
	}

	// 环检测：沿父链走，步数超过组数即成环。
	for name := range c.Groups {
		seen := map[string]bool{}
		cur := name
		for cur != "" {
			if seen[cur] {
				return fmt.Errorf("quota: cycle detected at group %q", cur)
			}
			seen[cur] = true
			cur = c.Groups[cur].Parent
		}
	}
	return nil
}

// cloneConfig 返回配置的独立深拷贝：拷贝与原配置不共享任何 map/指针，
// 任一方后续被修改都不会影响另一方。
func cloneConfig(c *Config) *Config {
	if c == nil {
		return nil
	}
	cp := &Config{Version: c.Version}
	if c.Groups != nil {
		cp.Groups = make(map[string]*GroupConfig, len(c.Groups))
		for name, g := range c.Groups {
			cp.Groups[name] = cloneGroupConfig(g)
		}
	}
	return cp
}

// cloneGroupConfig 返回单组配置的独立深拷贝。
func cloneGroupConfig(g *GroupConfig) *GroupConfig {
	if g == nil {
		return nil
	}
	cp := *g
	if g.Quotas != nil {
		cp.Quotas = make(map[ResourceName]Limit, len(g.Quotas))
		for res, l := range g.Quotas {
			cp.Quotas[res] = l
		}
	}
	return &cp
}
