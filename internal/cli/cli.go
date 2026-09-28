// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package cli 是 `cs` 的命令行入口:人类子命令与守护模式共用同一个二进制(决策 3.2)。
//
// M0 只落地 agent / selftest / version / help;status/start/stop/console/update/new/del…
// 按里程碑 M2 起陆续补齐。
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"arena/agent/internal/agent"
	"arena/agent/internal/config"
	"arena/agent/internal/job"
	"arena/agent/internal/matchipc"
	"arena/agent/internal/msm"
	"arena/agent/internal/ops"
	"arena/agent/internal/protocol"
	"arena/agent/internal/registry"
)

// Version 由构建时注入(-ldflags "-X arena/agent/internal/cli.Version=…")。
var Version = "dev"

// 退出码约定:0 成功 / 1 业务失败 / 2 用法错误 / 3 需要确认但未确认。
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
	ExitConfirm = 3
)

// Run 执行一次 CLI 调用,返回进程退出码。
func Run(args []string, stdout, stderr io.Writer) int {
	// agent/selftest 自己解析 --config(见各自 FlagSet),故先原样分派,不做全局提取
	if len(args) > 0 {
		switch args[0] {
		case "agent":
			return runAgent(args[1:], stdout, stderr)
		case "selftest":
			return runSelftest(args[1:], stdout, stderr)
		case "install":
			// 新机第一件事就是生成 config.yaml,故 install 不走 loadCtx(配置允许不存在)
			return cmdInstall(args[1:], stdout, stderr)
		case "version", "-v", "--version":
			fmt.Fprintf(stdout, "cs %s(v2 桥 agent)\n", Version)
			return ExitOK
		case "help", "-h", "--help":
			printUsage(stdout)
			return ExitOK
		}
	}

	// 其余命令:`--config`/`--json` 可出现在任意位置
	args, configPath, asJSON := extractGlobalFlags(args)
	if len(args) == 0 {
		printUsage(stdout)
		return ExitOK
	}
	cmd, rest := args[0], args[1:]

	// 其余命令都需要配置(本地注册表/msm 路径/心跳)
	c, code := loadCtx(configPath, stdout, stderr, asJSON)
	if c == nil {
		return code
	}
	switch cmd {
	case "status":
		return cmdStatus(c, rest)
	case "start", "stop", "restart":
		return cmdLifecycle(c, cmd, rest)
	case "send":
		return cmdSend(c, rest)
	case "log":
		return cmdLog(c, rest)
	case "console":
		return cmdConsole(c, rest)
	case "config":
		return cmdConfig(c, rest)
	case "new":
		return cmdNew(c, rest)
	case "del":
		return cmdDel(c, rest)
	case "update":
		return cmdUpdate(c, rest)
	case "doctor":
		return cmdDoctor(c, rest)
	case "jobs":
		return cmdJobs(c, rest)
	case "job":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "[cs] 用法: cs job log <任务号> [-f] | cs job cancel <任务号> [--force]")
			return ExitUsage
		}
		switch rest[0] {
		case "log":
			return cmdJobLog(c, rest[1:])
		case "cancel":
			return cmdJobCancel(c, rest[1:])
		default:
			fmt.Fprintf(stderr, "[cs] 未知的 job 子命令: %s\n", rest[0])
			return ExitUsage
		}
	case "plugins":
		return cmdPlugins(c, rest)
	case "demo":
		return cmdDemo(c, rest)
	case "host":
		return cmdHost(c, rest)
	default:
		fmt.Fprintf(stderr, "未知命令: %s\n\n", cmd)
		printUsage(stderr)
		return ExitUsage
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `用法: cs [--config <config.yaml>] [--json] <命令> [参数]

守护与自检:
  agent --config <f>       守护模式(供 systemd);SIGHUP 热重载、SIGTERM 干净退出
  selftest [--json]        离线自检(不连后端、不碰真实目录)
  version / help

实例状态与生命周期:
  status [<编号|名称|all>]  实例表(端口/GOTV/进程/平台锁/日志大小)+ 磁盘/版本/后端连接
  start|stop|restart <编号|名称|all>
  console <编号|名称>       exec tmux attach(原生交互,Ctrl-D 分离)
  send <编号|名称> <命令>   单条控制台命令(非 ASCII 会丢;中文内容请走文件通道)
  log <编号|名称> [--lines N] [-f]   控制台日志(默认尾部 100 行;-f 跟随)
  new --name <名称> [--port P] [--from <模板实例>] [--idx N]
                           建实例:msm clone → 清 SwiftlyS2 → 拷插件树 → 共享 steamapps
                           → 回读端口 + 写编号注册表;失败自动回滚(不留半成品)
  del <编号|名称> [--yes]  删实例:停服 → 删实例/配置目录(不留备份、编号不回收;输入 yes 确认)

任务(job 框架;按组独占,危险操作需二次确认):
  update [--check] [--yes] 游戏更新:--check 只比对 buildid;无参数=按组全实例更新(输入 yes 确认)
  jobs [--json]            任务列表
  job log <任务号> [-f] [--lines N]   任务日志(默认尾 200 行;-f 跟随)
  job cancel <任务号> [--force]       取消(默认步骤边界;--force 立即 kill 进程组,危险)
  plugins sync --from <实例> --to a,b [--delete] [--dry-run]
  plugins deploy <zip> <插件名> [--to a,b] [--no-backup] [--dry-run]
  demo path                录像目录;demo collect [--all|--match <场次>] [--dry-run]
  host cleanup --patterns backup:old,logs:rotate,bridge:old [--max-age-days 30] [--yes]

主机与配置:
  install [--check] [--token <t>] [--backend-ws <ws>] [--force] [--dry-run]
                          新服务器引导:L0~L5 逐层体检 + 投放 vendored msm + 生成 config.yaml
                          + 安装 systemd user unit(--check 只体检;--force 覆盖已有 msm 会先备份)
  install --arena-patches [--dry-run] [--bot-instance <名>]
                          重放主机侧补丁(L4):MatchZy 自维护版 dll / ArenaDuel / 人机 cfg 链 /
                          gamemode 配额 / gameinfo 搜索路径 / 日志外移与工坊图 symlink。
                          幂等:在位即跳过,只在被 CS2 更新/重装冲掉后补缺;改写前逐文件备份
  doctor [--json]          部署前自检:依赖/msm/游戏文件/磁盘/端口/后端连接/注册表 + 残留 top
  update --check           比对 buildid(退出码 0=已最新 / 1=有更新)
  config get <键> | set <键> <值>     读写 config.yaml(写前备份 + 通知守护热重载)

说明:
  - 桥只跑 reverse 模式:主动连后端 WS(/api/agent),主机不监听任何端口;
  - 实例清单与端口取自主机 msm 布局(cfg/inst-*/server.conf),编号(#{idx})由桥侧注册表维护;
`)
}

