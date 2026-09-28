// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package ops

import (
	"os"
	"path/filepath"
	"time"

	"arena/agent/internal/config"
	"arena/agent/internal/host"
	"arena/agent/internal/protocol"
	"arena/agent/internal/registry"
)

// host_status / instances_list:主机级只读盘点。
//
// 设计约束:
//   - **不 fork msm**(面板会周期刷新):进程/端口/tmux 全部走 /proc、tmux socket、server.conf;
//   - 任一子项失败只影响该字段(降级可读),不整体 5xx;
//   - 不上实例白名单(语义就是"管主机")。

// HostStatus 是 host_status 的回包形状(字段名即面板消费的契约)。
type HostStatus struct {
	OK          bool                   `json:"ok"`
	Host        HostBrief              `json:"host"`
	Disk        DiskInfo               `json:"disk"`
	Memory      MemoryInfo             `json:"memory"`
	Game        GameInfo               `json:"game"`
	Instances   []InstanceRuntime      `json:"instances"`
	Residual    map[string]any         `json:"residual"`
	Maintenance []protocol.Maintenance `json:"maintenance"`
	Source      string                 `json:"instancesSource"`
}

type HostBrief struct {
	Name         string   `json:"name"`
	IP           string   `json:"ip"`
	AgentVersion string   `json:"agentVersion"`
	Capabilities []string `json:"capabilities"`
}

type DiskInfo struct {
	TotalBytes   int64   `json:"totalBytes"`
	FreeBytes    int64   `json:"freeBytes"`
	UsedPct      float64 `json:"usedPct"`
	Warn         bool    `json:"warn"`
	MinFreeBytes int64   `json:"minFreeBytes"`
}

type MemoryInfo struct {
	TotalBytes     int64 `json:"totalBytes"`
	AvailableBytes int64 `json:"availableBytes"`
}

type GameInfo struct {
	Installed struct {
		Build string `json:"build"`
		Path  string `json:"steamInfPath"`
		Mtime int64  `json:"mtime"`
	} `json:"installed"`
	Latest struct {
		Build     string `json:"build"`
		QueriedAt int64  `json:"queriedAt"`
		Source    string `json:"source"`
		Error     string `json:"error,omitempty"`
	} `json:"latest"`
	UpdateAvailable bool `json:"updateAvailable"`
}

type InstanceRuntime struct {
	Name          string        `json:"name"`
	Port          int           `json:"port"`
	GotvPort      int           `json:"gotvPort"`
	Idx           int           `json:"idx"`
	Process       host.ProcInfo `json:"process"`
	Health        string        `json:"health"`
	LogPath       string        `json:"logPath,omitempty"`
	LogBytes      int64         `json:"logBytes"`
	AddonsBytes   int64         `json:"addonsBytes"`
	DemBytes      int64         `json:"demBytes"`
	PlatformState string        `json:"platformState,omitempty"`
	MatchID       *int64        `json:"matchId,omitempty"`
}

