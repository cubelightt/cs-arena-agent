// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package job

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"arena/agent/internal/config"
	"arena/agent/internal/msm"
	"arena/agent/internal/protocol"
)

// Spec 是一次任务的请求(job_start op / CLI 共用)。
type Spec struct {
	Kind   string
	Params map[string]any
	// JobID 由平台提供(守护路径,= 后端 jobs.id);0 = 本地自增(CLI 路径)。
	JobID  int64
	Origin string
	// CLIUser 记录发起任务的主机用户名(cs update 走 CLI)。
	CLIUser string
	// Confirm 是危险操作的二次确认串。
	Confirm string
}

// Step 是一个步骤;Run 返回 error 即任务失败(取消不算失败)。
type Step struct {
	Name string
	Run  func(c *Ctx) error
}

// Plan 是一个 kind 的执行计划(取消/失败后的恢复步骤独立于 Steps)。
type Plan struct {
	Steps   []Step
	Recover func(c *Ctx)
}

// Ctx 是步骤的执行上下文。
type Ctx struct {
	Job   *Job
	Cfg   *config.Config
	Paths *msm.Paths
	// Instances 是本任务涉及的实例(组的白名单)。
	Instances []string

	Runner  *msm.Runner
	GroupID string

	logf    func(format string, args ...any)
	logLine func(string)
	j       *Job
	mgr     *Manager
}

// Logf 写一行桥侧说明(带 [cs] 前缀)到任务日志。
func (c *Ctx) Logf(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
	}
}

// LogLine 原样落一行(msm 输出)。
func (c *Ctx) LogLine(line string) {
	if c.logLine != nil {
		c.logLine(line)
	}
}

// MSM 执行一条 msm 命令:流式输出进任务日志,可被取消(force 时 kill 进程组)。
func (c *Ctx) MSM(instance, op string, extra []string, timeoutS int) (*msm.Result, error) {
	return c.Runner.RunStream(c.ctx(), instance, op, extra, msm.StreamOpts{
		TimeoutS: timeoutS,
		OnLine:   func(line string) { c.LogLine(fmt.Sprintf("%s %s | %s", instance, op, line)) },
	})
}

// SetProgress 更新进度(0~100)并推状态帧。
func (c *Ctx) SetProgress(pct int) {
	c.mgr.progress(c.j.ID, pct)
}

// Cancelled 报告是否已请求取消(A 或 B)。
func (c *Ctx) Cancelled() bool { return c.mgr.cancelRequested(c.j.ID) }

// ForceCancelled 报告是否已被强制取消(B)。
func (c *Ctx) ForceCancelled() bool { return c.mgr.forceRequested(c.j.ID) }

func (c *Ctx) ctx() context.Context { return c.mgr.jobCtx(c.j.ID) }

// ---- 组锁(flock,跨进程)----------------------------------------------------

func (m *Manager) lockPath(groupID string) string {
	if groupID == "" {
		groupID = "local"
	}
	return filepath.Join(m.opt.Dir, groupID+".lock")
}

// acquireGroupLock 非阻塞取组锁;失败返回持有者说明(尽力读出 holder 信息)。
func (m *Manager) acquireGroupLock(groupID string) error {
	if err := os.MkdirAll(m.opt.Dir, 0o755); err != nil {
		return err
	}
	if j := m.ActiveOfGroup(groupID); j != nil {
		return fmt.Errorf("该服务器组已有进行中任务(job %d)", j.ID)
	}
	f, err := os.OpenFile(m.lockPath(groupID), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		holder := strings.TrimSpace(readFileString(m.lockPath(groupID)))
		if holder != "" {
			return fmt.Errorf("该服务器组已有进行中任务(%s)", holder)
		}
		return errors.New("该服务器组已有进行中任务")
	}
	// 写入持有者说明,供"另一个进程"的报错可读
	f.Truncate(0)
	f.Seek(0, 0)
	fmt.Fprintf(f, "job 由 pid %d 持有\n", os.Getpid())
	f.Sync()
	m.mu.Lock()
	m.locks[groupID] = f
	m.mu.Unlock()
	return nil
}

