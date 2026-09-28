// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package host 收集主机级只读信息:磁盘、游戏版本(buildid)、tmux 进程、残留占用。
//
// 全部走本地文件与系统调用 —— **不依赖后端**,离线也能用。
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Info 是主机概况快照(cs status / cs doctor / M3 的 host_status op 共用)。
type Info struct {
	Hostname     string  `json:"hostname"`
	MSMDir       string  `json:"msmDir"`
	DiskTotal    int64   `json:"diskTotalBytes"`
	DiskFree     int64   `json:"diskFreeBytes"`
	DiskUsedPct  float64 `json:"diskUsedPct"`
	BuildID      string  `json:"buildId,omitempty"`
	ManifestPath string  `json:"manifestPath,omitempty"`
	LatestBuild  string  `json:"latestBuildId,omitempty"`
	LatestError  string  `json:"latestError,omitempty"`
}

// BaseDir 返回 msm 布局下的 base 目录(<msm_dir>/../msm.d/cs2/base)。
func BaseDir(msmDir string) string {
	return filepath.Clean(filepath.Join(msmDir, "..", "msm.d", "cs2", "base"))
}

// ManifestPath 是 CS2 的 appmanifest_730.acf(buildid 真源)。
func ManifestPath(msmDir string) string {
	return filepath.Join(BaseDir(msmDir), "steamapps", "appmanifest_730.acf")
}

// DiskUsage 用 statvfs 读磁盘余量(取 path 所在挂载点)。
func DiskUsage(path string) (total, free int64, usedPct float64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	total = int64(st.Blocks) * int64(st.Bsize)
	free = int64(st.Bavail) * int64(st.Bsize)
	if total > 0 {
		usedPct = float64(total-free) / float64(total) * 100
	}
	return total, free, usedPct, nil
}

var buildIDRe = regexp.MustCompile(`"buildid"\s+"(\d+)"`)

// ReadBuildID 从 appmanifest_730.acf 读 buildid(Valve 的 KV 文本格式,正则足够稳)。
func ReadBuildID(manifestPath string) (string, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("读 appmanifest 失败: %s(%v)", manifestPath, err)
	}
	m := buildIDRe.FindSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("appmanifest 里没有 buildid: %s", manifestPath)
	}
	return string(m[1]), nil
}

// LatestBuildIDTimeboxed 与 LatestBuildID 相同,但自带超时(host_status 不允许卡住面板)。
func LatestBuildIDTimeboxed(d time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return LatestBuildID(ctx)
}

