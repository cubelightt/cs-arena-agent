// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"arena/agent/internal/fsx"
	"arena/agent/internal/host"
	"arena/agent/internal/registry"
)

// cmdLifecycle 实现 `cs start|stop|restart <n|all>`(经 msm;维护中的组拒绝)。
func cmdLifecycle(c *Ctx, op string, args []string) int {
	arg := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			arg = a
			break
		}
	}
	if arg == "" {
		fmt.Fprintf(c.Stderr, "[cs] 用法: cs %s <编号|名称|all>\n", op)
		return ExitUsage
	}
	if err := c.assertWritable(op); err != nil {
		fmt.Fprintf(c.Stderr, "[cs] %v\n", err)
		return ExitFailure
	}
	targets, err := c.targets(arg)
	if err != nil {
		fmt.Fprintf(c.Stderr, "[cs] %v\n", err)
		return ExitFailure
	}

	failures := 0
	for _, inst := range targets {
		res, err := c.Runner.Run(inst.Name, op, nil, nil, true)
		ok := err == nil && res != nil && res.Returncode == 0
		if !ok {
			failures++
		}
		detail := ""
		if err != nil {
			detail = err.Error()
		} else if res != nil && res.Returncode != 0 {
			detail = strings.TrimSpace(res.Stderr)
			if detail == "" {
				detail = strings.TrimSpace(res.Stdout)
			}
		}
		state := c.instanceState(inst.Name)
		if ok {
			fmt.Fprintf(c.Stdout, "  #%-2d %-10s %s → %s\n", inst.Idx, inst.Name, op, state)
		} else {
			fmt.Fprintf(c.Stderr, "  #%-2d %-10s %s 失败: %s\n", inst.Idx, inst.Name, op, detail)
		}
	}
	if failures > 0 {
		return ExitFailure
	}
	return ExitOK
}

// instanceState 判实例是否在跑:优先 tmux 会话(不 fork msm);tmux 不可用时回退 msm status。
func (c *Ctx) instanceState(name string) string {
	if _, err := exec.LookPath("tmux"); err == nil {
		if host.TmuxRunning(c.Cfg.MSMDir, name) {
			return "RUNNING"
		}
		return "STOPPED"
	}
	return c.Runner.Status(name, true)
}

// cmdSend 实现 `cs send <n> <命令>`(单条,非 ASCII 会丢,提示改用文件通道)。
func cmdSend(c *Ctx, args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(c.Stderr, "[cs] 用法: cs send <编号|名称> <控制台命令>\n")
		return ExitUsage
	}
	inst, err := c.resolve(args[0])
	if err != nil {
		fmt.Fprintf(c.Stderr, "[cs] %v\n", err)
		return ExitFailure
	}
	cmdline := strings.Join(args[1:], " ")
	res, err := c.Runner.Run(inst.Name, "send", []string{cmdline}, nil, true)
	if err != nil || res == nil || res.Returncode != 0 {
		detail := "未知错误"
		if err != nil {
			detail = err.Error()
		} else if res != nil {
			detail = strings.TrimSpace(firstNonEmptyStr(res.Stderr, res.Stdout))
		}
		fmt.Fprintf(c.Stderr, "[cs] %s send 失败: %s\n", inst.Name, detail)
		return ExitFailure
	}
	fmt.Fprintf(c.Stdout, "已下发到 %s: %s\n", inst.Name, cmdline)
	if !isASCII(cmdline) {
		fmt.Fprintf(c.Stderr, "[cs] 注意: 控制台通道(tmux)会丢非 ASCII;中文等内容请走 matchfile 文件通道\n")
	}
	return ExitOK
}

// cmdLog 实现 `cs log <n> [--lines N] [-f]`。
func cmdLog(c *Ctx, args []string) int {
	lines := 100
	follow := false
	instArg := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-f" || a == "--follow":
			follow = true
		case a == "--lines" || a == "-n":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &lines)
				i++
			}
		case strings.HasPrefix(a, "--lines="):
			fmt.Sscanf(strings.TrimPrefix(a, "--lines="), "%d", &lines)
		case !strings.HasPrefix(a, "-"):
			instArg = a
		}
	}
	if instArg == "" {
		fmt.Fprintf(c.Stderr, "[cs] 用法: cs log <编号|名称> [--lines N] [-f]\n")
		return ExitUsage
	}
	inst, err := c.resolve(instArg)
	if err != nil {
		fmt.Fprintf(c.Stderr, "[cs] %v\n", err)
		return ExitFailure
	}
	path, tried := c.logPathOf(inst.Name)
	if path == "" {
		fmt.Fprintf(c.Stderr, "[cs] 未找到 %s 的控制台日志;已尝试:\n  %s\n", inst.Name, strings.Join(tried, "\n  "))
		return ExitFailure
	}
	if !follow {
		out, err := fsx.ReadTail(path, lines)
		if err != nil {
			fmt.Fprintf(c.Stderr, "[cs] 读日志失败: %v\n", err)
			return ExitFailure
		}
		fmt.Fprint(c.Stdout, strings.Join(out, ""))
		return ExitOK
	}

	// -f:从当前尾部开始跟随(offset 语义同桥:只推进到最后一个完整行)
	var offset int64
	if fi, err := os.Stat(path); err == nil {
		offset = fi.Size()
	}
	fmt.Fprintf(c.Stderr, "[cs] 跟随 %s(Ctrl-C 退出)\n", path)
	for {
		fi, err := os.Stat(path)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if fi.Size() < offset {
			offset = 0 // 日志轮换
		}
		chunk, next, err := fsx.ReadNewLines(path, offset)
		if err == nil && len(chunk) > 0 {
			for _, l := range chunk {
				fmt.Fprintln(c.Stdout, l)
			}
			offset = next
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// cmdConsole 实现 `cs console <n>`:exec tmux attach(原生交互,Ctrl-D 分离)。
func cmdConsole(c *Ctx, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(c.Stderr, "[cs] 用法: cs console <编号|名称>\n")
		return ExitUsage
	}
	inst, err := c.resolve(args[0])
	if err != nil {
		fmt.Fprintf(c.Stderr, "[cs] %v\n", err)
		return ExitFailure
	}
	socket := host.TmuxSocket(c.Cfg.MSMDir, inst.Name)
	if _, err := os.Stat(socket); err != nil {
		fmt.Fprintf(c.Stderr, "[cs] 实例 %s 的 tmux socket 不存在(%s)—— 实例可能没在运行\n", inst.Name, socket)
		return ExitFailure
	}
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		fmt.Fprintf(c.Stderr, "[cs] 找不到 tmux: %v\n", err)
		return ExitFailure
	}
	argv := []string{"tmux", "-S", socket, "attach", "-t", "cs2@" + inst.Name}
	if err := syscall.Exec(tmuxBin, argv, os.Environ()); err != nil {
		fmt.Fprintf(c.Stderr, "[cs] 启动 tmux 失败: %v\n", err)
		return ExitFailure
	}
	return ExitOK // 不会到达
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// instanceLabel 便于日志里展示 #idx name。
func instanceLabel(i registry.Instance) string {
	return fmt.Sprintf("#%d %s", i.Idx, i.Name)
}
