// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package job

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"arena/agent/internal/config"
	"arena/agent/internal/host"
	"arena/agent/internal/msm"
)

// buildPlan 按 kind 构造执行计划;参数非法在此报错(不占组锁、不落任务)。
func buildPlan(kind string, spec Spec, instances []string, m *Manager) (Plan, error) {
	switch kind {
	case KindGameUpdate:
		return gameUpdatePlan(spec, instances, m)
	case KindPluginSync:
		return pluginSyncPlan(spec, m)
	case KindPluginDeploy:
		return pluginDeployPlan(spec, m)
	case KindDemoCollect:
		return demoCollectPlan(spec, m)
	case KindHostCleanup:
		return hostCleanupPlan(spec, instances, m)
	case KindInstanceCreate:
		return instanceCreatePlan(spec, instances, m)
	case KindInstanceDelete:
		return instanceDeletePlan(spec, instances, m)
	default:
		return Plan{}, fmt.Errorf("未知任务类型: %s", kind)
	}
}

// ---- game_update-------------------

func gameUpdatePlan(spec Spec, instances []string, m *Manager) (Plan, error) {
	confirm := spec.Confirm
	if v, ok := spec.Params["confirm"].(string); ok && v != "" {
		confirm = v
	}
	if confirm != "UPDATE" {
		return Plan{}, errors.New(`游戏更新需要二次确认(confirm 必须精确等于 "UPDATE")`)
	}
	timeoutS := intParam(spec.Params, "timeoutS", 3600)
	skipBackup := boolParam(spec.Params, "skipBackup", false)
	// msm update 是交互式的(游戏本体 Y;SwiftlyS2 固定 n,见 README.md 的主机配置)。
	// 交互序列做成参数,便于按 msm 版本微调。
	stdin := strParam(spec.Params, "msmUpdateStdin")
	if stdin == "" {
		stdin = "Y\nn\n"
	}

	// 更新前在跑的实例(恢复步骤用;步骤 1 采集)
	runningBefore := map[string]bool{}
	stopped := false
	backupDir := ""

	steps := []Step{
		{
			Name: "前置检查(磁盘余量/msm 可执行)",
			Run: func(c *Ctx) error {
				_, free, _, err := host.DiskUsage(c.Cfg.MSMDir)
				if err != nil {
					return fmt.Errorf("无法读取磁盘信息: %w", err)
				}
				if c.Cfg.MinFreeBytes > 0 && free < c.Cfg.MinFreeBytes {
					return fmt.Errorf("磁盘余量不足:剩 %s,低于阈值 %s",
						host.HumanBytes(free), host.HumanBytes(c.Cfg.MinFreeBytes))
				}
				c.Logf("磁盘余量 %s(阈值 %s)", host.HumanBytes(free), host.HumanBytes(c.Cfg.MinFreeBytes))
				for _, name := range c.Instances {
					runningBefore[name] = c.Runner.Status(name, false) == "RUNNING"
				}
				c.SetResult("runningBefore", runningNames(runningBefore))
				c.Logf("更新前在跑实例:%s", strings.Join(runningNames(runningBefore), ","))
				return nil
			},
		},
		{
			Name: "停止全部实例",
			Run: func(c *Ctx) error {
				for _, name := range c.Instances {
					if _, err := c.MSM(name, "stop", nil, 120); err != nil {
						return fmt.Errorf("停止 %s 失败: %w", name, err)
					}
				}
				stopped = true
				return nil
			},
		},
		{
			Name: "备份关键产物(cfg + 实例 MatchZy 配置)",
			Run: func(c *Ctx) error {
				if skipBackup {
					c.Logf("按参数跳过备份(skipBackup=true)")
					return nil
				}
				dir, err := backupPreUpdate(c)
				if err != nil {
					return err
				}
				backupDir = dir
				c.SetResult("backupDir", dir)
				return nil
			},
		},
		{
			// 取消点:步骤边界可取消;此步进行中**只有 force 才能打断**(不打断 steamcmd,PORTCOL-V2 §1.4)
			Name: "msm update(steamcmd)",
			Run: func(c *Ctx) error {
				res, err := c.Runner.RunStream(c.ctx(), "", "update", nil, msm.StreamOpts{
					TimeoutS: timeoutS,
					Stdin:    stdin,
					OnLine:   func(line string) { c.LogLine("update | " + line) },
				})
				if err != nil {
					return err
				}
				if res.Returncode != 0 {
					return fmt.Errorf("msm update 退出码 %d", res.Returncode)
				}
				return nil
			},
		},
		{
			Name: "启动全部实例",
			Run: func(c *Ctx) error {
				for _, name := range c.Instances {
					if _, err := c.MSM(name, "start", nil, 120); err != nil {
						return fmt.Errorf("启动 %s 失败: %w", name, err)
					}
				}
				return nil
			},
		},
		{
			Name: "回读状态与版本",
			Run: func(c *Ctx) error {
				for _, name := range c.Instances {
					c.Logf("%s: %s", name, c.Runner.Status(name, false))
				}
				if b, err := host.ReadBuildID(host.ManifestPath(c.Cfg.MSMDir)); err == nil {
					c.SetResult("build", b)
					c.Logf("当前 build %s", b)
				}
				if backupDir != "" {
					c.Logf("更新前备份:%s", backupDir)
				}
				return nil
			},
		},
		{
			Name: "提示:CS2 更新可能还原主机侧补丁(需复测)",
			Run: func(c *Ctx) error {
				c.Logf("提醒:CS2 更新可能还原 MatchZy dll / bot cfg / ArenaDuel / gameinfo.gi,请按复测清单核对")
				return nil
			},
		},
	}

	recoverFn := func(c *Ctx) {
		if !stopped {
			return
		}
		for _, name := range c.Instances {
			if !runningBefore[name] {
				continue
			}
			if _, err := c.MSM(name, "start", nil, 120); err != nil {
				c.Logf("恢复 %s 失败:%v", name, err)
				continue
			}
			c.Logf("已恢复 %s(更新前在跑)", name)
		}
	}
	return Plan{Steps: steps, Recover: recoverFn}, nil
}

func runningNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k, v := range set {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// backupPreUpdate 备份 cfg/ 与各实例的 MatchZy 配置(纯拷贝;失败即任务失败,不硬着头皮更新)。
func backupPreUpdate(c *Ctx) (string, error) {
	stamp := time.Now().Format("20060102-150405")
	dest := filepath.Join(backupRoot(c.Cfg), "pre-update-"+stamp)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", fmt.Errorf("创建备份目录失败: %w", err)
	}
	root := MSMRoot(c.Cfg.MSMDir)
	cfgDir := filepath.Join(root, "cfg")
	if fi, err := os.Stat(cfgDir); err == nil && fi.IsDir() {
		if _, err := copyTree(cfgDir, filepath.Join(dest, "cfg")); err != nil {
			return "", fmt.Errorf("备份 cfg 失败: %w", err)
		}
		c.Logf("已备份 %s → %s", cfgDir, filepath.Join(dest, "cfg"))
	}
	for _, name := range c.Instances {
		gameDir, _ := c.Paths.FindGameDir(name)
		if gameDir == "" {
			continue
		}
		src := filepath.Join(gameDir, "cfg", "MatchZy")
		if fi, err := os.Stat(src); err == nil && fi.IsDir() {
			if _, err := copyTree(src, filepath.Join(dest, name, "MatchZy")); err != nil {
				return "", fmt.Errorf("备份 %s 的 MatchZy 配置失败: %w", name, err)
			}
			c.Logf("已备份 %s/MatchZy → %s", name, filepath.Join(dest, name, "MatchZy"))
		}
	}
	return dest, nil
}

// backupRoot 是备份根(与 v1 的 ~/backup 一致;`archive_dir` 存在时放其下 backup/)。
// homeDir 取当前用户 HOME(取不到回 /tmp,绝不返回空串拼路径)。
func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return "/tmp"
}

func backupRoot(cfg *config.Config) string {
	if cfg.ArchiveDir != "" {
		return filepath.Join(cfg.ArchiveDir, "backup")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/tmp/backup"
	}
	return filepath.Join(home, "backup")
}

// MSMRoot = <msm_dir>/../msm.d/cs2(与 registry.CFGDir 同源)。
func MSMRoot(msmDir string) string {
	return filepath.Clean(filepath.Join(msmDir, "..", "msm.d", "cs2"))
}

// ---- 参数小工具 -------------------------------------------------------------

func intParam(params map[string]any, key string, def int) int {
	switch v := params[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func boolParam(params map[string]any, key string, def bool) bool {
	if v, ok := params[key].(bool); ok {
		return v
	}
	return def
}

func strParam(params map[string]any, key string) string {
	if v, ok := params[key].(string); ok {
		return v
	}
	return ""
}

// strSliceParam 取字符串数组:CLI 进程内传的是 []string,平台经 JSON 下发的是 []any,两者都要认。
func strSliceParam(params map[string]any, key string) []string {
	switch raw := params[key].(type) {
	case []string:
		return append([]string(nil), raw...)
	case []any:
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			out = append(out, msm.ScalarString(v))
		}
		return out
	}
	return nil
}
