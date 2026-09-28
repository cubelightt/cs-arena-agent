// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package msm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunStreamSuccessfulParentWithInheritedOutputPipe(t *testing.T) {
	dir := t.TempDir()
	msmPath := filepath.Join(dir, "msm")
	// 后台进程继承 stdout/stderr,模拟 msm start/update 拉起的子进程。
	// 主进程立即成功退出,Go 的 5 秒 WaitDelay 会先于 sleep 结束。
	if err := os.WriteFile(msmPath, []byte("#!/bin/sh\nsleep 8 &\necho update-complete\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(func() RunnerConfig { return RunnerConfig{MSM: msmPath, MSMDir: dir, TimeoutS: 20} })
	var lines []string
	res, err := r.RunStream(context.Background(), "", "update", nil, StreamOpts{
		OnLine: func(line string) { lines = append(lines, line) },
	})
	if err != nil || res == nil || res.Returncode != 0 {
		t.Fatalf("主进程已成功退出,不应因继承管道判失败: result=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Stdout, "update-complete") {
		t.Fatalf("丢失主进程输出: %q", res.Stdout)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "输出管道未在 5 秒内关闭") {
		t.Fatalf("任务日志缺少管道提示: %q", lines)
	}
}

func TestRunStreamNonzeroExitStillFails(t *testing.T) {
	dir := t.TempDir()
	msmPath := filepath.Join(dir, "msm")
	if err := os.WriteFile(msmPath, []byte("#!/bin/sh\necho update-failed\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(func() RunnerConfig { return RunnerConfig{MSM: msmPath, MSMDir: dir, TimeoutS: 20} })
	res, err := r.RunStream(context.Background(), "", "update", nil, StreamOpts{})
	if err != nil || res == nil || res.Returncode != 7 {
		t.Fatalf("非零退出码必须保留: result=%+v err=%v", res, err)
	}
}

// 顺序与大小写不敏感来自 v1 parse_state,但 **"not running" 的判断顺序已修复**
// (2026-09-20 用户批准):v1 先匹配 RUNNING,导致 "not running" 判成 RUNNING;v2 改为先判 "not running"。
func TestParseStateOrder(t *testing.T) {
	cases := map[string]string{
		"BOOTING":             "BOOTING",
		"Server is RUNNING":   "RUNNING",
		"server not running":  "STOPPED", // ← v2 修复(v1 会误判成 RUNNING)
		"NOT RUNNING":         "STOPPED",
		"STOPPED":             "STOPPED",
		"totally different":   "UNKNOWN",
		"":                    "UNKNOWN",
		"booting up":          "BOOTING",
		"Running (pid 123)":   "RUNNING",
		"service is stopped.": "STOPPED",
	}
	for in, want := range cases {
		if got := ParseState(in); got != want {
			t.Fatalf("ParseState(%q) = %q, want %q", in, got, want)
		}
	}
	// BOOTING 优先于 RUNNING(msm 启动过程中两种字样可能同时出现)
	if got := ParseState("RUNNING? no: BOOTING"); got != "BOOTING" {
		t.Fatalf("BOOTING 应优先: %q", got)
	}
}

// 文案必须与 v1 逐字一致(后端 smoke 断言过这些字符串)。
func TestSanitizeEnv(t *testing.T) {
	env, errStr := SanitizeEnv(map[string]any{"MAXPLAYERS": "16", "FOO": 3})
	if errStr != "" || env["MAXPLAYERS"] != "16" || env["FOO"] != "3" {
		t.Fatalf("合法 env 解析失败: %v %q", env, errStr)
	}
	if env, errStr := SanitizeEnv(nil); errStr != "" || len(env) != 0 {
		t.Fatalf("nil env 应为空: %v %q", env, errStr)
	}
	if _, errStr := SanitizeEnv(map[string]any{"lower": "1"}); errStr != "bad env key: lower" {
		t.Fatalf("小写键应拒绝: %q", errStr)
	}
	if _, errStr := SanitizeEnv(map[string]any{"OK": "a b"}); errStr != "bad env value for OK (allowed: A-Za-z0-9_.- up to 32 chars)" {
		t.Fatalf("含空格值应拒绝: %q", errStr)
	}
	// 值会进 msm 生成的 server-start.sh,注入类字符一律拒绝
	for _, bad := range []string{"1;2", "a\"b", "$(x)", "`id`", "a|b"} {
		if _, errStr := SanitizeEnv(map[string]any{"OK": bad}); errStr == "" {
			t.Fatalf("危险值 %q 应被拒绝", bad)
		}
	}
	tooMany := map[string]any{}
	for i := 0; i < MaxEnvKeys+1; i++ {
		tooMany["K"+string(rune('A'+i))] = "1"
	}
	if _, errStr := SanitizeEnv(tooMany); errStr != "env must be an object with at most 8 keys" {
		t.Fatalf("超键数应拒绝: %q", errStr)
	}
}
