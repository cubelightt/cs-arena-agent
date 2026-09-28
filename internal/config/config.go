// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package config 负责 v2 的 config.yaml:最小键集、校验、默认值、SIGHUP 热重载。
//
// 与 v1 的差别(决策 9/10):**不读旧 config.json、不迁移**,旧的实例清单/路径模板/传输类键
// 一律抛弃 —— 路径按 msm_dir 约定推导(internal/msm/paths.go),实例清单由后端 hello_ack 下发。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// DefaultMinFreeBytes 磁盘余量阈值默认值 15 GiB(更新/建实例前置检查共用)。
const DefaultMinFreeBytes int64 = 15 << 30

// Config 是 v2 运行期的全部本地配置(部署时只需这一个文件)。
type Config struct {
	// 引导参数(必填)
	Token     string `yaml:"token"`
	BackendWS string `yaml:"backend_ws"`
	MSMDir    string `yaml:"msm_dir"`

	// 可选(不写即用默认)
	MSM           string  `yaml:"msm"`
	DemoDir       string  `yaml:"demo_dir"`
	ArchiveDir    string  `yaml:"archive_dir"`
	ConsoleTailMS int     `yaml:"console_tail_ms"`
	HealthPushMS  int     `yaml:"health_push_ms"`
	StateCacheS   float64 `yaml:"state_cache_s"`
	IdleTimeoutS  int     `yaml:"idle_timeout_s"`
	TimeoutS      int     `yaml:"timeout"`
	MinFreeBytes  int64   `yaml:"min_free_bytes"`
}

// Defaults 按 COMPAT §3 的 v1 默认值给出(语义不变,只是键集收敛)。
func Defaults() *Config {
	return &Config{
		ConsoleTailMS: 500,
		HealthPushMS:  5000,
		StateCacheS:   2.0,
		IdleTimeoutS:  120,
		TimeoutS:      120,
		MinFreeBytes:  DefaultMinFreeBytes,
	}
}

// 已实现(或本版认得的)键:其余键启动时汇总警告一行后忽略。
var knownKeys = map[string]bool{
	"token": true, "backend_ws": true, "msm_dir": true, "msm": true,
	"demo_dir": true, "archive_dir": true, "console_tail_ms": true,
	"health_push_ms": true, "state_cache_s": true, "idle_timeout_s": true,
	"timeout": true, "min_free_bytes": true,
}

// 旧版键:一律抛弃(不读、不迁移),但要指名道姓地提示,免得手改配置时以为仍然生效。
var deprecatedKeys = map[string]string{
	"mode":          "http 模式已移除,桥只跑 reverse;该键不再读取",
	"server_id":     "server_id 由后端 hello_ack 下发",
	"instances":     "实例清单由后端 hello_ack 下发",
	"log_dirs":      "路径模板按 msm_dir 推导",
	"game_dirs":     "路径模板按 msm_dir 推导",
	"workshop_dirs": "路径模板按 msm_dir 推导",
	"search_dirs":   "路径模板按 msm_dir 推导",
	"probe_timeout": "已废弃(http 时代的探针参数)",
	"port":          "http 模式已移除",
	"bind":          "http 模式已移除",
	"send_prefixes": "前缀白名单唯一执行点在后端 config.js",
}

// Load 读取并校验配置。返回 (配置, 警告行, 错误)。
// 只有"必填键缺失/文件不可读/类型错误"才算错误 —— 未知键与弃用键只警告。
func Load(path string) (*Config, []string, error) {
	var warns []string

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("配置不可读: %s(%v);v2 的最小键集见 README.md", path, err)
	}

	// 先按 map 解析一遍,才能把"未知键/弃用键"与真正的类型错误区分开
	var all map[string]any
	if err := yaml.Unmarshal(raw, &all); err != nil {
		return nil, nil, fmt.Errorf("配置解析失败: %s: %v", path, err)
	}
	if all == nil {
		all = map[string]any{}
	}

	var unknown, deprecated []string
	for k := range all {
		if knownKeys[k] {
			continue
		}
		if reason, ok := deprecatedKeys[k]; ok {
			deprecated = append(deprecated, fmt.Sprintf("%s(%s)", k, reason))
			continue
		}
		unknown = append(unknown, k)
	}
	sort.Strings(unknown)
	sort.Strings(deprecated)
	if len(deprecated) > 0 {
		warns = append(warns, "已弃用键(忽略): "+strings.Join(deprecated, "、"))
	}
	if len(unknown) > 0 {
		warns = append(warns, "未知键(忽略,请检查拼写): "+strings.Join(unknown, "、"))
	}
	// 旧版本文件残留:只提示,绝不读取
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "config.json")); err == nil {
		warns = append(warns, "检测到旧版 config.json(已废弃,不读取);v2 配置为 config.yaml")
	}

	cfg := Defaults()
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, warns, fmt.Errorf("配置解析失败: %s: %v", path, err)
	}

	// 默认值与规范化
	if cfg.MSM == "" && cfg.MSMDir != "" {
		cfg.MSM = filepath.Join(cfg.MSMDir, "cs2-server")
	}
	if cfg.DemoDir == "" {
		cfg.DemoDir = "~/arena-data/demos"
	}
	if cfg.ArchiveDir == "" {
		cfg.ArchiveDir = "~/arena-data"
	}
	cfg.DemoDir = expandHome(cfg.DemoDir)
	cfg.ArchiveDir = expandHome(cfg.ArchiveDir)
	if cfg.ConsoleTailMS < 200 {
		// v1 同款夹取:tail 周期同时决定 recv 超时,过小会把 CPU 打满
		cfg.ConsoleTailMS = 200
	}
	if cfg.HealthPushMS <= 0 {
		cfg.HealthPushMS = 5000
	}
	if cfg.StateCacheS <= 0 {
		cfg.StateCacheS = 2.0
	}
	if cfg.TimeoutS <= 0 {
		cfg.TimeoutS = 120
	}
	if cfg.MinFreeBytes <= 0 {
		cfg.MinFreeBytes = DefaultMinFreeBytes
	}

	var missing []string
	if strings.TrimSpace(cfg.Token) == "" {
		missing = append(missing, "token")
	}
	if strings.TrimSpace(cfg.BackendWS) == "" {
		missing = append(missing, "backend_ws")
	}
	if strings.TrimSpace(cfg.MSMDir) == "" {
		missing = append(missing, "msm_dir")
	}
	if len(missing) > 0 {
		return nil, warns, fmt.Errorf("缺少必填键: %s(见 README.md 的最小键集样例)", strings.Join(missing, "、"))
	}

	return cfg, warns, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Store 持有当前配置并支持原子替换(SIGHUP 热重载)。
// 各组件每次使用时 Get() 取最新值 —— 改 token/地址/阈值无需重启进程。
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  *Config
}

func NewStore(path string, cfg *Config) *Store {
	return &Store{path: path, cfg: cfg}
}

func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Store) Path() string { return s.path }

// Reload 重新读取配置文件并替换当前值;失败时保留旧值(重载失败不该弄停桥)。
func (s *Store) Reload() (*Config, []string, error) {
	cfg, warns, err := Load(s.path)
	if err != nil {
		return nil, warns, err
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	return cfg, warns, nil
}
