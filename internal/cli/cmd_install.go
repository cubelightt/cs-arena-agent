// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// cmd_install:cs install —— 新服务器引导与逐层体检(L0~L5)。
//
// 与其它命令的差别:**配置可以不存在**(新机的第一件事就是生成 config.yaml),
// 故本命令在 Run() 里于 loadCtx 之前分派,自己解析 --config。
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"arena/agent/internal/config"
	"arena/agent/internal/install"
	"arena/agent/internal/registry"
)

func cmdInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "config.yaml", "config.yaml 路径(不存在时会生成)")
	checkOnly := fs.Bool("check", false, "只体检,不写任何文件")
	arenaPatches := fs.Bool("arena-patches", false, "重放主机侧补丁(L4:cfg 链/gameinfo/符号链接;插件由用户自行安装;幂等,只补缺)")
	botInstance := fs.String("bot-instance", "", "增强人机实例名(默认自动判定,退回 "+install.DefaultBotInstance+")")
	force := fs.Bool("force", false, "msm 目录已存在时覆盖(覆盖前自动备份)")
	yes := fs.Bool("yes", false, "跳过交互确认")
	dryRun := fs.Bool("dry-run", false, "只打印将要做什么")
	noCfg := fs.Bool("no-cfg", false, "不生成 config.yaml")
	noUnit := fs.Bool("no-unit", false, "不安装 systemd user unit")
	msmFrom := fs.String("msm-from", "", "外部 msm 源目录(默认用内嵌的 vendored msm)")
	msmDirFlag := fs.String("msm-dir", "", "msm 目标目录(默认取配置里的 msm_dir,否则 ~/cs2-multiserver-new)")
	token := fs.String("token", "", "平台 game_servers.bridge_token(生成 config.yaml 用)")
	backendWS := fs.String("backend-ws", "", "后端 WS 地址,如 ws://192.0.2.28:8080/api/agent")
	asJSON := fs.Bool("json", false, "机器可读输出")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	home, _ := os.UserHomeDir()
	// msm 目录:显式 --msm-dir 优先,否则取配置里的 msm_dir,最后用默认值
	msmDir := install.DefaultMSMDir
	cfgExists := false
	if _, err := os.Stat(*cfgPath); err == nil {
		cfgExists = true
		if cfg, _, err := config.Load(*cfgPath); err == nil && cfg.MSMDir != "" {
			msmDir = cfg.MSMDir
		}
	}
	if *msmDirFlag != "" {
		msmDir = *msmDirFlag
	}
	opt := install.Options{
		MSMDir:     msmDir,
		MSMExe:     filepath.Join(expandHomeDir(msmDir, home), "cs2-server"),
		ConfigPath: *cfgPath,
		UnitPath:   filepath.Join(home, ".config", "systemd", "user", "cs-agent.service"),
		Home:       home,
	}
	// 实例清单(有 msm 布局时扫 cfg/inst-*/server.conf;L4 体检用)
	if items, err := registry.Scan(expandHomeDir(msmDir, home)); err == nil {
		opt.Instances = registry.Names(items)
	}

	// ---- 主机侧补丁重做(与投放相互独立:只补缺,不动 msm/config)----
	if *arenaPatches {
		items, err := install.ApplyArenaPatches(install.ArenaPatchOptions{
			MSMDir:      msmDir,
			Instances:   opt.Instances,
			BotInstance: *botInstance,
			Home:        home,
			DryRun:      *dryRun,
		})
		if *asJSON {
			raw, _ := json.MarshalIndent(map[string]any{
				"items": items, "changed": install.Changed(items), "dryRun": *dryRun,
			}, "", "  ")
			fmt.Fprintln(stdout, string(raw))
		} else {
			printArenaPatchReport(stdout, items, *dryRun)
		}
		if err != nil {
			fmt.Fprintf(stderr, "[cs] 补丁重做未完成: %v\n", err)
			return ExitFailure
		}
		return ExitOK
	}

	items := install.Check(opt)
	if *asJSON && *checkOnly {
		raw, _ := json.MarshalIndent(map[string]any{"checks": items, "ok": install.CriticalOK(items)}, "", "  ")
		fmt.Fprintln(stdout, string(raw))
		return exitForCheck(items)
	}
	printCheckReport(stdout, items)

	if *checkOnly {
		printNextSteps(stdout, msmDir, *cfgPath, cfgExists, false)
		return exitForCheck(items)
	}

	// ---- 投放 ----
	if *force && !*yes {
		if !confirmPrompt(confirmCtx(stdout, stderr), "yes", "覆盖已存在的 msm 目录 "+msmDir) {
			return ExitConfirm
		}
	}
	steps, err := install.Apply(install.ApplyOptions{
		Options:   opt,
		MSMFrom:   *msmFrom,
		Force:     *force,
		WriteCfg:  !*noCfg,
		WriteUnit: !*noUnit,
		Token:     *token,
		BackendWS: *backendWS,
		DryRun:    *dryRun,
		Log: func(format string, args ...any) {
			fmt.Fprintf(stdout, format+"\n", args...)
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "[cs] 安装失败: %v\n", err)
		return ExitFailure
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(map[string]any{"steps": steps, "checks": items}, "", "  ")
		fmt.Fprintln(stdout, string(raw))
	}
	printNextSteps(stdout, msmDir, *cfgPath, cfgExists || !*noCfg, !*dryRun)
	return ExitOK
}

