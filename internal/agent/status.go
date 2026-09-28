// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"arena/agent/internal/fsx"
)

// RuntimeStatus 是守护进程写给 CLI 的心跳快照(agent.status.json)。
//
// 用途:`cs status` / `cs doctor` 要能回答"守护在不在、后端连没连、清单从哪来"——
// 这些都是 CLI 看不到的守护态;提供能力降级状态。
type RuntimeStatus struct {
	PID              int    `json:"pid"`
	Version          string `json:"version"`
	BackendWS        string `json:"backendWs"`
	Connected        bool   `json:"connected"`
	ConnectedAt      int64  `json:"connectedAt,omitempty"`
	LastFrameAt      int64  `json:"lastFrameAt,omitempty"`
	Reconnects       int64  `json:"reconnects"`
	InstancesSource  string `json:"instancesSource"` // backend | cache | none
	InstanceCount    int    `json:"instanceCount"`
	MaintenanceGroup string `json:"maintenanceGroup,omitempty"`
	UpdatedAt        int64  `json:"updatedAt"`
}

// StatusPath 与配置同目录(便于整目录备份/迁移)。
func StatusPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "agent.status.json")
}

// StatusWriter 周期写心跳(原子写;失败只记日志,不影响主循环)。
type StatusWriter struct {
	mu   sync.Mutex
	path string
	data RuntimeStatus
}

func NewStatusWriter(configPath, version, backendWS string) *StatusWriter {
	return &StatusWriter{
		path: StatusPath(configPath),
		data: RuntimeStatus{PID: os.Getpid(), Version: version, BackendWS: backendWS},
	}
}

// Update 就地更新字段并落盘。
func (w *StatusWriter) Update(fn func(*RuntimeStatus)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fn(&w.data)
	if w.data.InstanceCount > 0 && w.data.InstancesSource == "" {
		w.data.InstancesSource = "cache"
	}
	if w.data.InstancesSource == "" {
		w.data.InstancesSource = "none"
	}
	w.data.UpdatedAt = time.Now().UnixMilli()
	raw, err := json.MarshalIndent(w.data, "", "  ")
	if err != nil {
		return
	}
	_, _ = fsx.AtomicWriteText(w.path, string(raw)+"\n")
}

// ReadRuntimeStatus 读取心跳;文件不存在/损坏返回 nil。
func ReadRuntimeStatus(configPath string) *RuntimeStatus {
	raw, err := os.ReadFile(StatusPath(configPath))
	if err != nil {
		return nil
	}
	var st RuntimeStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

// touchFrame 记录"刚收到后端数据帧"(每帧调用;只在必要时落盘由 Update 负责)。
func (w *StatusWriter) touchFrame() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.data.LastFrameAt = time.Now().UnixMilli()
	w.mu.Unlock()
}

// Fresh 判断心跳是否新鲜(默认 30s 内更新过)。
func (s *RuntimeStatus) Fresh(d time.Duration) bool {
	if s == nil {
		return false
	}
	return time.Since(time.UnixMilli(s.UpdatedAt)) <= d
}