func (m *Manager) releaseGroupLock(groupID string) {
	if groupID == "" {
		groupID = "local"
	}
	m.mu.Lock()
	f := m.locks[groupID]
	delete(m.locks, groupID)
	m.mu.Unlock()
	if f != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

func readFileString(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

// ---- 启动 ------------------------------------------------------------------

// Start 校验并启动一个任务(立即返回;执行在后台 goroutine)。
func (m *Manager) Start(spec Spec) (*Job, error) {
	kind := spec.Kind
	if kind == "" {
		return nil, errors.New("缺少 kind")
	}
	groupID := ""
	if m.opt.GroupID != nil {
		groupID = m.opt.GroupID()
	}
	if groupID == "" {
		groupID = "local"
	}
	instances := m.groupInstances()

	// 计划先构建:参数非法/确认缺失要在占锁之前报错
	plan, err := buildPlan(kind, spec, instances, m)
	if err != nil {
		return nil, err
	}
	return m.startWithPlan(spec, plan)
}

// startWithPlan 起任务(plan 已构建;测试与生产共用这条路径)。
func (m *Manager) startWithPlan(spec Spec, plan Plan) (*Job, error) {
	kind := spec.Kind
	groupID := ""
	if m.opt.GroupID != nil {
		groupID = m.opt.GroupID()
	}
	if groupID == "" {
		groupID = "local"
	}
	if err := m.acquireGroupLock(groupID); err != nil {
		return nil, err
	}

	m.mu.Lock()
	id := m.allocIDLocked(spec.JobID)
	if _, dup := m.jobs[id]; dup {
		m.mu.Unlock()
		m.releaseGroupLock(groupID)
		return nil, fmt.Errorf("任务编号已存在(job %d)", id)
	}
	origin := spec.Origin
	if origin == "" {
		origin = OriginCLI
	}
	j := &Job{
		ID: id, Kind: kind, GroupID: groupID, Origin: origin, CLIUser: spec.CLIUser,
		Status: protocol.JobQueued, StartedAt: time.Now().UnixMilli(),
		StepTotal: len(plan.Steps), Params: spec.Params, PID: os.Getpid(),
	}
	// 建删实例任务记录实例名(平台 jobs.instance_name + 面板实例列;plugin_deploy 的 name 是插件名,不算)
	if kind == KindInstanceCreate || kind == KindInstanceDelete {
		j.Instance = strParam(spec.Params, "name")
	}
	m.jobs[id] = j
	m.order = append(m.order, id)
	m.own[id] = true // 本进程执行:Reload 时该任务的运行态以内存为准
	m.saveLocked()
	m.mu.Unlock()

	go m.run(j, plan)
	return copyJob(j), nil
}

// groupInstances 取本任务涉及的实例 = 白名单(桥只服务一个组)。
func (m *Manager) groupInstances() []string {
	if m.opt.Names == nil {
		return nil
	}
	out := append([]string(nil), m.opt.Names()...)
	return out
}

// run 执行计划(唯一写者:任务状态与日志)。
func (m *Manager) run(j *Job, plan Plan) {
	ctx, cancel := context.WithCancel(context.Background())
	m.setJobCtx(j.ID, cancel, ctx)
	defer m.clearJobCtx(j.ID)
	defer m.releaseGroupLock(j.GroupID)

	// 跨进程取消:每 500ms 读一次请求文件(<jobs>/<id>.cancel)。
	// 只在**本进程执行**期间轮询 —— 请求方(另一个进程)写文件,这里消费成内存标志;
	// A 语义由步骤循环的边界检查生效,force 由 consumeCancelRequest 直接 kill 进程组。
	pollStop := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-pollStop:
				return
			case <-t.C:
				m.consumeCancelRequest(j.ID)
			}
		}
	}()
	defer close(pollStop)

	lg, err := m.openLog(j.ID)
	if err != nil {
		m.finish(j, protocol.JobFailed, "无法创建任务日志: "+err.Error())
		return
	}
	defer lg.Close()

	c := &Ctx{Job: j, Cfg: m.opt.Cfg(), Paths: m.opt.Paths, Instances: m.groupInstances(), Runner: m.opt.Runner, GroupID: j.GroupID, j: j, mgr: m}
	c.logf = func(format string, args ...any) { lg.line("[cs] " + fmt.Sprintf(format, args...)) }
	c.logLine = func(line string) { lg.line(line) }

	m.setState(j.ID, protocol.JobRunning, "")
	c.Logf("任务 %d(%s)开始:组 %s,实例 %d 个", j.ID, j.Kind, j.GroupID, len(c.Instances))

	status := protocol.JobDone
	errText := ""
	for i, st := range plan.Steps {
		// 步骤边界 = 取消点(A 语义在这里生效;**不打断进行中的步骤**)
		if m.cancelRequested(j.ID) {
			c.Logf("收到取消请求,在当前步骤边界停止(已完成的步骤不回滚)")
			status = protocol.JobCancelled
			break
		}
		if ctx.Err() != nil {
			status = protocol.JobCancelled
			break
		}
		m.step(j.ID, i+1, len(plan.Steps), st.Name)
		c.Logf("步骤 %d/%d %s", i+1, len(plan.Steps), st.Name)
		if err := st.Run(c); err != nil {
			if m.forceRequested(j.ID) || ctx.Err() != nil {
				status = protocol.JobCancelled
				errText = ""
				c.Logf("步骤被强制取消:%v", err)
			} else {
				status = protocol.JobFailed
				errText = err.Error()
				c.Logf("步骤失败:%v", errText)
			}
			break
		}
	}
	if m.cancelRequested(j.ID) && status == protocol.JobDone {
		status = protocol.JobCancelled
	}
	if status == protocol.JobCancelled || status == protocol.JobFailed {
		if plan.Recover != nil {
			c.Logf("执行恢复步骤(把实例拉回更新前状态)")
			plan.Recover(c)
		}
	}
	m.finishWith(j.ID, status, errText, c, lg)
}

