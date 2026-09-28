// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"arena/agent/internal/fsx"
	"arena/agent/internal/protocol"
)

// StatePathFor 与配置同目录(state.json 是配置目录的一部分,便于整目录备份/迁移)。
func StatePathFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "state.json")
}

// StateData 是 state.json 的落盘形状(无锁,可安全按值返回快照)。
//
// 权威在平台 DB(实例清单/元数据/维护态),这里只是**离线降级用**的副本:
//   - 上线后由 hello_ack / config_sync / state_sync 覆盖;
//   - 从未连过后端时 Instances 为空 —— 不扫主机目录瞎猜(决策 10)。
type StateData struct {
	ServerID      string                        `json:"serverId,omitempty"`
	Instances     []string                      `json:"instances"`
	InstanceMeta  []protocol.InstanceMeta       `json:"instanceMeta,omitempty"`
	Maintenance   []protocol.Maintenance        `json:"maintenance,omitempty"`
	Locks         map[string]protocol.LockState `json:"locks,omitempty"`
	ConfigVersion int64                         `json:"configVersion,omitempty"`
	DemoDir       string                        `json:"demoDir,omitempty"`
	// Pending 是"待上报队列"(离线期间的建删/job;M1/M3 落地,先占位)
	Pending []json.RawMessage `json:"pending,omitempty"`
	// FromBackend 是运行期标记(不落盘):清单是否来自后端下发
	FromBackend bool `json:"-"`
}

// State 持有 StateData 并负责持久化。
type State struct {
	mu   sync.Mutex
	path string
	data StateData
}

// LoadState 读取 state.json;文件不存在或损坏都按"从未连过"处理(不阻断启动)。
func LoadState(path string) *State {
	st := &State{path: path, data: StateData{FromBackend: false}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	var d StateData
	if err := json.Unmarshal(raw, &d); err == nil {
		st.data = d
	}
	return st
}

func (s *State) Path() string { return s.path }

// Save 原子落盘(桥被 kill 也不会留下半截 JSON)。
func (s *State) Save() error {
	s.mu.Lock()
	d := s.data
	s.mu.Unlock()

	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	_, err = fsx.AtomicWriteText(s.path, string(raw)+"\n")
	return err
}

// Snapshot 返回只读快照(state 不带锁,可安全传值)。
func (s *State) Snapshot() StateData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

// Names 返回当前运行时白名单(hello_ack 下发 → 本地注册表 → 缓存)。
func (s *State) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.data.Instances))
	copy(out, s.data.Instances)
	return out
}

// ApplyHelloAck 用后端权威值覆盖本地缓存。
func (s *State) ApplyHelloAck(ack *protocol.Inbound) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ack.ServerID != "" {
		s.data.ServerID = ack.ServerID
	}
	s.data.Instances = normalizeNames(ack.Instances)
	s.data.InstanceMeta = ack.InstanceMeta
	s.data.Maintenance = ack.Maintenance
	if ack.ConfigVersion != 0 {
		s.data.ConfigVersion = ack.ConfigVersion
	}
	if ack.DemoDir != "" {
		s.data.DemoDir = ack.DemoDir
	}
	s.data.FromBackend = true
}

// ApplyStateSync 刷新锁/维护态快照(仅用于展示,不参与本地写操作门禁)。
func (s *State) ApplyStateSync(in *protocol.Inbound) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.Locks != nil {
		s.data.Locks = in.Locks
	}
	if in.Maintenance != nil {
		s.data.Maintenance = in.Maintenance
	}
}

// MaintenanceOf 返回该组的维护态(nil = 未知)。
func (s *State) MaintenanceOf(groupID string) *protocol.Maintenance {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Maintenance {
		if s.data.Maintenance[i].GroupID == groupID {
			m := s.data.Maintenance[i]
			return &m
		}
	}
	return nil
}

// normalizeNames 去空/去重并保持顺序。
func normalizeNames(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, n := range in {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
