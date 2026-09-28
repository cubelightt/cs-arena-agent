// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// 命令 cs:CS2 Arena 主机侧 agent(守护)+ 人类 CLI —— 一个静态二进制两种模式。
//
// 用法:
//
//	cs agent --config /home/testuser/data/bridge/config.yaml     # 守护(供 systemd)
//	cs selftest [--json]                                     # 离线自检
//	cs version | cs help
//
// 配置与用法见 README.md。
package main

import (
	"os"

	"arena/agent/internal/cli"
)

// 默认发布版本为 v2.0，可在构建时通过 `-ldflags "-X main.version=<标签>"` 覆盖。
var version = "v2.0"

func main() {
	cli.Version = version
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