// printArenaPatchReport 打印补丁重做报告(逐项 + 汇总)。
func printArenaPatchReport(w io.Writer, items []install.ArenaPatchItem, dryRun bool) {
	title := "== 主机侧补丁重做(L4)=="
	if dryRun {
		title = "== 主机侧补丁重做(L4;dry-run,未写盘)=="
	}
	fmt.Fprintln(w, title)
	mark := map[string]string{"ok": "✓", "fixed": "✎", "warn": "!", "skip": "·"}
	for _, it := range items {
		m := mark[it.Action]
		if m == "" {
			m = "?"
		}
		where := it.Where
		if where == "-" {
			where = " @ 全组"
		} else if where != "" {
			where = " @ " + strings.TrimPrefix(where, "@")
		}
		line := fmt.Sprintf("  %s %s%s", m, it.Item, where)
		if it.Note != "" {
			line += " —— " + it.Note
		}
		fmt.Fprintln(w, line)
	}
	n := map[string]int{}
	for _, it := range items {
		n[it.Action]++
	}
	verb := "本次修复"
	if dryRun {
		verb = "将修复"
	}
	fmt.Fprintf(w, "\n== 汇总 ==\n  已就位 %d · %s %d · 需人工 %d · 跳过 %d\n",
		n["ok"], verb, n["fixed"], n["warn"], n["skip"])
	if n["warn"] > 0 {
		fmt.Fprintln(w, "  (需人工项请按主机部署事实核对后处置)")
	}
}

func exitForCheck(items []install.CheckItem) int {
	if install.CriticalOK(items) {
		return ExitOK
	}
	return ExitFailure
}

func printCheckReport(w io.Writer, items []install.CheckItem) {
	fmt.Fprintln(w, "== 体检(逐层;✗ 为待处理项)==")
	layer := ""
	for _, it := range items {
		if it.Layer != layer {
			layer = it.Layer
			fmt.Fprintf(w, "\n%s\n", layerTitle(layer))
		}
		mark := "✓"
		if !it.OK {
			mark = "✗"
			if !it.Critical {
				mark = "△" // 提示项:缺了不阻塞(例如 steamcmd 会话)
			}
		}
		line := fmt.Sprintf("  %s %s", mark, it.Name)
		if it.Note != "" && !it.OK {
			line += " —— " + it.Note
		} else if it.Note != "" {
			line += "(" + it.Note + ")"
		}
		fmt.Fprintln(w, line)
	}
}

func layerTitle(l string) string {
	switch l {
	case "L0":
		return "L0 root 一次性依赖(需 root;apt install 交运维)"
	case "L1":
		return "L1 Steam 会话(仅 steamcmd 下载需要)"
	case "L2":
		return "L2 msm(管理脚本集)"
	case "L3":
		return "L3 游戏文件(请使用 SteamCMD 安装或导入已有 CS2 服务端文件)"
	case "L4":
		return "L4 主机侧补丁(cfg 链/gameinfo/符号链接)"
	case "L5":
		return "L5 桥自身(config.yaml / systemd unit / 守护)"
	default:
		return l
	}
}

// printNextSteps 打印收尾指引(装没装完都给出下一步)。
func printNextSteps(w io.Writer, msmDir, cfgPath string, cfgOK, applied bool) {
	fmt.Fprintln(w, "\n== 下一步 ==")
	if !cfgOK {
		fmt.Fprintf(w, "  1) 生成 config.yaml:`cs install --token <平台 token> --backend-ws ws://<平台>:8080/api/agent --config %s`\n", cfgPath)
	}
	if applied {
		fmt.Fprintln(w, "  · 启动守护:`systemctl --user daemon-reload && systemctl --user enable --now cs-agent`")
		fmt.Fprintln(w, "    (主机重启自启:`loginctl enable-linger $USER` 需要 root;无 root 可用 crontab @reboot + pgrep 守卫,详见仓库主机运维说明)")
	}
	fmt.Fprintln(w, "  · 复核:`./cs selftest`(离线自检)→ `./cs doctor`(依赖/msm/游戏文件/磁盘/连接)")
	fmt.Fprintf(w, "  · 建实例:`./cs new --name <名称>`;msm 布局:%s\n", msmDir)
	fmt.Fprintln(w, "  · 平台侧:面板「主机概况」看 connected;实例名/端口按平台 DB 为准")
}

func confirmCtx(stdout, stderr io.Writer) *Ctx {
	return &Ctx{Stdout: stdout, Stderr: stderr}
}

// expandHomeDir 展开 ~/ 前缀(cmd 侧小工具)。
func expandHomeDir(p, home string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
}
