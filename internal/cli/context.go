// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"arena/agent/internal/agent"
	"arena/agent/internal/config"
	"arena/agent/internal/job"
	"arena/agent/internal/msm"
	"arena/agent/internal/registry"
)

// Ctx 是各子命令共用的运行上下文(配置 + 路径 + msm + 注册表快照)。
type Ctx struct {
	ConfigPath string
	Cfg        *config.Config
	Store      *config.Store
	Paths      *msm.Paths
	Runner     *msm.Runner
	Instances  []registry.Instance
	Status     *agent.RuntimeStatus
	// jobsMgr 是任务框架(懒加载:只有用到任务的命令才建目录/读索引)
	jobsMgr *job.Manager

	Stdout io.Writer
	Stderr io.Writer
	JSON   bool
}

// extractGlobalFlags 允许 `--config`/`--json` 出现在任意位置(v1 的 msm CLI 也是这个手感)。
func extractGlobalFlags(args []string) (rest []string, configPath string, asJSON bool) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 < len(args) {
				configPath = args[i+1]
				i++
			}
		case "--json":
			asJSON = true
		default:
			out = append(out, args[i])
		}
	}
	if configPath == "" {
		configPath = os.Getenv("CS_CONFIG")
	}
	if configPath == "" {
		configPath = "config.yaml"
	}
	return out, configPath, asJSON
}

// loadCtx 读配置并准备注册表;失败时返回 (nil, 退出码)。
func loadCtx(configPath string, stdout, stderr io.Writer, asJSON bool) (*Ctx, int) {
	cfg, warns, err := config.Load(configPath)
	for _, w := range warns {
		fmt.Fprintf(stderr, "[cs] WARNING: %s\n", w)
	}
	if err != nil {
		fmt.Fprintf(stderr, "[cs] %v\n", err)
		return nil, ExitFailure
	}
	paths := msm.NewPaths(cfg.MSMDir)
	runner := msm.NewRunner(func() msm.RunnerConfig {
		return msm.RunnerConfig{MSM: cfg.MSM, MSMDir: cfg.MSMDir, TimeoutS: cfg.TimeoutS, StateCacheS: cfg.StateCacheS}
	})

	c := &Ctx{
		ConfigPath: configPath,
		Cfg:        cfg,
		Store:      config.NewStore(configPath, cfg),
		Paths:      paths,
		Runner:     runner,
		Status:     agent.ReadRuntimeStatus(configPath),
		Stdout:     stdout,
		Stderr:     stderr,
		JSON:       asJSON,
	}
	if scanned, err := registry.Scan(cfg.MSMDir); err == nil {
		rf := registry.Load(registry.PathFor(configPath))
		c.Instances = registry.MergeIdx(scanned, rf.Items, rf.MaxIdx)
	} else {
		fmt.Fprintf(stderr, "[cs] 提示: 未读到实例清单(%v);注册表类命令不可用\n", err)
	}
	return c, ExitOK
}

// saveRegistry 把注册表落盘(<config_dir>/registry.json,原子写;cs new/cs del 调用)。
func (c *Ctx) saveRegistry(f registry.File) error {
	return registry.Save(registry.PathFor(c.ConfigPath), f)
}

// loadRegistry 读本机注册表文件(不存在 → 空表)。
func (c *Ctx) loadRegistry() registry.File {
	return registry.Load(registry.PathFor(c.ConfigPath))
}

// regNamesProvider 提供"本机注册表实例名"(扫描 + registry.json 编号合并),带 1s TTL 缓存:
// 守护的 Names()(每次入站 op 与 health 推送都调用)不能每次都去扫目录/读文件。
type regNamesProvider struct {
	mu      sync.Mutex
	cfgFn   func() *config.Config
	cfgPath string
	expires time.Time
	names   []string
}

func (p *regNamesProvider) get() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Now().Before(p.expires) {
		return p.names
	}
	var names []string
	if scanned, err := registry.Scan(p.cfgFn().MSMDir); err == nil {
		rf := registry.Load(registry.PathFor(p.cfgPath))
		names = registry.Names(registry.MergeIdx(scanned, rf.Items, rf.MaxIdx))
	}
	p.names, p.expires = names, time.Now().Add(time.Second)
	return names
}

// unionNames 合并两份实例名(去空去重,保序)。
func unionNames(base, extra []string) []string {
	seen := make(map[string]bool, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, list := range [][]string{base, extra} {
		for _, n := range list {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// resolve 解析"编号或名称";arg 为 all/空 时由调用方处理。
func (c *Ctx) resolve(arg string) (*registry.Instance, error) {
	return registry.Resolve(c.Instances, arg)
}

// targets 解析 <n|all>:all 展开为全部实例。
func (c *Ctx) targets(arg string) ([]registry.Instance, error) {
	if isAll(arg) {
		if len(c.Instances) == 0 {
			return nil, fmt.Errorf("注册表为空(检查 msm_dir 与 cfg/inst-* 布局)")
		}
		return c.Instances, nil
	}
	one, err := c.resolve(arg)
	if err != nil {
		return nil, err
	}
	return []registry.Instance{*one}, nil
}

func isAll(arg string) bool {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "all", "*", "":
		return true
	default:
		return false
	}
}

// maintained 返回该组维护中的信息(nil = 未维护/未知)。
func (c *Ctx) maintained() *agent.RuntimeStatus {
	if c.Status != nil && c.Status.MaintenanceGroup != "" {
		return c.Status
	}
	return nil
}

// assertWritable 是写操作门禁:维护中的组拒绝启停。
// 判定顺序:**本地进行中的任务优先**(离线也生效)→ 后端下发的维护态(缓存)。
func (c *Ctx) assertWritable(what string) error {
	for _, m := range c.jobs().ActiveGroups() {
		return fmt.Errorf("服务器组 %s 正在维护中(%s);已拒绝 %s", m.GroupID, m.Reason, what)
	}
	if st := c.maintained(); st != nil {
		return fmt.Errorf("服务器组 %s 正在维护中(%s);已拒绝 %s", st.MaintenanceGroup, st.MaintenanceGroup, what)
	}
	return nil
}

// logPathOf 取实例控制台日志(找不到时返回 tried 供报错展示)。
func (c *Ctx) logPathOf(instance string) (string, []string) {
	return c.Paths.FindLogPath(instance)
}
