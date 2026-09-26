package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Store 已提交配置的持久化接口。
// 只有提交成功的版本才允许落盘；未提交的中间改动不得写入。
type Store interface {
	// Save 原子地保存一个已提交版本。
	Save(c *Config) error
	// Load 返回最近一次已提交版本；不存在时返回 nil, nil。
	Load() (*Config, error)
}

// FileStore 基于本地文件的原子持久化（写临时文件后 rename）。
type FileStore struct {
	Path string
}

// Save 实现 Store。先写临时文件并 fsync，再原子 rename，
// 保证任何时刻文件要么是旧版本要么是新版本，不会出现半写入状态。
func (fs *FileStore) Save(c *Config) error {
	if c == nil {
		return errors.New("quota: cannot save nil config")
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("quota: marshal config: %w", err)
	}
	dir := filepath.Dir(fs.Path)
	tmp, err := os.CreateTemp(dir, ".quota-*.tmp")
	if err != nil {
		return fmt.Errorf("quota: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后再删是 no-op
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("quota: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("quota: fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("quota: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, fs.Path); err != nil {
		return fmt.Errorf("quota: rename temp file: %w", err)
	}
	return nil
}

// Load 实现 Store。文件不存在时返回 nil, nil（表示尚无已提交版本）。
func (fs *FileStore) Load() (*Config, error) {
	data, err := os.ReadFile(fs.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("quota: read config file: %w", err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("quota: unmarshal config: %w", err)
	}
	return &c, nil
}