// runAgent 守护模式:加载配置 → 起 agent → 等信号。
func runAgent(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config.yaml 路径(必填)")
	foreground := fs.Bool("foreground", false, "前台运行(调试用;systemd 下不加)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *cfgPath == "" {
		fmt.Fprintln(stderr, "缺少 --config: 守护模式必须显式指定 config.yaml(见 agent/v2/README.md)")
		return ExitUsage
	}
	_ = foreground

	cfg, warns, err := config.Load(*cfgPath)
	for _, w := range warns {
		fmt.Fprintf(stderr, "[bridge] WARNING: %s\n", w)
	}
	if err != nil {
		fmt.Fprintf(stderr, "[bridge] %v\n", err)
		return ExitFailure
	}
	store := config.NewStore(*cfgPath, cfg)

	paths := msm.NewPaths(cfg.MSMDir)
	runner := msm.NewRunner(func() msm.RunnerConfig {
		c := store.Get()
		return msm.RunnerConfig{MSM: c.MSM, MSMDir: c.MSMDir, TimeoutS: c.TimeoutS, StateCacheS: c.StateCacheS}
	})

	state := agent.LoadState(agent.StatePathFor(*cfgPath))
	// 本机注册表(扫描 cfg/inst-* + registry.json 已落盘编号):白名单要用,也供 hello/上报镜像
	reg := &regNamesProvider{cfgFn: store.Get, cfgPath: *cfgPath}
	regItems := func() []registry.Instance {
		scanned, err := registry.Scan(store.Get().MSMDir)
		if err != nil {
			return nil
		}
		rf := registry.Load(registry.PathFor(*cfgPath))
		return registry.MergeIdx(scanned, rf.Items, rf.MaxIdx)
	}
	o := &ops.Ops{
		Runner:          runner,
		Paths:           paths,
		ArchiveOverride: cfg.ArchiveDir,
		CfgFn:           store.Get,
		Version:         Version,
		Registry:        regItems,
		RegistryRemoved: func() []string { return registry.Load(registry.PathFor(*cfgPath)).Removed },
		Caps:            func() []string { return agent.Capabilities },
		MaintFn:         func() []protocol.Maintenance { return state.Snapshot().Maintenance },
		FromBackend:     func() bool { return state.Snapshot().FromBackend },
		// 白名单 = 后端下发清单 ∪ 本机注册表(M5:主机侧 cs new 建的实例必须立即可用;
		// 从未连过后端时就是本机注册表本身 —— 与 plan §2.3 e 的离线语义一致)
		Names: func() []string { return unionNames(state.Names(), reg.get()) },
	}
	matchServer := &matchipc.Server{
		// 比赛绑定只认后端 hello_ack 下发的实例清单；本地注册表并集只供一般主机操作。
		Store: &matchipc.Store{Paths: paths, Names: state.Names,
			PrivateDir: filepath.Join(filepath.Dir(*cfgPath), "arena-match")},
		Logf: func(format string, args ...any) { fmt.Fprintf(stderr, "[bridge] "+format+"\n", args...) },
	}
	o.MatchIPC = matchServer
	// 控制台订阅/tail 是进程级状态(跨重连续读),与 ops 相互引用后注入
	o.Hub = ops.NewConsoleHub(o)
	// 任务框架:任务状态与日志落 <config_dir>/jobs/,守护负责把未上报的任务收敛给平台
	o.Jobs = job.New(job.Options{
		Dir:    filepath.Join(filepath.Dir(*cfgPath), "jobs"),
		Cfg:    store.Get,
		Paths:  paths,
		Runner: runner,
		Names:  state.Names,
		GroupID: func() string {
			if id := state.Snapshot().ServerID; id != "" {
				return id
			}
			return "local"
		},
		RegistryPath: registry.PathFor(*cfgPath),
		Logger:       func(format string, args ...any) { fmt.Fprintf(stderr, "[bridge] "+format+"\n", args...) },
	})

	fmt.Fprintf(stderr, "[bridge] cs %s 启动:backend=%s msm_dir=%s\n", Version, cfg.BackendWS, cfg.MSMDir)
	if len(state.Names()) == 0 {
		fmt.Fprintln(stderr, "[bridge] 提示: 尚无实例清单(等待后端 hello_ack 下发)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// SIGHUP:热重载 config.yaml(改 token/地址/阈值无需重启进程)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if _, warns, err := store.Reload(); err != nil {
				fmt.Fprintf(stderr, "[bridge] SIGHUP 重载失败(保留原配置): %v\n", err)
				continue
			} else {
				for _, w := range warns {
					fmt.Fprintf(stderr, "[bridge] WARNING: %s\n", w)
				}
				fmt.Fprintln(stderr, "[bridge] SIGHUP 已重载 config.yaml")
			}
		}
	}()

	status := agent.NewStatusWriter(*cfgPath, Version, cfg.BackendWS)
	status.Update(func(st *agent.RuntimeStatus) {}) // 先落一次盘,CLI 立刻能读到"守护已启动"

	ag := agent.New(agent.Options{
		Cfg:     store,
		Ops:     o,
		State:   state,
		Status:  status,
		Version: Version,
		// 日志格式与 v1 逐字对齐(不带 Go 默认的时间戳前缀),主机排障脚本/经验可沿用
		Logger: log.New(stderr, "", 0),
	})
	matchServer.Report = ag.SendArenaMatchResult
	go matchServer.Run(ctx)
	if err := ag.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "[bridge] agent 退出: %v\n", err)
		return ExitFailure
	}
	fmt.Fprintln(stderr, "[bridge] agent 已停止")
	return ExitOK
}
