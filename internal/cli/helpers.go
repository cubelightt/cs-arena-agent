// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package cli

import (
	"os"

	"arena/agent/internal/agent"
	"arena/agent/internal/protocol"
)

// stateView 是 state.json 的展示视图(锁/维护态;离线时的"缓存于 <时间>"口径)。
type stateView struct {
	ServerID      string
	ConfigVersion int64
	Locks         map[string]protocol.LockState
	Maintenance   []protocol.Maintenance
}

// loadStateFile 读桥侧 state.json;不存在返回 nil(CLI 明确显示"未知",不假装正常)。
func loadStateFile(configPath string) *stateView {
	data := agent.LoadState(agent.StatePathFor(configPath)).Snapshot()
	if data.ServerID == "" && len(data.Instances) == 0 && data.Locks == nil && data.Maintenance == nil {
		return nil
	}
	return &stateView{
		ServerID:      data.ServerID,
		ConfigVersion: data.ConfigVersion,
		Locks:         data.Locks,
		Maintenance:   data.Maintenance,
	}
}

func statSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func versionString() string {
	if Version == "" {
		return "dev"
	}
	return Version
}
