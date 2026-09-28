// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package job

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"arena/agent/internal/config"
	"arena/agent/internal/msm"
	"arena/agent/internal/protocol"
	"arena/agent/internal/registry"
)

// mkManager 建一个只依赖临时目录的 Manager(不碰 msm / 真实路径)。
func mkManager(t *testing.T, push func(any) error) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.MSMDir = dir
	cfg.DemoDir = filepath.Join(dir, "demos")
	cfg.ArchiveDir = filepath.Join(dir, "archive")
	m := New(Options{
		Dir:     filepath.Join(dir, "jobs"),
		Cfg:     func() *config.Config { return cfg },
		Names:   func() []string { return []string{"main", "match1"} },
		GroupID: func() string { return "g1" },
		Push:    push,
	})
	return m, dir
}

// waitStatus 等任务到某状态(超时 3s)。
func waitStatus(t *testing.T, m *Manager, id int64, want ...string) *Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		j := m.Get(id)
		if j != nil {
			for _, w := range want {
				if j.Status == w {
					return j
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	j := m.Get(id)
	t.Fatalf("任务 %d 未在 3s 内到达 %v(当前 %v)", id, want, j)
	return nil
}

func TestJobRunsStepsAndWritesLog(t *testing.T) {
	m, _ := mkManager(t, nil)
	var mu sync.Mutex
	var order []string
	plan := Plan{Steps: []Step{
		{Name: "一", Run: func(c *Ctx) error { mu.Lock(); order = append(order, "一"); mu.Unlock(); c.Logf("hi"); return nil }},
		{Name: "二", Run: func(c *Ctx) error { mu.Lock(); order = append(order, "二"); mu.Unlock(); return nil }},
	}}
	j, err := m.startWithPlan(Spec{Kind: KindGameUpdate, Params: map[string]any{}}, plan)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	done := waitStatus(t, m, j.ID, protocol.JobDone)
	if done.Progress != 100 {
		t.Fatalf("终态进度应为 100,得到 %d", done.Progress)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "一" {
		t.Fatalf("步骤顺序不对: %v", order)
	}
	lines, _, err := m.LogSince(j.ID, 0)
	if err != nil {
		t.Fatalf("读日志: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "[cs] hi") || !strings.Contains(joined, "任务结束:done") {
		t.Fatalf("任务日志内容不对:\n%s", joined)
	}
	// index.json 落盘且可重新载入
	if _, err := os.Stat(filepath.Join(m.opt.Dir, "index.json")); err != nil {
		t.Fatalf("index.json 未落盘: %v", err)
	}
	m2 := New(m.opt)
	if m2.Get(j.ID) == nil {
		t.Fatal("新 Manager 未载入历史任务")
	}
}

func TestJobGroupLockExclusive(t *testing.T) {
	m, _ := mkManager(t, nil)
	release := make(chan struct{})
	plan := Plan{Steps: []Step{{Name: "阻塞", Run: func(c *Ctx) error { <-release; return nil }}}}

	j1, err := m.startWithPlan(Spec{Kind: KindGameUpdate}, plan)
	if err != nil {
		t.Fatalf("第一个任务应能启动: %v", err)
	}
	waitStatus(t, m, j1.ID, protocol.JobRunning)

	if _, err := m.startWithPlan(Spec{Kind: KindGameUpdate}, Plan{Steps: []Step{{Name: "x", Run: func(c *Ctx) error { return nil }}}}); err == nil {
		t.Fatal("同组第二个任务应被拒绝(按组独占)")
	} else if !strings.Contains(err.Error(), "已有进行中任务") {
		t.Fatalf("拒绝文案不对: %v", err)
	}
	close(release)
	waitStatus(t, m, j1.ID, protocol.JobDone)
	// 释放后可再起
	j3, err := m.startWithPlan(Spec{Kind: KindGameUpdate}, Plan{Steps: []Step{{Name: "y", Run: func(c *Ctx) error { return nil }}}})
	if err != nil {
		t.Fatalf("前一个任务结束后应能再起: %v", err)
	}
	waitStatus(t, m, j3.ID, protocol.JobDone)
}

func TestCancelAtStepBoundary(t *testing.T) {
	m, _ := mkManager(t, nil)
	started := make(chan struct{})
	rec := false
	plan := Plan{
		Steps: []Step{
			{Name: "一", Run: func(c *Ctx) error { close(started); time.Sleep(50 * time.Millisecond); return nil }},
			{Name: "二", Run: func(c *Ctx) error { t.Error("A 取消后不应进入第二步"); return nil }},
		},
		Recover: func(c *Ctx) { rec = true },
	}
	j, err := m.startWithPlan(Spec{Kind: KindGameUpdate}, plan)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	<-started
	if _, err := m.Cancel(j.ID, false); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitStatus(t, m, j.ID, protocol.JobCancelled)
	time.Sleep(50 * time.Millisecond)
	if !rec {
		t.Fatal("取消后应执行恢复步骤")
	}
}

func TestForceCancelKillsRunningStep(t *testing.T) {
	m, _ := mkManager(t, nil)
	plan := Plan{Steps: []Step{{Name: "长步骤", Run: func(c *Ctx) error {
		select {
		case <-c.ctx().Done():
			return c.ctx().Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}}}}
	j, err := m.startWithPlan(Spec{Kind: KindGameUpdate}, plan)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitStatus(t, m, j.ID, protocol.JobRunning)
	time.Sleep(30 * time.Millisecond)
	if _, err := m.Cancel(j.ID, true); err != nil {
		t.Fatalf("force cancel: %v", err)
	}
	waitStatus(t, m, j.ID, protocol.JobCancelled)
	// 已结束的任务再取消 → 报错
	if _, err := m.Cancel(j.ID, false); err == nil {
		t.Fatal("已结束的任务再次取消应报错")
	}
}

// waitConsumed 等执行进程的轮询协程把取消请求消费成内存标志(500ms 一拍)。
func waitConsumed(t *testing.T, m *Manager, id int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m.cancelRequested(id) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("执行进程未在期限内消费跨进程取消请求")
}

// 跨进程取消 A(步骤边界):执行方是 m1,请求方是 m2(同目录 = 另一个进程)。
// 内存标志跨不了进程 —— 请求必须落 <jobs>/<id>.cancel,由执行方轮询消费。
func TestCancelAcrossProcessesAtStepBoundary(t *testing.T) {
	m1, root := mkManager(t, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	rec := false
	plan := Plan{
		Steps: []Step{
			{Name: "一", Run: func(c *Ctx) error { close(started); <-release; return nil }},
			{Name: "二", Run: func(c *Ctx) error { t.Error("跨进程 A 取消后不应进入第二步"); return nil }},
		},
		Recover: func(c *Ctx) { rec = true },
	}
	j, err := m1.startWithPlan(Spec{Kind: KindGameUpdate}, plan)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	<-started

	reqPath := filepath.Join(root, "jobs", fmt.Sprintf("%d.cancel", j.ID))
	m2 := New(m1.opt) // 同目录的第二个 Manager = 另一个进程
	out, err := m2.Cancel(j.ID, false)
	if err != nil {
		t.Fatalf("跨进程 cancel: %v", err)
	}
	if out["requested"] != true || out["status"] != protocol.JobCancelling {
		t.Fatalf("非执行方取消应落盘并回 requested/status,得到 %v", out)
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("取消请求文件未落盘: %v", err)
	}
	// 执行方仍按 running 跑(m2 不写 index.json,执行方的内存状态不被污染)
	if cur := m1.Get(j.ID); cur == nil || cur.Status != protocol.JobRunning {
		t.Fatalf("执行方状态应仍为 running,得到 %v", cur)
	}
	waitConsumed(t, m1, j.ID, 3*time.Second)
	if cur := m1.Get(j.ID); cur == nil || cur.Status != protocol.JobCancelling {
		t.Fatalf("消费请求后执行方应转 cancelling,得到 %v", cur)
	}
	close(release) // 放行当前步骤 → 步骤边界上按 A 语义停止
	waitStatus(t, m1, j.ID, protocol.JobCancelled)
	if !rec {
		t.Fatal("跨进程取消后应执行恢复步骤")
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Fatalf("任务终态后请求文件应被清理,stat err=%v", err)
	}
}

// 跨进程取消 B(force):执行方必须被**另一个进程**的请求 kill 掉进行中的步骤。
func TestForceCancelAcrossProcesses(t *testing.T) {
	m1, _ := mkManager(t, nil)
	plan := Plan{Steps: []Step{{Name: "长步骤", Run: func(c *Ctx) error {
		select {
		case <-c.ctx().Done():
			return c.ctx().Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}}}}
	j, err := m1.startWithPlan(Spec{Kind: KindGameUpdate}, plan)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitStatus(t, m1, j.ID, protocol.JobRunning)

	m2 := New(m1.opt)
	if out, err := m2.Cancel(j.ID, true); err != nil || out["requested"] != true {
		t.Fatalf("跨进程 force cancel: out=%v err=%v", out, err)
	}
	// 轮询消费 → 直接 cancel() 掉进行中的步骤(不必等 5s 的自然结束)
	waitStatus(t, m1, j.ID, protocol.JobCancelled)
}

// 请求文件必须在**终态可见之前**清掉:CLI 前台跟随的主协程一看到终态就可能立刻退出进程,
// 清理若排在终态之后会随进程退出一起丢掉(主机环境验证:jobs/<id>.cancel 残留)。
// 用 push 钩子在终态帧发出的那一刻断言文件已经不在。
func TestCancelRequestFileClearedBeforeTerminalPush(t *testing.T) {
	var mu sync.Mutex
	var fileExistedAtTerminal *bool
	var reqPath string
	m, root := mkManager(t, func(v any) error {
		frame, ok := v.(protocol.JobPush)
		if !ok || !isTerminal(frame.Status) {
			return nil
		}
		_, err := os.Stat(reqPath)
		existed := err == nil
		mu.Lock()
		fileExistedAtTerminal = &existed
		mu.Unlock()
		return nil
	})
	release := make(chan struct{})
	plan := Plan{Steps: []Step{{Name: "一", Run: func(c *Ctx) error { <-release; return nil }}}}
	j, err := m.startWithPlan(Spec{Kind: KindGameUpdate}, plan)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitStatus(t, m, j.ID, protocol.JobRunning)
	reqPath = filepath.Join(root, "jobs", fmt.Sprintf("%d.cancel", j.ID))

	m2 := New(m.opt) // 同目录的第二个 Manager = 另一个进程
	if _, err := m2.Cancel(j.ID, false); err != nil {
		t.Fatalf("跨进程 cancel: %v", err)
	}
	waitConsumed(t, m, j.ID, 3*time.Second)
	close(release)
	waitStatus(t, m, j.ID, protocol.JobCancelled)

	mu.Lock()
	got := fileExistedAtTerminal
	mu.Unlock()
	if got == nil {
		t.Fatal("没有观察到终态推送帧")
	}
	if *got {
		t.Fatal("终态帧发出时取消请求文件仍在(CLI 一退出就会残留)")
	}
}

func TestJobReportPendingAndMarkReported(t *testing.T) {
	var mu sync.Mutex
	var frames []any
	m, _ := mkManager(t, func(v any) error {
		mu.Lock()
		frames = append(frames, v)
		mu.Unlock()
		return nil
	})
	j, err := m.startWithPlan(Spec{Kind: KindGameUpdate, Origin: OriginCLI}, Plan{Steps: []Step{{Name: "一", Run: func(c *Ctx) error { return nil }}}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitStatus(t, m, j.ID, protocol.JobDone)
	mu.Lock()
	frames = nil
	mu.Unlock()

	if n := m.ReportPending(); n == 0 {
		t.Fatal("终态未上报的任务应被 ReportPending 收集")
	}
	mu.Lock()
	got := len(frames)
	mu.Unlock()
	if got == 0 {
		t.Fatal("ReportPending 没有发出帧")
	}
	// 已上报的终态不再重复上报
	mu.Lock()
	frames = nil
	mu.Unlock()
	if n := m.ReportPending(); n != 0 {
		t.Fatalf("已上报的终态任务不应重复上报(得到 %d)", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 0 {
		t.Fatalf("已上报后不应再发帧: %+v", frames)
	}
}

func TestReportFrameShape(t *testing.T) {
	j := &Job{ID: 1000001, Kind: KindInstanceCreate, GroupID: "g1", Origin: OriginCLI, Status: protocol.JobDone,
		Step: "回读状态与版本", StepIndex: 6, StepTotal: 7, Progress: 100, StartedAt: 1, FinishedAt: 2, Error: "x",
		CLIUser: "aaa", Instance: "arena1", Result: map[string]any{"port": 27119, "idx": 5}}
	raw, err := json.Marshal(JobReportFrame(j))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	for _, want := range []string{`"type":"job_report"`, `"jobId":1000001`, `"groupId":"g1"`, `"cliUser":"aaa"`,
		`"stepTotal":7`, `"instance":"arena1"`, `"port":27119`} {
		if !strings.Contains(s, want) {
			t.Fatalf("job_report 缺少字段 %s:\n%s", want, s)
		}
	}
	// 进行中的帧不带 result(避免每轮重复大对象);终态帧才带
	running := JobReportFrame(&Job{ID: 2, Kind: KindInstanceCreate, Status: protocol.JobRunning, Instance: "arena1",
		Result: map[string]any{"port": 27119}})
	if running.Job.Result != nil {
		t.Fatalf("进行中的 job_report 不应带 result: %+v", running.Job.Result)
	}
	pushRaw, _ := json.Marshal(protocol.JobPush{Type: protocol.TypePush, Kind: protocol.KindJob, JobID: 7, Status: protocol.JobRunning, Line: "x"})
	ps := string(pushRaw)
	for _, want := range []string{`"kind":"job"`, `"line":"x"`} {
		if !strings.Contains(ps, want) {
			t.Fatalf("job push 缺少字段 %s:\n%s", want, ps)
		}
	}
}

func TestLoadMarksInterruptedJobFailed(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.MSMDir = dir
	// 造一个"上次进程留下的 running 任务"
	idx := indexFile{NextID: 42, Jobs: []*Job{{ID: 41, Kind: KindGameUpdate, GroupID: "g1", Status: protocol.JobRunning, StartedAt: 1}}}
	raw, _ := json.Marshal(idx)
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jobs", "index.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(Options{Dir: filepath.Join(dir, "jobs"), Cfg: func() *config.Config { return cfg }})
	j := m.Get(41)
	if j == nil {
		t.Fatal("历史任务未载入")
	}
	if j.Status != protocol.JobFailed || !strings.Contains(j.Error, "重启") {
		t.Fatalf("中断任务应标记 failed,得到 %s/%s", j.Status, j.Error)
	}
}

// 另一个进程仍在跑的任务不得被 load() 误标 failed:PID 存活 = 状态归执行方写。
// (旧实现只看状态,导致 `cs jobs` 这类只读命令会把并发运行的长任务写坏。)
func TestLoadKeepsJobOfLiveProcessRunning(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.MSMDir = dir
	idx := indexFile{NextID: 1000001, Jobs: []*Job{
		{ID: 1000000, Kind: KindInstanceCreate, GroupID: "g1", Status: protocol.JobRunning, StartedAt: 1, PID: os.Getpid()},
	}}
	raw, _ := json.Marshal(idx)
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jobs", "index.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(Options{Dir: filepath.Join(dir, "jobs"), Cfg: func() *config.Config { return cfg }})
	j := m.Get(1000000)
	if j == nil {
		t.Fatal("历史任务未载入")
	}
	if j.Status != protocol.JobRunning {
		t.Fatalf("执行进程仍在(PID %d)的任务不应被标 failed,得到 %s/%s", os.Getpid(), j.Status, j.Error)
	}
	// 盘上也不得被改写
	after, err := os.ReadFile(filepath.Join(dir, "jobs", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f indexFile
	if err := json.Unmarshal(after, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Jobs) != 1 || f.Jobs[0].Status != protocol.JobRunning {
		t.Fatalf("index.json 被 load() 改写: %+v", f.Jobs)
	}
}

func TestLogSinceOnlyCompleteLines(t *testing.T) {
	m, _ := mkManager(t, nil)
	j, err := m.startWithPlan(Spec{Kind: KindHostCleanup, Params: map[string]any{"patterns": []any{"backup:old"}}},
		Plan{Steps: []Step{{Name: "一", Run: func(c *Ctx) error { return nil }}}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitStatus(t, m, j.ID, protocol.JobDone)
	// 追加半行:不应被读出,offset 也不推进
	f, err := os.OpenFile(m.LogPath(j.ID), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, "半行没有换行")
	f.Close()
	size := m.LogSize(j.ID)
	lines, next, err := m.LogSince(j.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "\n") == "半行没有换行" {
		t.Fatal("半行不应被读出")
	}
	if next >= size {
		t.Fatalf("offset 不应推进过半行(next=%d size=%d)", next, size)
	}
}

func TestHostCleanupRequiresConfirmWhenNotDryRun(t *testing.T) {
	m, dir := mkManager(t, nil)
	// 默认 dryRun=true → 可启动(dry-run 不需要确认)
	j, err := m.startWithPlan(Spec{Kind: KindHostCleanup, Params: map[string]any{"patterns": []any{"backup:old"}}},
		Plan{Steps: []Step{{Name: "一", Run: func(c *Ctx) error { return nil }}}})
	if err != nil {
		t.Fatalf("dry-run 应可启动: %v", err)
	}
	waitStatus(t, m, j.ID, protocol.JobDone)

	// 真删但没有 confirm → 参数校验必须拒绝(在占锁之前)
	if _, err := m.Start(Spec{Kind: KindHostCleanup, Params: map[string]any{"patterns": []any{"backup:old"}, "dryRun": false}}); err == nil {
		t.Fatal("非 dry-run 清理应要求 confirm=CLEAN")
	}
	// 真删 + confirm → 启动成功,并真的删掉过期备份(最新一份永远保留)
	backupRootDir := filepath.Join(dir, "archive", "backup")
	past := time.Now().AddDate(0, 0, -90)
	older := time.Now().AddDate(0, 0, -120)
	newest := filepath.Join(backupRootDir, "old-newest")
	mid := filepath.Join(backupRootDir, "old-mid")
	ancient := filepath.Join(backupRootDir, "old-ancient")
	for _, d := range []string{newest, mid, ancient} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Chtimes(newest, past, past)
	_ = os.Chtimes(mid, older, older)
	_ = os.Chtimes(ancient, older, older)
	jj, err := m.Start(Spec{Kind: KindHostCleanup, Params: map[string]any{
		"patterns": []any{"backup:old"}, "dryRun": false, "confirm": "CLEAN", "maxAgeDays": 30,
	}})
	if err != nil {
		t.Fatalf("带确认的清理应能启动: %v", err)
	}
	waitStatus(t, m, jj.ID, protocol.JobDone)
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("最新一份备份必须保留: %v", err)
	}
	for _, d := range []string{mid, ancient} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("过期备份应被删除(%s): %v", filepath.Base(d), err)
		}
	}
}

func TestGameUpdateRequiresConfirm(t *testing.T) {
	m, _ := mkManager(t, nil)
	if _, err := m.Start(Spec{Kind: KindGameUpdate, Params: map[string]any{}}); err == nil {
		t.Fatal("游戏更新必须要求 confirm=UPDATE")
	}
	if _, err := m.Start(Spec{Kind: KindGameUpdate, Confirm: "update"}); err == nil {
		t.Fatal("确认串大小写敏感")
	}
}

// 回归:CLI 进程内传的是 []string(平台经 JSON 下发才是 []any),两者都必须被接受。
func TestStringSliceParamsFromCLI(t *testing.T) {
	dir := t.TempDir()
	layoutRoot := filepath.Join(dir, "host")
	msmDir := filepath.Join(layoutRoot, "cs2-multiserver-new")
	msmD := filepath.Join(layoutRoot, "msm.d", "cs2")
	// 来源实例的 addons 目录(够走完 plugin_sync 的参数校验与拷贝)
	addons := filepath.Join(msmD, "inst-main", "game", "csgo", "addons")
	if err := os.MkdirAll(addons, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(addons, "p.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 目标实例的 csgo 目录
	if err := os.MkdirAll(filepath.Join(msmD, "inst-match1", "game", "csgo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.MSMDir = msmDir
	m := New(Options{
		Dir:     filepath.Join(dir, "jobs"),
		Cfg:     func() *config.Config { return cfg },
		Paths:   msm.NewPaths(msmDir),
		Names:   func() []string { return []string{"main", "match1"} },
		GroupID: func() string { return "g1" },
	})
	j, err := m.Start(Spec{Kind: KindPluginSync, Params: map[string]any{
		"from": "main", "targets": []string{"match1"},
	}})
	if err != nil {
		t.Fatalf("[]string 目标应被接受(旧实现只认 []any,会误报 缺少 targets): %v", err)
	}
	done := waitStatus(t, m, j.ID, protocol.JobDone, protocol.JobFailed)
	if done.Status != protocol.JobDone {
		t.Fatalf("同步应成功(两个目录都在位),得到 %s: %s", done.Status, done.Error)
	}
	// 目标侧真的收到文件
	if _, err := os.Stat(filepath.Join(msmD, "inst-match1", "game", "csgo", "addons", "p.txt")); err != nil {
		t.Fatalf("目标实例未收到插件文件: %v", err)
	}
}

// 跨进程收敛:CLI(另一个进程)起的任务,守护经 Reload + ReportPending 上报给平台。
func TestReloadPicksUpCliJobsFromDisk(t *testing.T) {
	m, dir := mkManager(t, nil)
	jdir := filepath.Join(dir, "jobs")
	cfg := config.Defaults()
	cfg.MSMDir = dir

	// 模拟"另一个进程"(CLI):独立 Manager,同一份 jobs 目录,跑一个任务
	cli := New(Options{Dir: jdir, Cfg: func() *config.Config { return cfg }, GroupID: func() string { return "g1" }})
	cj, err := cli.startWithPlan(Spec{Kind: KindGameUpdate, Origin: OriginCLI, CLIUser: "aaa"},
		Plan{Steps: []Step{{Name: "一", Run: func(c *Ctx) error { return nil }}}})
	if err != nil {
		t.Fatalf("cli job: %v", err)
	}
	waitStatus(t, cli, cj.ID, protocol.JobDone)

	// 守护:内存里没有该任务 → Reload 后才能上报
	var mu sync.Mutex
	var frames []any
	m.SetPush(func(v any) error {
		mu.Lock()
		frames = append(frames, v)
		mu.Unlock()
		return nil
	})
	if n := m.ReportPending(); n == 0 {
		t.Fatal("守护应经 Reload 发现 CLI 起的任务并上报")
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, f := range frames {
		if r, ok := f.(protocol.JobReport); ok && r.Job.JobID == cj.ID && r.Job.Origin == OriginCLI && r.Job.CLIUser == "aaa" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未上报 CLI 任务: %+v", frames)
	}
}

// 回归:CLI 的**长任务**(守护先看到它的 running 快照、之后 CLI 才写盘终态)必须收敛到终态。
// 旧实现 Reload 见到"内存里是 running"就跳过合并 → 平台永远停在 running、该组一直"维护中"
// (真机触发条件:任务时长 > 守护轮询间隔,如 cs host cleanup / cs new / cs update)。
func TestReloadAdoptsCliTerminalState(t *testing.T) {
	m, dir := mkManager(t, nil)
	jdir := filepath.Join(dir, "jobs")
	cfg := config.Defaults()
	cfg.MSMDir = dir

	// ① 另一个进程(CLI)起一个长任务:步骤阻塞,直到测试放行
	cli := New(Options{Dir: jdir, Cfg: func() *config.Config { return cfg }, GroupID: func() string { return "g1" }})
	release := make(chan struct{})
	cj, err := cli.startWithPlan(Spec{Kind: KindGameUpdate, Origin: OriginCLI, CLIUser: "aaa"},
		Plan{Steps: []Step{{Name: "长步骤", Run: func(c *Ctx) error { <-release; return nil }}}})
	if err != nil {
		t.Fatalf("cli job: %v", err)
	}
	waitStatus(t, cli, cj.ID, protocol.JobRunning)

	// ② 守护此刻 Reload:看到的是 running(旧实现就是在这一步把它钉死的)
	if n := m.Reload(); n == 0 {
		t.Fatal("守护应经 Reload 发现 CLI 起的任务")
	}
	if got := m.Get(cj.ID); got == nil || got.Status != protocol.JobRunning {
		t.Fatalf("守护应先看到 running,得到 %+v", got)
	}

	// ③ 放行 → CLI 任务跑完,盘上变 done
	close(release)
	waitStatus(t, cli, cj.ID, protocol.JobDone)

	// ④ 守护再 Reload:必须采纳盘上终态
	m.Reload()
	if got := m.Get(cj.ID); got == nil || got.Status != protocol.JobDone {
		t.Fatalf("守护必须采纳 CLI 任务的终态(期望 done;旧实现停在 running): %+v", got)
	}

	// ⑤ 收敛上报里也应是 done(平台据此清维护模式)
	var mu sync.Mutex
	status := map[int64]string{}
	m.SetPush(func(v any) error {
		if r, ok := v.(protocol.JobReport); ok {
			mu.Lock()
			status[r.Job.JobID] = r.Job.Status
			mu.Unlock()
		}
		return nil
	})
	m.ReportPending()
	mu.Lock()
	defer mu.Unlock()
	if status[cj.ID] != protocol.JobDone {
		t.Fatalf("上报状态应为 done,得到 %q", status[cj.ID])
	}
}

// 回归:插件树里的**符号链接**必须按链接复制(不跟随)。真机形态(2026-09-21 实测):
// addons/counterstrikesharp/logs -> ~/arena-data/cssharp-logs/<实例>(日志外移),
// 旧实现跟随链接把目录当文件拷 → "copy_file_range: is a directory" → cs new 第 4 步失败并回滚。
// 指向"来源实例专属目录"的链接还要重指向新实例,否则新实例与来源共用一份日志。
func TestSyncTreeCopiesSymlinks(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "inst-main", "game", "csgo", "addons")
	dst := filepath.Join(root, "inst-arena1", "game", "csgo", "addons")
	if err := os.MkdirAll(filepath.Join(src, "counterstrikesharp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "counterstrikesharp", "plugin.dll"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(root, "arena-data", "cssharp-logs", "main")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(logDir, filepath.Join(src, "counterstrikesharp", "logs")); err != nil {
		t.Fatal(err)
	}

	files, _, _, err := syncTree(src, dst, false, false, "main", "arena1", nil)
	if err != nil {
		t.Fatalf("符号链接必须按链接复制(旧实现跟随链接报 copy_file_range: is a directory): %v", err)
	}
	if files != 2 {
		t.Fatalf("应拷贝 1 普通文件 + 1 符号链接,得到 %d", files)
	}
	got, err := os.Readlink(filepath.Join(dst, "counterstrikesharp", "logs"))
	if err != nil {
		t.Fatalf("符号链接未被复制: %v", err)
	}
	want := filepath.Join(root, "arena-data", "cssharp-logs", "arena1")
	if got != want {
		t.Fatalf("指向来源实例的链接必须重指向新实例(期望 %s,得到 %s)", want, got)
	}
	if fi, err := os.Stat(filepath.Join(dst, "counterstrikesharp", "logs")); err != nil || !fi.IsDir() {
		t.Fatalf("重指向后的目标目录应先建好(否则是断链): %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "counterstrikesharp", "plugin.dll")); err != nil || string(data) != "x" {
		t.Fatalf("普通文件未拷贝: %v", err)
	}
	if same := retargetLink(logDir, "match1", "arena1"); same != logDir {
		t.Fatalf("目标里不含来源实例名时不应改写: %s", same)
	}
}

// 回归:共享 steamapps 的路径推导 = `<inst>/game/bin/linuxsteamrt64/steamapps`。
// 旧实现多剥了一层目录(`<inst>/bin/…`),主机环境验证永远命中"来源共享目录不存在"并被静默跳过,
// 新实例因此拿不到工坊图共享(真机 2026-09-21:cs new 第 5 步被跳过)。
func TestSharedSteamappsPath(t *testing.T) {
	got := sharedSteamappsPath("/home/testuser/msm.d/cs2/inst-main/game/csgo")
	want := "/home/testuser/msm.d/cs2/inst-main/game/bin/linuxsteamrt64/steamapps"
	if got != want {
		t.Fatalf("路径推导错误(旧实现得到 …/inst-main/bin/…):期望 %s,得到 %s", want, got)
	}
}

// 回归:平台给的号落在 CLI 区段时的分配冲突(e2e 实测:面板任务拿到 1000001 后,
// 下一次 `cs new` 直接 "任务编号已存在(job 1000001)")。
// 根因:① 平台号不推进 nextID;② 只信文件里的 nextId,不看实际任务号。
func TestAllocIDSkipsTakenAndAdvancesOnPlatformIDs(t *testing.T) {
	plan := Plan{Steps: []Step{{Name: "一", Run: func(c *Ctx) error { return nil }}}}

	// ① 同进程:守护替平台起任务拿到 CLI 区段的号(平台序列被 CLI 上报顶高)→ 下次 CLI 分配不许撞
	m1, dir := mkManager(t, nil)
	pj, err := m1.startWithPlan(Spec{Kind: KindGameUpdate, Origin: OriginPlatform, JobID: 1000000}, plan)
	if err != nil {
		t.Fatalf("平台任务: %v", err)
	}
	if pj.ID != 1000000 {
		t.Fatalf("平台任务应沿用平台号,得到 %d", pj.ID)
	}
	waitStatus(t, m1, pj.ID, protocol.JobDone)
	cj1, err := m1.startWithPlan(Spec{Kind: KindInstanceCreate, Origin: OriginCLI, Params: map[string]any{"name": "arena1"}}, plan)
	if err != nil {
		t.Fatalf("同进程分配撞号(旧实现 nextID 停在 1000000 → 「任务编号已存在」): %v", err)
	}
	if cj1.ID <= 1000000 {
		t.Fatalf("CLI 号必须 > 平台已用号 1000000,得到 %d", cj1.ID)
	}
	if cj1.Instance != "arena1" {
		t.Fatalf("建实例任务应记录实例名(平台 jobs.instance_name/面板实例列):%+v", cj1)
	}
	waitStatus(t, m1, cj1.ID, protocol.JobDone) // 释放组锁,否则下一个 Manager 起不来任务

	// ② 另一个进程(CLI)在同一 jobs 目录起任务:load 也要把 nextID 推过已存在的号
	cli := New(Options{Dir: filepath.Join(dir, "jobs"), Cfg: func() *config.Config { return config.Defaults() }, GroupID: func() string { return "g1" }})
	cj2, err := cli.startWithPlan(Spec{Kind: KindInstanceDelete, Origin: OriginCLI, Params: map[string]any{"name": "arena1"}},
		Plan{Steps: []Step{{Name: "一", Run: func(c *Ctx) error { return nil }}}})
	if err != nil {
		t.Fatalf("CLI 分配撞号(旧实现会报「任务编号已存在」): %v", err)
	}
	if cj2.ID <= cj1.ID {
		t.Fatalf("CLI 新任务号必须 > 已用号 %d,得到 %d", cj1.ID, cj2.ID)
	}
	waitStatus(t, cli, cj2.ID, protocol.JobDone) // 等任务写完日志再返回(否则 TempDir 清理竞态)
}

// ---- 建/删实例 ----------------------------------------------------------

func mkDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	mkDir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mkProvisionManager 造一个带 msm 布局的 Manager:
// <root>/cs2-multiserver-new 是 msm_dir,msm 数据根 = <root>/msm.d/cs2(与主机布局一致)。
func mkProvisionManager(t *testing.T) (m *Manager, root, dataRoot string) {
	t.Helper()
	root = t.TempDir()
	msmDir := filepath.Join(root, "cs2-multiserver-new")
	mkDir(t, msmDir)
	cfg := config.Defaults()
	cfg.MSMDir = msmDir
	cfg.MSM = filepath.Join(msmDir, "cs2-server")
	cfg.DemoDir = filepath.Join(root, "demos")
	cfg.ArchiveDir = filepath.Join(root, "archive")
	m = New(Options{
		Dir:          filepath.Join(root, "jobs"),
		Cfg:          func() *config.Config { return cfg },
		Names:        func() []string { return []string{"main", "match1"} },
		GroupID:      func() string { return "g1" },
		RegistryPath: filepath.Join(root, "registry.json"),
	})
	return m, root, filepath.Join(root, "msm.d", "cs2")
}

// 建实例:名字/来源/重名校验在占锁前就报错(参数非法不落任务)。
func TestInstanceCreateValidatesParams(t *testing.T) {
	m, _, _ := mkProvisionManager(t)
	instances := []string{"main", "match1"}
	if _, err := buildPlan(KindInstanceCreate, Spec{Params: map[string]any{"name": "bad name!"}}, instances, m); err == nil {
		t.Fatal("非法实例名应报错")
	}
	if _, err := buildPlan(KindInstanceCreate, Spec{Params: map[string]any{"name": "match1"}}, instances, m); err == nil {
		t.Fatal("已存在的实例名应报错")
	}
	if _, err := buildPlan(KindInstanceCreate, Spec{Params: map[string]any{"name": "arena1", "cloneFrom": "bad from!"}}, instances, m); err == nil {
		t.Fatal("cloneFrom 名字非法应报错")
	}
	if _, err := buildPlan(KindInstanceCreate, Spec{Params: map[string]any{"name": "main"}}, instances, m); err == nil {
		t.Fatal("目标名与模板同名应报错")
	}
	plan, err := buildPlan(KindInstanceCreate, Spec{Params: map[string]any{"name": "arena1"}}, instances, m)
	if err != nil {
		t.Fatalf("合法参数应可构建计划: %v", err)
	}
	if len(plan.Steps) < 6 || plan.Recover == nil {
		t.Fatalf("建实例步骤数/回滚缺失: %d", len(plan.Steps))
	}
}

// 删实例:confirm 必须精确等于实例名;唯一实例拒绝删除。
func TestInstanceDeleteGuards(t *testing.T) {
	m, _, _ := mkProvisionManager(t)
	instances := []string{"main", "match1"}
	if _, err := buildPlan(KindInstanceDelete, Spec{Params: map[string]any{"name": "match1"}}, instances, m); err == nil {
		t.Fatal("缺 confirm 应报错")
	}
	if _, err := buildPlan(KindInstanceDelete, Spec{Params: map[string]any{"name": "match1", "confirm": "main"}}, instances, m); err == nil {
		t.Fatal("confirm 与实例名不符应报错")
	}
	if _, err := buildPlan(KindInstanceDelete, Spec{Params: map[string]any{"name": "ghost", "confirm": "ghost"}}, instances, m); err == nil {
		t.Fatal("不在注册表/白名单内的实例应拒绝删除")
	}
	if _, err := buildPlan(KindInstanceDelete, Spec{Params: map[string]any{"name": "main", "confirm": "main"}}, []string{"main"}, m); err == nil {
		t.Fatal("唯一实例应拒绝删除")
	}
	if _, err := buildPlan(KindInstanceDelete, Spec{Params: map[string]any{"name": "match1", "confirm": "match1"}}, instances, m); err != nil {
		t.Fatalf("合法删除应可构建计划: %v", err)
	}
}

// 删除安全边界:msm 数据根之外的路径一律拒绝(防误删主机目录)。
func TestRemoveTreeGuardedBounds(t *testing.T) {
	m, root, dataRoot := mkProvisionManager(t)
	msmDir := m.opt.Cfg().MSMDir
	inside := filepath.Join(dataRoot, "inst-match1")
	outside := filepath.Join(root, "important")
	writeFile(t, filepath.Join(inside, "game", "csgo", "a.txt"), "x")
	writeFile(t, filepath.Join(outside, "b.txt"), "x")
	if _, err := removeTreeGuarded(msmDir, inside); err != nil {
		t.Fatalf("数据根内应可删除: %v", err)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatal("实例目录应已删除")
	}
	if _, err := removeTreeGuarded(msmDir, outside); err == nil {
		t.Fatal("数据根之外的路径必须拒绝")
	}
	if _, err := os.Stat(filepath.Join(outside, "b.txt")); err != nil {
		t.Fatal("数据根之外的文件不得被删除")
	}
}

// 删实例全程:停服(未运行则跳过)→ 删两个目录 → 注册表移除 + 结果带 freedBytes。
func TestInstanceDeleteExecutesAndUpdatesRegistry(t *testing.T) {
	m, _, dataRoot := mkProvisionManager(t)
	instDir := filepath.Join(dataRoot, "inst-match1")
	cfgDir := filepath.Join(dataRoot, "cfg", "inst-match1")
	writeFile(t, filepath.Join(instDir, "game", "csgo", "big.bin"), strings.Repeat("x", 4096))
	writeFile(t, filepath.Join(cfgDir, "server.conf"), "PORT=\"27016\"\n")
	// main 也真实存在(注册表 Sync 会按主机实际布局剪掉"盘上没有"的条目)
	writeFile(t, filepath.Join(dataRoot, "cfg", "inst-main", "server.conf"), "PORT=\"27015\"\n")
	// 注册表里先有 main + match1
	if err := registry.Save(m.opt.RegistryPath, registry.File{MaxIdx: 2, Items: []registry.Instance{
		{Idx: 1, Name: "main", Port: 27015, GotvPort: 27115},
		{Idx: 2, Name: "match1", Port: 27016, GotvPort: 27116},
	}}); err != nil {
		t.Fatal(err)
	}

	plan, err := buildPlan(KindInstanceDelete, Spec{Params: map[string]any{"name": "match1", "confirm": "match1"}}, []string{"main", "match1"}, m)
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.startWithPlan(Spec{Kind: KindInstanceDelete, Params: map[string]any{"name": "match1"}, Confirm: "match1"}, plan)
	if err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, m, j.ID, protocol.JobDone)
	if freed, _ := done.Result["freedBytes"].(int64); freed < 4096 {
		t.Fatalf("freedBytes 应 ≥ 4096,得到 %v(%v)", done.Result["freedBytes"], done.Result)
	}
	if _, err := os.Stat(instDir); !os.IsNotExist(err) {
		t.Fatal("实例目录应已删除")
	}
	if _, err := os.Stat(cfgDir); !os.IsNotExist(err) {
		t.Fatal("实例配置目录应已删除")
	}
	back := registry.Load(m.opt.RegistryPath)
	if len(back.Items) != 1 || back.Items[0].Name != "main" || back.MaxIdx != 2 {
		t.Fatalf("注册表应只剩 main 且编号高水位保持 2: %+v", back)
	}
}