// finish 在任务还没跑起来时收尾(日志都建不出来)。
func (m *Manager) finish(j *Job, status, errText string) {
	m.clearCancelRequest(j.ID)
	m.mu.Lock()
	if cur := m.jobs[j.ID]; cur != nil {
		cur.Status = status
		cur.Error = errText
		cur.FinishedAt = time.Now().UnixMilli()
		cur.Reported = false
	}
	m.saveLocked()
	m.mu.Unlock()
	m.pushStatus(j.ID, "")
}

// finishWith 正常终态收尾:写结果、推终态帧(未连接则留给守护收敛上报)。
func (m *Manager) finishWith(id int64, status, errText string, c *Ctx, lg *jobLog) {
	// 请求文件必须在**终态可见之前**清掉:CLI 前台跟随的主协程一看到终态就可能立刻退出进程
	// (cs 命令的退出码路径),放在这之后清理会随进程退出一起丢掉(主机环境验证:文件残留)。
	m.clearCancelRequest(id)
	m.mu.Lock()
	cur := m.jobs[id]
	if cur == nil {
		m.mu.Unlock()
		return
	}
	cur.Status = status
	cur.Error = errText
	cur.FinishedAt = time.Now().UnixMilli()
	cur.Progress = 100
	cur.Reported = false
	if c != nil && cur.Result == nil {
		cur.Result = map[string]any{}
	}
	m.saveLocked()
	m.mu.Unlock()
	if c != nil {
		c.logf("任务结束:%s%s", status, orDash(errText))
	}
	lg.Flush()
	m.pushStatus(id, "")
}

func orDash(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

// ---- 状态变更 --------------------------------------------------------------

func (m *Manager) setState(id int64, status, errText string) {
	m.mutate(id, func(j *Job) {
		j.Status = status
		if errText != "" {
			j.Error = errText
		}
		if status == protocol.JobRunning && j.StartedAt == 0 {
			j.StartedAt = time.Now().UnixMilli()
		}
	})
	m.pushStatus(id, "")
}

func (m *Manager) step(id int64, idx, total int, name string) {
	m.mutate(id, func(j *Job) {
		j.StepIndex, j.StepTotal, j.Step = idx, total, name
		if total > 0 {
			j.Progress = int(float64(idx-1) / float64(total) * 100)
		}
	})
	m.pushStatus(id, "")
}

func (m *Manager) progress(id int64, pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	m.mutate(id, func(j *Job) { j.Progress = pct })
	m.pushStatus(id, "")
}

// SetResult 供步骤写入任务结果(例如 freedBytes)。
func (m *Manager) SetResult(id int64, key string, val any) {
	m.mutate(id, func(j *Job) {
		if j.Result == nil {
			j.Result = map[string]any{}
		}
		j.Result[key] = val
	})
}

// SetResultFn 返回给步骤用的便捷写法。
func (c *Ctx) SetResult(key string, val any) { c.mgr.SetResult(c.j.ID, key, val) }

// ---- 取消 ------------------------------------------------------------------

// Cancel 取消任务:force=false → A(步骤边界);force=true → B(立即 kill 进程组)。
//
// 两种情形:
//   - **本进程在跑这个任务**(own/cancels 有它)→ 直接置内存标志(A 在步骤边界生效,B 立刻 kill 进程组);
//   - **别的进程在跑**(CLI 起的长任务被面板/守护取消,或反之)→ 落取消请求文件
//     `<jobs>/<id>.cancel`,执行进程 500ms 轮询后按同一套语义处理(内存标志跨不了进程)。
func (m *Manager) Cancel(id int64, force bool) (map[string]any, error) {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("任务不存在(job %d)", id)
	}
	switch j.Status {
	case protocol.JobDone, protocol.JobFailed, protocol.JobCancelled:
		status := j.Status
		m.mu.Unlock()
		return nil, fmt.Errorf("任务已结束(job %d,%s)", id, status)
	}
	if !m.own[id] && m.cancels[id] == nil {
		// 跨进程:不碰 index.json(它是执行进程的写场,只有执行方才有最新状态)
		m.mu.Unlock()
		if err := m.WriteCancelRequest(id, force, fmt.Sprintf("pid %d", os.Getpid())); err != nil {
			return nil, fmt.Errorf("写入取消请求失败: %w", err)
		}
		m.logf("[job] 任务 %d 的取消请求已落盘(本进程不是执行方;force=%v)", id, force)
		return map[string]any{
			"ok": true, "jobId": id, "status": protocol.JobCancelling, "requested": true,
			"note": fmt.Sprintf("取消请求已写入 %s,由执行该任务的进程处理", m.cancelFilePath(id)),
		}, nil
	}
	if force {
		j.forceReq = true
	} else {
		j.cancelReq = true
	}
	j.Status = protocol.JobCancelling
	m.saveLocked()
	m.mu.Unlock()

	if force {
		m.mu.Lock()
		cancel := m.cancels[id]
		m.mu.Unlock()
		if cancel != nil {
			cancel() // kill 进程组(见 msm.RunStream 的 cmd.Cancel)
		}
	}
	m.pushStatus(id, "")
	cur := m.Get(id)
	st := protocol.JobCancelling
	if cur != nil {
		st = cur.Status
	}
	return map[string]any{"ok": true, "jobId": id, "status": st}, nil
}

