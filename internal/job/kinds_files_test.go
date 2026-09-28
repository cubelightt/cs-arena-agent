// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package job

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arena/agent/internal/config"
)

// backup:old 必须同时扫 `<archive_dir>/backup` 与 `~/backup`:
// 只认前者时,主机(archive_dir=~/arena-data)会把真正的备份目录(~/backup)整个漏掉 —— 主机环境验证的静默漏项。
func TestHostCleanupBackupOldScansBothRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	archive := t.TempDir()
	old := time.Now().Add(-90 * 24 * time.Hour)
	var wantOld []string
	for _, root := range []string{filepath.Join(home, "backup"), filepath.Join(archive, "backup")} {
		for _, name := range []string{"old-one", "old-two"} {
			p := filepath.Join(root, name)
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
			wantOld = append(wantOld, p)
		}
		// 每个根各留一个"最新"的:永远不删
		fresh := filepath.Join(root, "fresh")
		if err := os.MkdirAll(fresh, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.ArchiveDir = archive
	got, err := cleanupPattern(&Ctx{Cfg: cfg}, "backup:old", 30, true, "")
	if err != nil {
		t.Fatalf("backup:old 报错: %v", err)
	}
	have := map[string]bool{}
	for _, it := range got {
		have[fmt.Sprint(it["path"])] = true
	}
	for _, p := range wantOld {
		if !have[p] {
			t.Fatalf("两个根里的旧备份都应被列出,缺 %s(得到 %v)", p, got)
		}
	}
	for p := range have {
		if filepath.Base(p) == "fresh" {
			t.Fatalf("每个根的最新一份必须保留,却列出了 %s", p)
		}
	}
	if len(got) != 4 {
		t.Fatalf("两个根各 2 个旧目录都应列出(每根的最新一份保留),共应 4 个,得到 %v", got)
	}
}

func TestHostCleanupBackupOldReportsBothPathsWhenMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Defaults()
	cfg.ArchiveDir = filepath.Join(t.TempDir(), "archive")
	_, err := cleanupPattern(&Ctx{Cfg: cfg}, "backup:old", 30, true, "")
	if err == nil {
		t.Fatal("两个根都不存在时应报错")
	}
	if !strings.Contains(err.Error(), filepath.Join(home, "backup")) {
		t.Fatalf("报错应列出尝试过的两个路径,得到: %v", err)
	}
}

