// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// 任务框架的 CLI:cs update(执行)/ cs jobs / cs job log|cancel / cs plugins / cs demo / cs host cleanup。
//
// 与守护的关系:CLI 是**独立进程**,任务在 CLI 进程内执行(前台,可 Ctrl-C);
// 任务状态与日志落在 <config_dir>/jobs/,守护连上后端后会把未上报的任务收敛给平台
// ,因此"主机侧发起的更新"平台侧同样可见。
package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"arena/agent/internal/agent"
	"arena/agent/internal/config"
	"arena/agent/internal/job"
	"arena/agent/internal/protocol"
	"arena/agent/internal/registry"
)

// confirmPrompt 要求手工输入确认串。
func confirmPrompt(c *Ctx, want, what string) bool {
	fmt.Fprintf(c.Stdout, "请输入 %q 确认%s:", want, what)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		fmt.Fprintln(c.Stderr, "[cs] 未确认,已取消")
		return false
	}
	if strings.TrimSpace(sc.Text()) != want {
		fmt.Fprintln(c.Stderr, "[cs] 确认串不匹配,已取消")
		return false
	}
	return true
}

// currentUser 是发起任务的 SSH 用户名(审计用;拿不到就留空)。
func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// startJob 启动任务并**前台跟随**到终态,返回退出码。
func startJob(c *Ctx, spec job.Spec, title string) int {
	mgr := c.jobs()
	if spec.Origin == "" {
		spec.Origin = job.OriginCLI
	}
	if spec.CLIUser == "" {
		spec.CLIUser = currentUser()
	}
	j, err := mgr.Start(spec)
	if err != nil {
		fmt.Fprintf(c.Stderr, "[cs] 无法启动任务: %v\n", err)
		return ExitFailure
	}
	fmt.Fprintf(c.Stdout, "%s:任务 %d(%s)已启动,组 %s\n", title, j.ID, j.Kind, j.GroupID)
	fmt.Fprintf(c.Stdout, "[cs] 日志:%s(cs job log %d -f)\n", mgr.LogPath(j.ID), j.ID)

	// 前台跟随:每 300ms 读一次增量行,直到终态
	var offset int64
	for {
		lines, next, err := mgr.LogSince(j.ID, offset)
		if err == nil {
			for _, ln := range lines {
				fmt.Fprintln(c.Stdout, ln)
			}
			offset = next
		}
		cur := mgr.Get(j.ID)
		if cur == nil {
			return ExitFailure
		}
		switch cur.Status {
		case protocol.JobDone:
			fmt.Fprintf(c.Stdout, "%s 完成(job %d,结果 %s)\n", title, cur.ID, compactJSON(cur.Result))
			return ExitOK
		case protocol.JobFailed:
			fmt.Fprintf(c.Stderr, "%s 失败(job %d):%s\n", title, cur.ID, cur.Error)
			return ExitFailure
		case protocol.JobCancelled:
			fmt.Fprintf(c.Stderr, "%s 已取消(job %d)\n", title, cur.ID)
			return ExitFailure
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func compactJSON(v any) string {
	if v == nil {
		return "{}"
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// cmdUpdate:cs update [--check] [--yes]
func cmdUpdateJob(c *Ctx, args []string) int {
	skipConfirm := false
	for _, a := range args {
		if a == "--yes" {
			skipConfirm = true
		}
	}
	info := c.hostInfo(true)
	if info.BuildID != "" && info.LatestBuild != "" && info.BuildID == info.LatestBuild {
		fmt.Fprintf(c.Stdout, "已是最新(build %s),无需更新\n", info.BuildID)
		return ExitOK
	}
	fmt.Fprintf(c.Stdout, "游戏更新计划(组 %s,实例 %s):\n", c.groupID(), strings.Join(c.instanceNames(), ", "))
	for i, st := range []string{
		"前置检查(磁盘余量 ≥ " + fmt.Sprintf("%s", humanBytesShort(c.Cfg.MinFreeBytes)) + ")",
		"停止全部实例", "备份 cfg 与 MatchZy 配置", "msm update(steamcmd,此步不可被打断)",
		"启动全部实例", "回读状态与版本", "提示复测主机侧补丁",
	} {
		fmt.Fprintf(c.Stdout, "  %d) %s\n", i+1, st)
	}
	if info.BuildID != "" && info.LatestBuild != "" {
		fmt.Fprintf(c.Stdout, "版本:%s → %s\n", info.BuildID, info.LatestBuild)
	}
	if !skipConfirm && !confirmPrompt(c, "yes", "执行游戏更新(该组全部实例将停止数分钟)") {
		return ExitConfirm // 3 = 需要确认但未确认(plan §3 退出码)
	}
	return startJob(c, job.Spec{
		Kind:    job.KindGameUpdate,
		Params:  map[string]any{"confirm": "UPDATE"},
		Origin:  job.OriginCLI,
		Confirm: "UPDATE",
	}, "游戏更新")
}

func humanBytesShort(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f G", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f M", float64(n)/(1<<20))
	default:
		return strconv.FormatInt(n, 10)
	}
}

func (c *Ctx) groupID() string {
	st := agent.LoadState(agent.StatePathFor(c.ConfigPath)).Snapshot()
	if st.ServerID != "" {
		return st.ServerID
	}
	return "local"
}

func (c *Ctx) instanceNames() []string {
	st := agent.LoadState(agent.StatePathFor(c.ConfigPath))
	if ns := st.Names(); len(ns) > 0 {
		return ns
	}
	return registry.Names(c.Instances)
}

// cmdJobs:cs jobs [--json] —— 列出任务(新 → 旧)。
func cmdJobs(c *Ctx, args []string) int {
	mgr := c.jobs()
	items := mgr.List()
	if c.JSON {
		out := make([]map[string]any, 0, len(items))
		for _, j := range items {
			out = append(out, map[string]any{
				"jobId": j.ID, "kind": j.Kind, "groupId": j.GroupID, "status": j.Status,
				"step": j.Step, "stepIndex": j.StepIndex, "stepTotal": j.StepTotal,
				"progress": j.Progress, "startedAt": j.StartedAt, "finishedAt": j.FinishedAt,
				"error": j.Error, "origin": j.Origin, "result": j.Result,
			})
		}
		raw, _ := json.MarshalIndent(map[string]any{"jobs": out, "active": mgr.ActiveGroups()}, "", "  ")
		fmt.Fprintln(c.Stdout, string(raw))
		return ExitOK
	}
	if len(items) == 0 {
		fmt.Fprintln(c.Stdout, "没有任务记录")
		return ExitOK
	}
	for _, j := range items {
		line := fmt.Sprintf("#%-8d %-14s %-8s %3d%%  %s/%s", j.ID, j.Kind, j.Status, j.Progress, j.Step, j.GroupID)
		if j.Status == protocol.JobRunning || j.Status == protocol.JobCancelling {
			line = "▶ " + line
		}
		if j.Error != "" {
			line += "  " + j.Error
		}
		fmt.Fprintln(c.Stdout, line)
	}
	return ExitOK
}

// cmdJobLog:cs job log <id> [--lines N] [-f]
func cmdJobLog(c *Ctx, args []string) int {
	pos := positional(args)
	if len(pos) == 0 {
		fmt.Fprintln(c.Stderr, "[cs] 用法: cs job log <任务号> [--lines N] [-f]")
		return ExitUsage
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		fmt.Fprintf(c.Stderr, "[cs] 任务号非法: %s\n", pos[0])
		return ExitUsage
	}
	mgr := c.jobs()
	if mgr.Get(id) == nil {
		fmt.Fprintf(c.Stderr, "[cs] 没有任务 %d\n", id)
		return ExitFailure
	}
	follow := hasFlag(args, "-f") || hasFlag(args, "--follow")
	lines := intFlag(args, "--lines", 200)
	if !follow {
		tail, err := mgr.LogTail(id, lines)
		if err != nil {
			fmt.Fprintf(c.Stderr, "[cs] 读日志失败: %v\n", err)
			return ExitFailure
		}
		fmt.Fprint(c.Stdout, strings.Join(tail, "\n"))
		return ExitOK
	}
	var offset int64
	for {
		newLines, next, err := mgr.LogSince(id, offset)
		if err == nil {
			for _, ln := range newLines {
				fmt.Fprintln(c.Stdout, ln)
			}
			offset = next
		}
		cur := mgr.Get(id)
		if cur == nil || cur.Status == protocol.JobDone || cur.Status == protocol.JobFailed || cur.Status == protocol.JobCancelled {
			return ExitOK
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// cmdJobCancel:cs job cancel <id> [--force]
func cmdJobCancel(c *Ctx, args []string) int {
	pos := positional(args)
	if len(pos) == 0 {
		fmt.Fprintln(c.Stderr, "[cs] 用法: cs job cancel <任务号> [--force]")
		return ExitUsage
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		fmt.Fprintf(c.Stderr, "[cs] 任务号非法: %s\n", pos[0])
		return ExitUsage
	}
	force := hasFlag(args, "--force")
	mgr := c.jobs()
	if force {
		fmt.Fprintln(c.Stderr, "[cs] 强制取消会 kill 进程组:游戏目录可能处于半更新状态,需随后执行 cs update 或 msm validate")
		if !confirmPrompt(c, "yes", "强制取消") {
			return ExitConfirm // 3 = 需要确认但未确认(plan §3 退出码)
		}
	}
	out, err := mgr.Cancel(id, force)
	if err != nil {
		fmt.Fprintf(c.Stderr, "[cs] %v\n", err)
		return ExitFailure
	}
	// 跨进程取消(执行该任务的是另一个进程,如守护或另一次 cs 调用):请求已落盘,由执行进程处理
	if note, _ := out["note"].(string); note != "" {
		fmt.Fprintf(c.Stdout, "任务 %d:取消请求已提交 —— %s\n", id, note)
		return ExitOK
	}
	fmt.Fprintf(c.Stdout, "任务 %d:%v\n", id, out["status"])
	return ExitOK
}

// ---- plugins / demo / host cleanup -----------------------------------------

// cmdPlugins:cs plugins sync|deploy
func cmdPlugins(c *Ctx, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.Stderr, "[cs] 用法: cs plugins sync --from <实例> --to a,b [--delete] [--dry-run]")
		fmt.Fprintln(c.Stderr, "          cs plugins deploy <zip 路径> <插件名> [--to a,b] [--no-backup] [--dry-run]")
		return ExitUsage
	}
	switch args[0] {
	case "sync":
		from := strFlag(args, "--from", "main")
		to := splitList(strFlag(args, "--to", ""))
		if len(to) == 0 {
			fmt.Fprintln(c.Stderr, "[cs] 需要 --to <目标,逗号分隔>(base 表示共享安装)")
			return ExitUsage
		}
		return startJob(c, job.Spec{Kind: job.KindPluginSync, Params: map[string]any{
			"from": from, "targets": to, "delete": hasFlag(args, "--delete"), "dryRun": hasFlag(args, "--dry-run"),
		}}, "插件同步")
	case "deploy":
		pos := positional(args)
		if len(pos) < 3 {
			fmt.Fprintln(c.Stderr, "[cs] 用法: cs plugins deploy <zip 路径> <插件名> [--to a,b] [--no-backup] [--dry-run]")
			return ExitUsage
		}
		targets := splitList(strFlag(args, "--to", "base"))
		return startJob(c, job.Spec{Kind: job.KindPluginDeploy, Params: map[string]any{
			"path": pos[1], "name": pos[2], "targets": targets,
			"backup": !hasFlag(args, "--no-backup"), "dryRun": hasFlag(args, "--dry-run"),
		}}, "插件部署")
	default:
		fmt.Fprintf(c.Stderr, "[cs] 未知的 plugins 子命令: %s\n", args[0])
		return ExitUsage
	}
}

// cmdDemo:cs demo path | collect
func cmdDemo(c *Ctx, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.Stderr, "[cs] 用法: cs demo path          查看录像目录")
		fmt.Fprintln(c.Stderr, "          cs demo collect [--all] [--match <场次>] [--dry-run]")
		return ExitUsage
	}
	switch args[0] {
	case "path":
		fmt.Fprintf(c.Stdout, "%s\n", c.Cfg.DemoDir)
		return ExitOK
	case "collect":
		matchID := strFlag(args, "--match", "")
		all := hasFlag(args, "--all") || matchID == ""
		return startJob(c, job.Spec{Kind: job.KindDemoCollect, Params: map[string]any{
			"matchId": matchID, "all": all, "dryRun": hasFlag(args, "--dry-run"),
		}}, "录像归集")
	default:
		fmt.Fprintf(c.Stderr, "[cs] 未知的 demo 子命令: %s\n", args[0])
		return ExitUsage
	}
}

// cmdHost:cs host cleanup
func cmdHost(c *Ctx, args []string) int {
	if len(args) == 0 || args[0] != "cleanup" {
		fmt.Fprintln(c.Stderr, "[cs] 用法: cs host cleanup --patterns backup:old,logs:rotate,bridge:old [--max-age-days 30] [--yes]")
		fmt.Fprintln(c.Stderr, "          (默认 dry-run 只报告;--yes 并输入确认串才真删)")
		return ExitUsage
	}
	patterns := splitList(strFlag(args, "--patterns", ""))
	if len(patterns) == 0 {
		fmt.Fprintln(c.Stderr, "[cs] 需要 --patterns(白名单:backup:old / logs:rotate / sniper:stubs / stale-instance-dirs / bridge:old)")
		return ExitUsage
	}
	confirm := hasFlag(args, "--yes")
	params := map[string]any{
		"patterns": patterns, "maxAgeDays": intFlag(args, "--max-age-days", 30),
		"dryRun": !confirm,
	}
	if confirm {
		params["confirm"] = "CLEAN"
	}
	return startJob(c, job.Spec{Kind: job.KindHostCleanup, Params: params, Confirm: confirmString(confirm), Origin: job.OriginCLI}, "主机清理")
}

func confirmString(yes bool) string {
	if yes {
		return "CLEAN"
	}
	return ""
}

// ---- 参数小工具(与现有 CLI 手感一致:手写解析,不引第三方)------------------

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func strFlag(args []string, name, def string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return def
}

func intFlag(args []string, name string, def int) int {
	if v := strFlag(args, name, ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// positional 返回非选项参数(跳过 --flag 及其值)。
func positional(args []string) []string {
	out := make([]string, 0, len(args))
	flagsWithValue := map[string]bool{"--from": true, "--to": true, "--lines": true, "--match": true, "--patterns": true, "--max-age-days": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if flagsWithValue[a] {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// jobs 在 Ctx 里懒加载 job.Manager(CLI 进程路径)。
func (c *Ctx) jobs() *job.Manager {
	if c.jobsMgr != nil {
		return c.jobsMgr
	}
	dir := filepath.Join(filepath.Dir(c.ConfigPath), "jobs")
	st := agent.LoadState(agent.StatePathFor(c.ConfigPath))
	c.jobsMgr = job.New(job.Options{
		Dir:    dir,
		Cfg:    func() *config.Config { return c.Cfg },
		Paths:  c.Paths,
		Runner: c.Runner,
		Names: func() []string {
			if ns := st.Names(); len(ns) > 0 {
				return ns
			}
			return registry.Names(c.Instances)
		},
		GroupID: func() string {
			if id := st.Snapshot().ServerID; id != "" {
				return id
			}
			return "local"
		},
		RegistryPath: registry.PathFor(c.ConfigPath),
		Logger:       func(format string, args ...any) { fmt.Fprintf(c.Stderr, format+"\n", args...) },
	})
	return c.jobsMgr
}
