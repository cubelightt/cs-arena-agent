// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"arena/agent/internal/host"
)

// statusRow 是一行实例状态(cs status / --json 共用)。
type statusRow struct {
	Idx       int    `json:"idx"`
	Name      string `json:"name"`
	Port      int    `json:"port"`
	GotvPort  int    `json:"gotvPort"`
	Running   bool   `json:"running"`
	State     string `json:"state"` // RUNNING / STOPPED(tmux 判定)
	Lock      string `json:"lock"`  // 平台锁(idle/in_match/…);离线未知
	MatchID   *int64 `json:"matchId,omitempty"`
	LogBytes  int64  `json:"logBytes"`
	LogFile   string `json:"logFile,omitempty"`
	BuildID   string `json:"buildId,omitempty"`
	LockStale bool   `json:"lockStale,omitempty"`
}

// cmdStatus 实现 `cs status [<n|all>] [--json]`。
func cmdStatus(c *Ctx, args []string) int {
	arg := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			arg = a
			break
		}
	}

	rows, err := c.targets(arg)
	if err != nil {
		fmt.Fprintf(c.Stderr, "[cs] %v\n", err)
		return ExitFailure
	}

	// 主机概况(磁盘/版本)一次性取;latest 查询失败只降级
	info := c.hostInfo(true)
	snap := c.stateSnapshot()

	out := make([]statusRow, 0, len(rows))
	for _, inst := range rows {
		row := statusRow{Idx: inst.Idx, Name: inst.Name, Port: inst.Port, GotvPort: inst.GotvPort}
		row.State = c.instanceState(inst.Name)
		row.Running = row.State == "RUNNING"
		if snap != nil {
			if lock, ok := snap.Locks[inst.Name]; ok {
				row.Lock = lock.State
				row.MatchID = lock.MatchID
			} else {
				row.Lock = "idle"
			}
		} else {
			row.Lock = "未知(离线/无缓存)"
			row.LockStale = true
		}
		if path, _ := c.logPathOf(inst.Name); path != "" {
			row.LogFile = path
			if fi, err := statSize(path); err == nil {
				row.LogBytes = fi
			}
		}
		row.BuildID = info.BuildID
		out = append(out, row)
	}

	if c.JSON {
		payload := map[string]any{
			"host":            info,
			"instances":       out,
			"agent":           c.Status,
			"instancesSource": instancesSource(c),
			"maintenance":     maintenanceList(c),
		}
		raw, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Fprintln(c.Stdout, string(raw))
		return ExitOK
	}

	// 文本输出
	name := hostname()
	fmt.Fprintf(c.Stdout, "CS 主机 agent %s · %s\n", versionString(), name)
	fmt.Fprintf(c.Stdout, "%s\n", c.agentLine())
	for _, r := range out {
		lock := r.Lock
		if r.MatchID != nil {
			lock = fmt.Sprintf("%s #%d", r.Lock, *r.MatchID)
		}
		fmt.Fprintf(c.Stdout, "  #%-2d %-10s %5d/%5d  %-8s %-14s build %-10s log %s\n",
			r.Idx, r.Name, r.Port, r.GotvPort, r.State, lock, orDash(r.BuildID), host.HumanBytes(r.LogBytes))
	}
	fmt.Fprintf(c.Stdout, "%s\n", c.diskLine(info))
	fmt.Fprintf(c.Stdout, "%s\n", c.versionLine(info))
	if m := maintenanceList(c); len(m) > 0 {
		fmt.Fprintf(c.Stdout, "维护: %s\n", strings.Join(m, "、"))
	} else {
		fmt.Fprintf(c.Stdout, "维护: 无\n")
	}
	if demoDir := c.Cfg.DemoDir; demoDir != "" {
		size, count := host.DirSize(demoDir)
		fmt.Fprintf(c.Stdout, "录像目录 %s(%s / %d 个文件)\n", demoDir, host.HumanBytes(size), count)
	}
	return ExitOK
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// hostInfo 汇总磁盘/版本;withLatest=true 时查官方最新 buildid(失败降级)。
func (c *Ctx) hostInfo(withLatest bool) host.Info {
	info := host.Info{Hostname: hostname(), MSMDir: c.Cfg.MSMDir}
	base := host.BaseDir(c.Cfg.MSMDir)
	if total, free, pct, err := host.DiskUsage(base); err == nil {
		info.DiskTotal, info.DiskFree, info.DiskUsedPct = total, free, pct
	}
	info.ManifestPath = host.ManifestPath(c.Cfg.MSMDir)
	if bid, err := host.ReadBuildID(info.ManifestPath); err == nil {
		info.BuildID = bid
	}
	if withLatest {
		if latest, err := host.LatestBuildID(context.Background()); err == nil {
			info.LatestBuild = latest
		} else {
			info.LatestError = err.Error()
		}
	}
	return info
}