// bridge:old:桥自身目录的冗余件清理(2026-09-22 用户定档「路线 B」)。
// 规则:① cs.bak-* / bridge.py.bak-* 每类只留最新一份;② 桥日志超 8 MiB 才轮转(gzip + 截断);
// ③ __pycache__ 删;④ jobs/*.log 保留最新 20 份,index.json/*.cancel/*.lock 绝不碰。
func TestHostCleanupBridgeOld(t *testing.T) {
	bridge := t.TempDir()
	jobs := filepath.Join(bridge, "jobs")
	if err := os.MkdirAll(jobs, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	mk := func(dir, name string, size int, mod time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Repeat("x", size)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// ① 备份:cs 3 份、bridge.py 2 份 → 各留最新一份
	csOld1 := mk(bridge, "cs.bak-20260921140834", 16, old)
	csOld2 := mk(bridge, "cs.bak-20260921160857", 16, old)
	csNew := mk(bridge, "cs.bak-20260922112837", 16, time.Now().Add(-time.Hour))
	pyOld := mk(bridge, "bridge.py.bak-20260918160405", 16, old)
	pyNew := mk(bridge, "bridge.py.bak-20260920204822", 16, time.Now().Add(-2*time.Hour))
	// 保留项:当前二进制与旧桥本体(不该被列)
	cur := mk(bridge, "cs", 16, time.Now())
	py := mk(bridge, "bridge.py", 16, time.Now())
	// ② 日志:bridge.log 超阈值 → 轮转;cs-agent.log 未超 → 不动
	logBig := mk(bridge, "bridge.log", 9<<20, time.Now().Add(-time.Hour))
	logSmall := mk(bridge, "cs-agent.log", 1<<20, time.Now().Add(-time.Hour))
	// ③ __pycache__
	if err := os.MkdirAll(filepath.Join(bridge, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	// ④ jobs:22 个旧日志 + index.json + cancel/lock(后三者不该被列)
	var jobOld []string
	for i := 0; i < 22; i++ {
		jobOld = append(jobOld, mk(jobs, fmt.Sprintf("%d.log", 1000000+i), 8, old.Add(time.Duration(i)*time.Minute)))
	}
	idx := mk(jobs, "index.json", 8, old)
	cancel := mk(jobs, "1000001.cancel", 8, old)
	lock := mk(jobs, "g1.lock", 8, old)

	got, err := cleanupPattern(&Ctx{Cfg: config.Defaults()}, "bridge:old", 30, true, bridge)
	if err != nil {
		t.Fatalf("bridge:old 报错: %v", err)
	}
	have := map[string]bool{}
	for _, it := range got {
		have[fmt.Sprint(it["path"])] = true
	}
	for _, p := range []string{csOld1, csOld2, pyOld, logBig} {
		if !have[p] {
			t.Fatalf("应被列出却缺失: %s(得到 %v)", p, got)
		}
	}
	// 22 个日志里最新 20 份保留 → 只应列出最旧的 2 份
	pruned := 0
	for _, p := range jobOld {
		if have[p] {
			pruned++
		}
	}
	if pruned != 2 {
		t.Fatalf("jobs 日志应只清理最旧的 2 份,实际 %d 份(得到 %v)", pruned, got)
	}
	for _, p := range []string{csNew, pyNew, cur, py, logSmall, idx, cancel, lock} {
		if have[p] {
			t.Fatalf("不该被列出: %s", p)
		}
	}
	if !have[filepath.Join(bridge, "__pycache__")] {
		t.Fatalf("__pycache__ 应被列出(得到 %v)", got)
	}

	// 真跑:gzip 轮转 + 截断,备份/日志/缓存消失,保留项仍在
	if _, err := cleanupPattern(&Ctx{Cfg: config.Defaults()}, "bridge:old", 30, false, bridge); err != nil {
		t.Fatalf("bridge:old 执行报错: %v", err)
	}
	if fi, err := os.Stat(logBig); err != nil || fi.Size() != 0 {
		t.Fatalf("bridge.log 应被截断为 0 字节(内容进 .gz),得到 %v/%v", fi, err)
	}
	gs, _ := filepath.Glob(logBig + ".*.gz")
	if len(gs) != 1 {
		t.Fatalf("应生成 1 份 .gz,得到 %v", gs)
	}
	for _, p := range []string{csOld1, pyOld} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("旧备份应已删除: %s", p)
		}
	}
	for _, p := range []string{csNew, pyNew, cur, py, logSmall, idx, cancel, lock} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("保留项不该被动: %s(%v)", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(bridge, "__pycache__")); !os.IsNotExist(err) {
		t.Fatal("__pycache__ 应已删除")
	}
}

// 同一目录里的"新鲜"任务日志(5 分钟内)永不删,避免清掉正在写的任务。
func TestHostCleanupBridgeOldSkipsFreshJobLogs(t *testing.T) {
	bridge := t.TempDir()
	jobs := filepath.Join(bridge, "jobs")
	if err := os.MkdirAll(jobs, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	var fresh string
	for i := 0; i < 25; i++ {
		p := filepath.Join(jobs, fmt.Sprintf("%d.log", 1000100+i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mod := old.Add(time.Duration(i) * time.Minute)
		if i == 24 {
			mod = time.Now().Add(-30 * time.Second) // 最新一份且刚写过
			fresh = p
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	got, err := cleanupPattern(&Ctx{Cfg: config.Defaults()}, "bridge:old", 30, true, bridge)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range got {
		if fmt.Sprint(it["path"]) == fresh {
			t.Fatalf("5 分钟内还在写的任务日志不该被清理: %s", fresh)
		}
	}
}