// LatestBuildID 查 Steam 官方最新 public buildid(主机出网;失败只降级,不阻断)。
func LatestBuildID(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.steamcmd.net/v1/info/730", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("steamcmd api HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	var parsed struct {
		Data map[string]struct {
			Depots struct {
				Branches map[string]struct {
					BuildID string `json:"buildid"`
				} `json:"branches"`
			} `json:"depots"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	app, ok := parsed.Data["730"]
	if !ok {
		return "", fmt.Errorf("steamcmd api 返回里没有 app 730")
	}
	pub, ok := app.Depots.Branches["public"]
	if !ok || pub.BuildID == "" {
		return "", fmt.Errorf("steamcmd api 返回里没有 public buildid")
	}
	return pub.BuildID, nil
}

// TmuxSocket 是实例的 tmux socket 路径(msm 每实例一个)。
func TmuxSocket(msmDir, instance string) string {
	return filepath.Clean(filepath.Join(msmDir, "..", "msm.d", "cs2", "inst-"+instance, "msm.d", "tmp", "server.tmux-socket"))
}

// TmuxRunning 判断实例的 tmux 会话是否在(等价"实例进程在跑")。
func TmuxRunning(msmDir, instance string) bool {
	cmd := exec.Command("tmux", "-S", TmuxSocket(msmDir, instance), "has-session", "-t", "cs2@"+instance)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run() == nil
}

// InstanceLogDir 是实例控制台日志目录(msm 布局:<msm_dir>/../msm.d/cs2/log/inst-<name>)。
func InstanceLogDir(msmDir, instance string) string {
	return filepath.Clean(filepath.Join(msmDir, "..", "msm.d", "cs2", "log", "inst-"+instance))
}

// DirSize 递归统计目录字节数与前 N 大条目(残留盘点用;目录不存在返回 0)。
func DirSize(path string) (int64, int) {
	var total int64
	var count int
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		count++
		return nil
	})
	return total, count
}

// TopEntries 返回目录下按字节数降序的前 n 个条目(残留 top5 展示用)。
func TopEntries(path string, n int) []Entry {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	var out []Entry
	for _, e := range entries {
		full := filepath.Join(path, e.Name())
		size, count := DirSize(full)
		out = append(out, Entry{Path: full, Bytes: size, Files: count})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Bytes > out[j-1].Bytes; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// Entry 是一个目录条目的大小统计。
type Entry struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
}

// CheckDeps 检查 msm/steamcmd 需要的外部命令(缺失项返回名字清单)。
func CheckDeps() []string {
	required := []string{"tmux", "wget", "tar", "jq", "inotifywait"}
	var missing []string
	for _, c := range required {
		if _, err := exec.LookPath(c); err != nil {
			missing = append(missing, c)
		}
	}
	return missing
}

// HumanBytes 便于 CLI 展示(13 G / 512 M)。
func HumanBytes(n int64) string {
	const (
		ki = 1024
		mi = ki * 1024
		gi = mi * 1024
	)
	switch {
	case n >= gi:
		return fmt.Sprintf("%.1f G", float64(n)/float64(gi))
	case n >= mi:
		return fmt.Sprintf("%.1f M", float64(n)/float64(mi))
	case n >= ki:
		return fmt.Sprintf("%.1f K", float64(n)/float64(ki))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ReadTextFile 读文本文件(辅助:CLI 展示 server.conf 等)。
func ReadTextFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// JoinLines 把多行合并为一行展示(CLI 里压缩长文案)。
func JoinLines(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// InstanceDir 是实例目录(<msm_dir>/../msm.d/cs2/inst-<name>)。
func InstanceDir(msmDir, instance string) string {
	return filepath.Clean(filepath.Join(msmDir, "..", "msm.d", "cs2", "inst-"+instance))
}

// ProcInfo 是实例进程快照。
type ProcInfo struct {
	Running   bool   `json:"running"`
	PID       int    `json:"pid"`
	UptimeSec int64  `json:"uptimeSec"`
	Session   string `json:"tmuxSession"`
}

// ProcessInfo 取实例进程信息:tmux 会话是否存在 + pane 进程 pid + 进程启动时长(/proc)。
func ProcessInfo(msmDir, instance string) ProcInfo {
	info := ProcInfo{Session: "cs2@" + instance}
	socket := TmuxSocket(msmDir, instance)
	pidOut, err := exec.Command("tmux", "-S", socket, "list-panes", "-t", info.Session, "-F", "#{pane_pid}").Output()
	if err == nil {
		info.Running = true
		fields := strings.Fields(string(pidOut))
		if len(fields) > 0 {
			if pid, err := strconv.Atoi(fields[0]); err == nil {
				info.PID = pid
				info.UptimeSec = processUptime(pid)
			}
		}
	}
	return info
}

// processUptime 用 /proc/<pid>/stat 的 starttime(第 22 字段)与 /proc/stat 的 btime 估算秒数。
func processUptime(pid int) int64 {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// 进程名可能含空格/括号:取最后一个 ')' 之后的字段
	idx := strings.LastIndex(string(raw), ")")
	if idx < 0 {
		return 0
	}
	fields := strings.Fields(string(raw)[idx+1:])
	if len(fields) < 20 {
		return 0
	}
	startTicks, err := strconv.ParseInt(fields[19], 10, 64) // 第 22 字段 = 索引 19
	if err != nil {
		return 0
	}
	btime := bootTime()
	if btime == 0 {
		return 0
	}
	startSec := btime + startTicks/100 // HZ 假定 100(Linux 常见)
	up := time.Now().Unix() - startSec
	if up < 0 {
		return 0
	}
	return up
}

func bootTime() int64 {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "btime ") {
			if v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64); err == nil {
				return v
			}
		}
	}
	return 0
}

// Memory 读 /proc/meminfo 的总量与可用量(字节)。
func Memory() (total, available int64) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = val * 1024
		case "MemAvailable:":
			available = val * 1024
		}
	}
	return total, available
}

// ManifestMtime 取 appmanifest 的 mtime(游戏文件更新时间)。
func ManifestMtime(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime().Unix()
	}
	return 0
}

// HostIP 取默认出口 IP(尽力而为,失败返回空)。
func HostIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}

// StaleInstanceDirs 列出 cfg 里已无对应 inst-* 的实例目录(残留,只读).
func StaleInstanceDirs(msmDir string, known []string) []string {
	root := filepath.Clean(filepath.Join(msmDir, "..", "msm.d", "cs2"))
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	knownSet := map[string]bool{}
	for _, k := range known {
		knownSet[k] = true
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "inst-") {
			continue
		}
		name := strings.TrimPrefix(e.Name(), "inst-")
		if !knownSet[name] {
			out = append(out, e.Name())
		}
	}
	return out
}