func (c *Ctx) diskLine(info host.Info) string {
	if info.DiskTotal == 0 {
		return "磁盘: 未知(未命中 base 目录)"
	}
	warn := ""
	if c.Cfg.MinFreeBytes > 0 && info.DiskFree < c.Cfg.MinFreeBytes {
		warn = fmt.Sprintf("  ⚠ 低于阈值 %s", host.HumanBytes(c.Cfg.MinFreeBytes))
	}
	return fmt.Sprintf("磁盘 剩 %s(%.0f%% 已用)%s", host.HumanBytes(info.DiskFree), info.DiskUsedPct, warn)
}

func (c *Ctx) versionLine(info host.Info) string {
	switch {
	case info.BuildID == "":
		return "版本 未知(appmanifest 读不到 buildid)"
	case info.LatestBuild == "":
		return fmt.Sprintf("版本 %s(官方最新查询失败: %s)", info.BuildID, info.LatestError)
	case info.BuildID == info.LatestBuild:
		return fmt.Sprintf("版本 已是最新(%s)", info.BuildID)
	default:
		return fmt.Sprintf("版本 %s → 最新 %s,可执行 cs update", info.BuildID, info.LatestBuild)
	}
}

// agentLine 展示守护与后端连接(取自心跳文件;离线时明确标降级)。
func (c *Ctx) agentLine() string {
	st := c.Status
	if st == nil {
		return "后端 未知(守护未运行或未写过心跳:agent.status.json)"
	}
	conn := "未连接"
	if st.Connected {
		conn = fmt.Sprintf("已连接(最近帧 %s)", humanAgo(st.LastFrameAt))
	}
	fresh := ""
	if !st.Fresh(30 * time.Second) {
		fresh = "(心跳陈旧,守护可能已退出)"
	}
	return fmt.Sprintf("后端 %s %s%s  清单来源 %s(%d 个实例)", st.BackendWS, conn, fresh, instancesSource(c), st.InstanceCount)
}

func humanAgo(ms int64) string {
	if ms == 0 {
		return "无"
	}
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Second:
		return "<1s 前"
	case d < time.Minute:
		return fmt.Sprintf("%ds 前", int(d.Seconds()))
	default:
		return fmt.Sprintf("%dm 前", int(d.Minutes()))
	}
}

func instancesSource(c *Ctx) string {
	if c.Status == nil {
		return "none"
	}
	if c.Status.InstancesSource == "" {
		return "none"
	}
	return c.Status.InstancesSource
}

func maintenanceList(c *Ctx) []string {
	// 本地进行中的任务优先(CLI 自己起的任务,守护心跳可能还没刷新)
	if c.jobsMgr != nil {
		if active := c.jobsMgr.ActiveGroups(); len(active) > 0 {
			out := make([]string, 0, len(active))
			for _, m := range active {
				out = append(out, fmt.Sprintf("组 %s %s", m.GroupID, m.Reason))
			}
			return out
		}
	}
	st := c.Status
	if st == nil || st.MaintenanceGroup == "" {
		if snap := c.stateSnapshot(); snap != nil {
			var out []string
			for _, m := range snap.Maintenance {
				if m.Enabled {
					out = append(out, fmt.Sprintf("组 %s(%s)", m.GroupID, m.Reason))
				}
			}
			if len(out) > 0 {
				return out
			}
		}
		return nil
	}
	return []string{fmt.Sprintf("组 %s 维护中", st.MaintenanceGroup)}
}

// stateSnapshot 读桥侧 state.json(离线缓存的锁/维护态)。
func (c *Ctx) stateSnapshot() *stateView {
	st := loadStateFile(c.ConfigPath)
	if st == nil {
		return nil
	}
	return st
}
