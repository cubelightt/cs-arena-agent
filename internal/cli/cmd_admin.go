// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"arena/agent/internal/config"
	"arena/agent/internal/host"
	"arena/agent/internal/registry"
)

// writableConfigKeys 是 `cs config set` 的白名单(与 config.Load 认得的键一致)。
var writableConfigKeys = []string{
	"token", "backend_ws", "msm_dir", "msm", "demo_dir", "archive_dir",
	"console_tail_ms", "health_push_ms", "state_cache_s", "idle_timeout_s", "timeout", "min_free_bytes",
}

// cmdConfig 实现 `cs config get <key> | set <key> <value>`。
// set 会**按行改写**(保留注释),写前备份 config.yaml.bak-<时间>,成功后向守护发 SIGHUP 热重载。
func cmdConfig(c *Ctx, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(c.Stderr, "[cs] 用法: cs config get <键> | cs config set <键> <值>\n可用键: %s\n", strings.Join(writableConfigKeys, ", "))
		return ExitUsage
	}
	switch args[0] {
	case "get":
		if len(args) < 2 {
			fmt.Fprintf(c.Stderr, "[cs] 用法: cs config get <键>\n")
			return ExitUsage
		}
		raw, err := os.ReadFile(c.ConfigPath)
		if err != nil {
			fmt.Fprintf(c.Stderr, "[cs] 读配置失败: %v\n", err)
			return ExitFailure
		}
		val, ok := yamlScalarValue(string(raw), args[1])
		if !ok {
			fmt.Fprintf(c.Stderr, "[cs] 配置里没有键 %s(注意 v2 只认最小键集)\n", args[1])
			return ExitFailure
		}
		fmt.Fprintln(c.Stdout, val)
		return ExitOK
	case "set":
		if len(args) < 3 {
			fmt.Fprintf(c.Stderr, "[cs] 用法: cs config set <键> <值>\n")
			return ExitUsage
		}
		key, value := args[1], args[2]
		if !knownWritableKey(key) {
			fmt.Fprintf(c.Stderr, "[cs] 未知或不支持改写的键: %s\n可用键: %s\n", key, strings.Join(writableConfigKeys, ", "))
			return ExitUsage
		}
		backup, err := setConfigValue(c.ConfigPath, key, value)
		if err != nil {
			fmt.Fprintf(c.Stderr, "[cs] 写入失败(已回滚): %v\n", err)
			return ExitFailure
		}
		fmt.Fprintf(c.Stdout, "%s = %s(备份 %s)\n", key, value, backup)
		if pid := daemonPID(c); pid > 0 {
			if err := syscall.Kill(pid, syscall.SIGHUP); err == nil {
				fmt.Fprintf(c.Stdout, "已通知守护进程(PID %d)热重载配置\n", pid)
			} else {
				fmt.Fprintf(c.Stderr, "[cs] 通知守护进程失败: %v(可在下次重启生效)\n", err)
			}
		}
		return ExitOK
	default:
		fmt.Fprintf(c.Stderr, "[cs] 未知子命令: config %s\n", args[0])
		return ExitUsage
	}
}

func knownWritableKey(key string) bool {
	for _, k := range writableConfigKeys {
		if k == key {
			return true
		}
	}
	return false
}

// yamlScalarValue 取 `key: value` 的值部分(去注释与引号)。
func yamlScalarValue(text, key string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(trimmed, key+":") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(trimmed, key+":"))
		if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		return strings.Trim(val, `"'`), true
	}
	return "", false
}

