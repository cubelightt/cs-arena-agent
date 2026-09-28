// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// 建/删实例的 CLI:`cs new` / `cs del`。
//
// 与后端面板同一条路径:都走 job 框架的 instance_create / instance_delete
// (状态机/进度/日志/取消复用 —— 面板任务条与 `cs jobs` 看到的是同一个任务)。
package cli

import (
	"fmt"
	"path/filepath"

	"arena/agent/internal/host"
	"arena/agent/internal/job"
	"arena/agent/internal/registry"
)

// cmdNew:cs new [--name X] [--port P] [--from <模板实例>] [--idx N]
//
// 建实例不是破坏性操作,故不要二次确认串;
// 真正的把关是 job 步骤 1 的前置检查(磁盘/msm/来源/重名/端口冲突)+ 失败自动回滚。
func cmdNew(c *Ctx, args []string) int {
	// 手写解析(与 cmd_jobs.go 的其它命令一致):flag 包在**第一个位置参数**处停止解析,
	// 而文档给出的用法是 `cs new --name X …` / `cs del <名> --yes` —— 尾随选项必须也认。
	name := strFlag(args, "--name", "")
	port := intFlag(args, "--port", 0)
	src := strFlag(args, "--from", "")
	idx := intFlag(args, "--idx", 0)
	if name == "" {
		fmt.Fprintln(c.Stderr, "[cs] 用法: cs new --name <实例名> [--port P] [--from <模板实例>] [--idx N]")
		return ExitUsage
	}
	if port != 0 && (port < 1024 || port > 65535) {
		fmt.Fprintln(c.Stderr, "[cs] 端口需在 1024~65535 之间")
		return ExitUsage
	}
	if src == "" {
		src = defaultSourceName(c)
	}
	fmt.Fprintf(c.Stdout, "创建实例计划(组 %s):\n", c.groupID())
	for i, st := range []string{
		"前置检查(磁盘余量/msm 可执行/模板实例/重名/端口冲突)",
		fmt.Sprintf("msm clone:从 %s 克隆出 %s", src, name),
		"清理 SwiftlyS2 残留(addons/swiftlys2、gameinfo.gi、server.conf)",
		"拷贝插件树(纯拷贝,不需要 rsync)",
		"共享 steamapps 符号链接(工坊图零拷贝)",
		"回读端口 + 写编号注册表(registry.json)",
	} {
		fmt.Fprintf(c.Stdout, "  %d) %s\n", i+1, st)
	}
	if port != 0 {
		fmt.Fprintf(c.Stdout, "端口:%d(GOTV %d)\n", port, port+100)
	}
	fmt.Fprintf(c.Stdout, "说明:失败自动回滚(删掉半成品);完成后实例尚未启动,用 `cs start %s` 启动\n", name)

	params := map[string]any{"name": name, "cloneFrom": src}
	if port != 0 {
		params["port"] = port
	}
	if idx != 0 {
		params["idx"] = idx
	}
	return startJob(c, job.Spec{Kind: job.KindInstanceCreate, Params: params, Origin: job.OriginCLI}, "创建实例")
}

// cmdDel:cs del <编号|名称> [--yes]
func cmdDel(c *Ctx, args []string) int {
	yes := hasFlag(args, "--yes") // 跳过交互确认(无人值守;任务层仍要求 confirm = 实例名)
	rest := positional(args)
	if len(rest) == 0 {
		fmt.Fprintln(c.Stderr, "[cs] 用法: cs del <编号|名称> [--yes]")
		return ExitUsage
	}
	inst, err := c.resolve(rest[0])
	if err != nil {
		fmt.Fprintf(c.Stderr, "[cs] %v\n", err)
		return ExitFailure
	}
	// 打印将删除的路径与字节数
	instDir := host.InstanceDir(c.Cfg.MSMDir, inst.Name)
	cfgDir := filepath.Join(registry.CFGDir(c.Cfg.MSMDir), "inst-"+inst.Name)
	var total int64
	fmt.Fprintln(c.Stdout, "将删除(不可恢复,不留备份):")
	for _, p := range []string{instDir, cfgDir} {
		size, files := host.DirSize(p)
		total += size
		fmt.Fprintf(c.Stdout, "  %s(%s / %d 个文件)\n", p, host.HumanBytes(size), files)
	}
	fmt.Fprintf(c.Stdout, "共 %s;实例 #%d %s 端口 %d\n", host.HumanBytes(total), inst.Idx, inst.Name, inst.Port)
	fmt.Fprintf(c.Stdout, "注意:共享 steamapps(工坊图)不在删除范围;平台侧按墓碑收敛删除该行\n")
	if !yes && !confirmPrompt(c, "yes", fmt.Sprintf("删除实例 %s", inst.Name)) {
		return ExitConfirm
	}
	return startJob(c, job.Spec{
		Kind:    job.KindInstanceDelete,
		Params:  map[string]any{"name": inst.Name, "confirm": inst.Name},
		Origin:  job.OriginCLI,
		Confirm: inst.Name,
	}, "删除实例")
}

// defaultSourceName 取默认模板实例:main 优先,否则第一个(与 job 侧同口径,只为打印计划)。
func defaultSourceName(c *Ctx) string {
	if len(c.Instances) == 0 {
		return "main"
	}
	for _, it := range c.Instances {
		if it.Name == "main" {
			return it.Name
		}
	}
	return c.Instances[0].Name
}
