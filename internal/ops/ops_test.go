// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package ops

import (
	"path/filepath"
	"testing"

	"arena/agent/internal/msm"
)

func newTestOps(t *testing.T, msmBin string, names []string) *Ops {
	t.Helper()
	r := msm.NewRunner(func() msm.RunnerConfig {
		return msm.RunnerConfig{MSM: msmBin, MSMDir: t.TempDir(), TimeoutS: 5, StateCacheS: 2}
	})
	return &Ops{Runner: r, Paths: msm.NewPaths(t.TempDir()), Names: func() []string { return names }}
}

// ok 恒由 returncode 决定(v1 的"缺省 true"曾把失败吞成成功)。
func TestReplyOkFollowsReturncode(t *testing.T) {
	if got := Reply(&msm.Result{Returncode: 0}, nil); got["ok"] != true {
		t.Fatalf("rc=0 应 ok: %v", got)
	}
	if got := Reply(&msm.Result{Returncode: 3}, nil); got["ok"] != false {
		t.Fatalf("rc=3 应 !ok: %v", got)
	}
	if got := ReplyBusy(nil); got["ok"] != false || got["error"] != "msm busy(实例正被其他命令占用),请重试" {
		t.Fatalf("busy 回包文案不符: %v", got)
	}
}

// 白名单:非清单实例一律拒绝,且不得产生副作用。
func TestInstanceWhitelist(t *testing.T) {
	o := newTestOps(t, "/bin/true", []string{"main"})
	if got := o.Handle("send", "other", map[string]any{"cmd": "x"}); got["ok"] != false {
		t.Fatalf("清单外实例应拒绝: %v", got)
	}
	// health 是主机级 op:不校验实例名
	if got := o.Handle("health", "", nil); got["ok"] != true {
		t.Fatalf("health 不该走白名单: %v", got)
	}
}

func TestUnknownOpAndNotImplemented(t *testing.T) {
	o := newTestOps(t, "/bin/true", []string{"main"})
	if got := o.Handle("nope", "", nil); got["error"] != "unknown op: nope" {
		t.Fatalf("未知 op 文案不符: %v", got)
	}
	// 已知但未实现的 op 必须明确报错(不能静默成功)
	got := o.Handle("matchfile", "main", map[string]any{"filename": "a.cfg"})
	if got["ok"] != false {
		t.Fatalf("未实现 op 应失败: %v", got)
	}
}

// start/restart 的 env 校验失败必须在执行 msm 之前拦下。
func TestStartEnvValidatedBeforeRun(t *testing.T) {
	o := newTestOps(t, "/bin/true", []string{"main"})
	got := o.Handle("start", "main", map[string]any{"env": map[string]any{"bad-key": "1"}})
	if got["ok"] != false || got["error"] != "bad env key: bad-key" {
		t.Fatalf("env 校验应先于 msm: %v", got)
	}
	// status 不接受 env:传了也当没传(与 v1 start_env 一致)
	if got := o.Handle("status", "main", map[string]any{"env": map[string]any{"bad-key": "1"}}); got["ok"] != true {
		t.Fatalf("status 不该校验 env: %v", got)
	}
}

// 命令起不来(可执行不存在)必须收口成 ok:false,而不是 panic/断通道。
func TestExecFailureIsContained(t *testing.T) {
	dir := t.TempDir()
	o := newTestOps(t, filepath.Join(dir, "no-such-msm"), []string{"main"})
	got := o.Handle("status", "main", nil)
	if got["ok"] != false || got["error"] == nil || got["error"] == "" {
		t.Fatalf("执行失败应回错误: %v", got)
	}
}