// setConfigValue 按行改写键值(保留注释与顺序),写前备份;写完用 config.Load 校验,失败回滚。
func setConfigValue(path, key, value string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(raw), "\n")
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, key+":") {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		lines[i] = indent + key + ": " + yamlQuoteIfNeeded(value)
		replaced = true
		break
	}
	if !replaced {
		lines = append(lines, key+": "+yamlQuoteIfNeeded(value))
	}
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n" // 保留文件结尾换行(手改/工具改都不该把它吃掉)
	}

	stamp := time.Now().Format("20060102150405")
	backup := path + ".bak-" + stamp
	for i := 2; ; i++ { // 同一秒内多次改动不互相覆盖
		if _, err := os.Stat(backup); err != nil {
			break
		}
		backup = fmt.Sprintf("%s.bak-%s-%d", path, stamp, i)
	}
	if err := os.WriteFile(backup, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return "", err
	}
	if _, _, err := config.Load(path); err != nil {
		_ = os.WriteFile(path, raw, 0o600) // 回滚(校验失败不留坏配置)
		return "", fmt.Errorf("%v", err)
	}
	return backup, nil
}

// yamlQuoteIfNeeded 给需要引号的值加双引号(值里有 YAML 特殊字符时)。
func yamlQuoteIfNeeded(v string) string {
	if v == "" {
		return `""`
	}
	if strings.ContainsAny(v, ":#{}[],&*?|-<>=!%@`\"'\n") || strings.HasPrefix(v, " ") || strings.HasSuffix(v, " ") {
		return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
	}
	return v
}

// daemonPID 从心跳文件取守护 PID(0 = 没有)。
func daemonPID(c *Ctx) int {
	if c.Status == nil || !c.Status.Fresh(30*time.Second) {
		return 0
	}
	return c.Status.PID
}

// cmdUpdate 只做 `--check`(只读);真正的更新是 job 框架。
func cmdUpdate(c *Ctx, args []string) int {
	checkOnly := false
	for _, a := range args {
		if a == "--check" {
			checkOnly = true
		}
	}
	if !checkOnly {
		// 执行路径(job 框架):打印计划 → 二次确认 → 前台跟踪任务
		return cmdUpdateJob(c, args)
	}
	info := c.hostInfo(true)
	switch {
	case info.BuildID == "":
		fmt.Fprintf(c.Stderr, "[cs] 读不到当前 buildid: %s\n", info.ManifestPath)
		return ExitFailure
	case info.LatestBuild == "":
		fmt.Fprintf(c.Stderr, "[cs] 查询官方最新 buildid 失败: %s\n", info.LatestError)
		return ExitFailure
	case info.BuildID == info.LatestBuild:
		fmt.Fprintf(c.Stdout, "已是最新(build %s)\n", info.BuildID)
		return ExitOK
	default:
		fmt.Fprintf(c.Stdout, "有更新: %s → %s(执行:cs update)\n", info.BuildID, info.LatestBuild)
		return ExitFailure
	}
}