// consumeCancelRequest 把**另一个进程**写下的取消请求转成本进程的内存标志
// (force 时立即 kill 进程组)。由 run() 的轮询协程调用;A 语义仍在步骤边界生效。
// 返回是否首次消费(用于日志/推送去重)。
func (m *Manager) consumeCancelRequest(id int64) bool {
	force, ok := m.readCancelRequest(id)
	if !ok {
		return false
	}
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil {
		m.mu.Unlock()
		return false
	}
	first := !j.cancelReq && !j.forceReq
	j.cancelReq = true
	if force {
		j.forceReq = true
	}
	moved := false
	if j.Status == protocol.JobQueued || j.Status == protocol.JobRunning {
		j.Status = protocol.JobCancelling
		moved = true
	}
	cancel := m.cancels[id]
	m.saveLocked()
	m.mu.Unlock()
	if cancel != nil && force {
		cancel() // kill 进程组(msm.RunStream 的 cmd.Cancel)
	}
	if first {
		m.logf("[job] 任务 %d 收到跨进程取消请求(force=%v)", id, force)
	}
	if first || moved {
		m.pushStatus(id, "")
	}
	return first
}

func (m *Manager) cancelRequested(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	return j != nil && (j.cancelReq || j.forceReq)
}

func (m *Manager) forceRequested(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	return j != nil && j.forceReq
}

func (m *Manager) setJobCtx(id int64, cancel context.CancelFunc, ctx context.Context) {
	m.mu.Lock()
	m.cancels[id] = cancel
	m.ctxs[id] = ctx
	m.mu.Unlock()
}

func (m *Manager) clearJobCtx(id int64) {
	m.mu.Lock()
	delete(m.cancels, id)
	delete(m.ctxs, id)
	m.mu.Unlock()
}

func (m *Manager) jobCtx(id int64) context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx := m.ctxs[id]; ctx != nil {
		return ctx
	}
	return context.Background()
}

// ---- 日志 ------------------------------------------------------------------

// jobLog 是任务日志:行式追加 + 推送增量(push kind job 的 line 字段)。
type jobLog struct {
	mu     sync.Mutex
	f      *os.File
	id     int64
	mgr    *Manager
	buf    []string
	lastAt time.Time
}

func (m *Manager) openLog(id int64) (*jobLog, error) {
	if err := os.MkdirAll(m.opt.Dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(m.opt.Dir, LogName(id)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &jobLog{f: f, id: id, mgr: m}, nil
}

// line 写一行(总是落盘;推送按 200ms 节流 —— 丢的推送可用 job_log 按 offset 补拉)。
func (l *jobLog) line(s string) {
	l.mu.Lock()
	l.f.WriteString(s + "\n")
	l.f.Sync()
	now := time.Now()
	shouldPush := now.Sub(l.lastAt) >= 200*time.Millisecond
	l.lastAt = now
	l.mu.Unlock()
	if shouldPush {
		l.mgr.pushStatus(l.id, s)
	}
}

func (l *jobLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
	}
}

// Flush 兼容占位(行已即时落盘)。
func (l *jobLog) Flush() {}