// handleHostStatus 组装主机概况。
func (o *Ops) handleHostStatus(payload map[string]any) map[string]any {
	cfg := o.Config()
	// 空集合也用 [] 而不是 null:面板不必为"没有实例/没有残留"做 null 分支
	status := HostStatus{OK: true, Instances: []InstanceRuntime{}, Residual: map[string]any{}}

	// 主机身份
	status.Host = HostBrief{
		Name:         hostName(),
		IP:           host.HostIP(),
		AgentVersion: o.Version,
		Capabilities: o.Capabilities(),
	}

	// 磁盘(阈值告警与更新/建实例前置检查共用 min_free_bytes)
	if total, free, pct, err := host.DiskUsage(host.BaseDir(cfg.MSMDir)); err == nil {
		status.Disk = DiskInfo{TotalBytes: total, FreeBytes: free, UsedPct: pct,
			Warn: cfg.MinFreeBytes > 0 && free < cfg.MinFreeBytes, MinFreeBytes: cfg.MinFreeBytes}
	}

	// 内存
	total, avail := host.Memory()
	status.Memory = MemoryInfo{TotalBytes: total, AvailableBytes: avail}

	// 游戏版本:安装的 buildid ↔ 官方最新
	manifest := host.ManifestPath(cfg.MSMDir)
	status.Game.Installed.Path = manifest
	status.Game.Installed.Mtime = host.ManifestMtime(manifest)
	if build, err := host.ReadBuildID(manifest); err == nil {
		status.Game.Installed.Build = build
	} else {
		status.Game.Latest.Error = "读不到本地 buildid: " + err.Error()
	}
	status.Game.Latest.Source = "api.steamcmd.net"
	status.Game.Latest.QueriedAt = time.Now().UnixMilli()
	if latest, err := host.LatestBuildIDTimeboxed(10 * time.Second); err == nil {
		status.Game.Latest.Build = latest
		status.Game.UpdateAvailable = status.Game.Installed.Build != "" && latest != status.Game.Installed.Build
	} else {
		status.Game.Latest.Error = err.Error()
	}

	// 实例运行态(tmux/proc + 日志大小 + addons/demo 占用;平台锁来自 state 缓存由后端补)
	for _, it := range o.Instances() {
		row := InstanceRuntime{
			Name: it.Name, Port: it.Port, GotvPort: it.GotvPort, Idx: it.Idx,
			Process: host.ProcessInfo(cfg.MSMDir, it.Name),
		}
		if row.Process.Running {
			row.Health = "RUNNING"
		} else {
			row.Health = "STOPPED"
		}
		if path, _ := o.Paths.FindLogPath(it.Name); path != "" {
			row.LogPath = path
			if fi, err := os.Stat(path); err == nil {
				row.LogBytes = fi.Size()
			}
		}
		instDir := host.InstanceDir(cfg.MSMDir, it.Name)
		row.AddonsBytes, _ = host.DirSize(filepath.Join(instDir, "game", "csgo", "addons"))
		row.DemBytes, _ = host.DirSize(filepath.Join(instDir, "game", "csgo", "MatchZy"))
		status.Instances = append(status.Instances, row)
	}

	// 残留占用(清理参考;只读)
	home, _ := os.UserHomeDir()
	residual := map[string]any{}
	addResidual := func(key, path string) {
		if path == "" {
			return
		}
		size, count := host.DirSize(path)
		residual[key] = map[string]any{"path": path, "bytes": size, "files": count}
	}
	addResidual("demos", cfg.DemoDir)
	addResidual("backup", filepath.Join(home, "backup"))
	addResidual("msmLog", filepath.Join(cfg.MSMDir, "..", "msm.d", "cs2", "log"))
	addResidual("workshop", filepath.Join(cfg.MSMDir, "..", "msm.d", "cs2", "inst-main", "game", "bin", "linuxsteamrt64", "steamapps", "workshop", "content", "730"))
	addResidual("archives", cfg.ArchiveDir)
	status.Residual = residual
	if stale := host.StaleInstanceDirs(cfg.MSMDir, registry.Names(o.Instances())); len(stale) > 0 {
		residual["staleInstanceDirs"] = stale
	}

	status.Maintenance = o.Maintenance()
	if o.InstancesFromBackend() {
		status.Source = "backend"
	} else if len(o.Instances()) > 0 {
		status.Source = "cache"
	} else {
		status.Source = "none"
	}

	return statusToMap(status)
}

// handleInstancesList 只读实例清单(比 host_status 轻;供面板/CLI 快速刷新)。
func (o *Ops) handleInstancesList() map[string]any {
	cfg := o.Config()
	type row struct {
		Idx       int    `json:"idx"`
		Name      string `json:"name"`
		Port      int    `json:"port"`
		GotvPort  int    `json:"gotvPort"`
		Running   bool   `json:"running"`
		UptimeSec int64  `json:"uptimeSec"`
	}
	out := []row{}
	for _, it := range o.Instances() {
		proc := host.ProcessInfo(cfg.MSMDir, it.Name)
		out = append(out, row{Idx: it.Idx, Name: it.Name, Port: it.Port, GotvPort: it.GotvPort,
			Running: proc.Running, UptimeSec: proc.UptimeSec})
	}
	return map[string]any{"ok": true, "instances": out}
}

// Instances 返回运行时注册表(host ops 用)。
func (o *Ops) Instances() []registry.Instance {
	if o.Registry != nil {
		return o.Registry()
	}
	return nil
}

// Config 返回当前配置(host ops 用;缺失时给安全默认,避免 panic)。
func (o *Ops) Config() *config.Config {
	if o.CfgFn != nil {
		return o.CfgFn()
	}
	return &config.Config{MinFreeBytes: config.DefaultMinFreeBytes}
}

func (o *Ops) Capabilities() []string {
	if o.Caps != nil {
		return o.Caps()
	}
	return nil
}

func (o *Ops) Maintenance() []protocol.Maintenance {
	if o.MaintFn != nil {
		return o.MaintFn()
	}
	return []protocol.Maintenance{}
}

func (o *Ops) InstancesFromBackend() bool {
	if o.FromBackend != nil {
		return o.FromBackend()
	}
	return false
}

func hostName() string {
	if h, err := os.Hostname(); err != nil {
		return "unknown"
	} else {
		return h
	}
}

// statusToMap 把结构体转成 map(回包统一是 map[string]any)。
func statusToMap(st HostStatus) map[string]any {
	return map[string]any{
		"ok":              st.OK,
		"host":            st.Host,
		"disk":            st.Disk,
		"memory":          st.Memory,
		"game":            st.Game,
		"instances":       st.Instances,
		"residual":        st.Residual,
		"maintenance":     st.Maintenance,
		"instancesSource": st.Source,
	}
}