// cmdDoctor 做部署前自检:依赖/配置/路径/磁盘/端口/后端连接/守护/注册表一致性。
func cmdDoctor(c *Ctx, args []string) int {
	type check struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
		Note string `json:"note,omitempty"`
	}
	var checks []check
	add := func(name string, ok bool, note string) {
		checks = append(checks, check{Name: name, OK: ok, Note: note})
	}

	// 1) 外部命令(msm/steamcmd 依赖)
	if missing := host.CheckDeps(); len(missing) == 0 {
		add("依赖命令齐全(tmux/wget/tar/jq/inotifywait)", true, "")
	} else {
		add("依赖命令齐全(tmux/wget/tar/jq/inotifywait)", false, "缺失: "+strings.Join(missing, ", "))
	}
	// 2) msm 可执行
	if _, err := os.Stat(c.Cfg.MSM); err == nil {
		add("msm 可执行在位", true, c.Cfg.MSM)
	} else {
		add("msm 可执行在位", false, c.Cfg.MSM+": "+err.Error())
	}
	// 3) base 目录与 buildid
	manifest := host.ManifestPath(c.Cfg.MSMDir)
	buildID, err := host.ReadBuildID(manifest)
	add("游戏文件在位(appmanifest buildid)", err == nil, fmt.Sprintf("build %s @ %s", buildID, manifest))
	// 4) 磁盘余量
	info := c.hostInfo(false)
	if info.DiskTotal == 0 {
		add("磁盘余量 ≥ 阈值", false, "读不到磁盘用量")
	} else {
		ok := info.DiskFree >= c.Cfg.MinFreeBytes
		add("磁盘余量 ≥ 阈值", ok, fmt.Sprintf("剩 %s(阈值 %s)", host.HumanBytes(info.DiskFree), host.HumanBytes(c.Cfg.MinFreeBytes)))
	}
	// 5) 实例注册表与端口冲突
	if len(c.Instances) == 0 {
		add("实例注册表(cfg/inst-*/server.conf)", false, "未发现实例;检查 msm_dir 布局")
	} else {
		nameOK := true
		for _, it := range c.Instances {
			if it.Port == 0 {
				nameOK = false
			}
		}
		add("实例注册表(cfg/inst-*/server.conf)", nameOK, fmt.Sprintf("%d 个实例: %s", len(c.Instances), strings.Join(registry.Names(c.Instances), ", ")))
	}
	if conflicts := registry.PortConflict(c.Instances); len(conflicts) == 0 {
		add("端口/GOTV 无冲突", true, "")
	} else {
		add("端口/GOTV 无冲突", false, strings.Join(conflicts, "; "))
	}
	// 6) 后端连接(心跳)
	switch {
	case c.Status == nil:
		add("守护进程与后端连接", false, "无心跳文件(agent.status.json):守护可能未运行")
	case !c.Status.Fresh(30 * time.Second):
		add("守护进程与后端连接", false, "心跳陈旧,守护可能已退出")
	case !c.Status.Connected:
		add("守护进程与后端连接", false, "守护在跑但后端未连接("+c.Status.BackendWS+")")
	default:
		add("守护进程与后端连接", true, fmt.Sprintf("%s,清单来源 %s", c.Status.BackendWS, c.Status.InstancesSource))
	}
	// 7) 实例清单来源
	add("实例清单来源可读", instancesSource(c) != "none", "instancesSource="+instancesSource(c))
	// 8) 残留 top5(只读展示,供 M6 清理参考)
	residual := residualSummary(c)

	if c.JSON {
		raw, _ := json.MarshalIndent(map[string]any{"checks": checks, "residual": residual, "host": info}, "", "  ")
		fmt.Fprintln(c.Stdout, string(raw))
	} else {
		failed := 0
		for _, ck := range checks {
			mark := "✓"
			if !ck.OK {
				mark = "✗"
				failed++
			}
			note := ck.Note
			if note != "" {
				note = "  " + note
			}
			fmt.Fprintf(c.Stdout, "  %s %s%s\n", mark, ck.Name, note)
		}
		if len(residual) > 0 {
			fmt.Fprintln(c.Stdout, "  残留占用 top:")
			for _, e := range residual {
				fmt.Fprintf(c.Stdout, "    %s  %s\n", host.HumanBytes(e.Bytes), e.Path)
			}
		}
		if failed > 0 {
			fmt.Fprintf(c.Stdout, "\n%d 项未通过\n", failed)
			return ExitFailure
		}
		fmt.Fprintln(c.Stdout, "\n全部通过")
	}
	return ExitOK
}

// residualSummary 只读盘点常见残留(备份/录像/msm 日志/已删实例目录)。
func residualSummary(c *Ctx) []host.Entry {
	home, _ := os.UserHomeDir()
	var out []host.Entry
	for _, p := range []string{
		filepath.Join(home, "backup"),
		c.Cfg.DemoDir,
		filepath.Join(c.Cfg.MSMDir, "..", "msm.d", "cs2", "log"),
		filepath.Join(c.Cfg.MSMDir, "..", "msm.d", "cs2", "base", "game", "csgo", "MatchZy"),
	} {
		if size, count := host.DirSize(p); size > 0 {
			out = append(out, host.Entry{Path: p, Bytes: size, Files: count})
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Bytes > out[j-1].Bytes; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}
