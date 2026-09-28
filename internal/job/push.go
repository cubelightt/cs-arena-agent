// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package job

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"arena/agent/internal/fsx"
	"arena/agent/internal/protocol"
)

// ---- 上行:push kind job 与 job_report(守护注入 Push;CLI 进程为 nil → 只落盘)------

// pushStatus 发一帧 push kind:'job'(state 变化 line="" 或增量日志行)。
func (m *Manager) pushStatus(id int64, line string) {
	push := m.pushFn()
	if push == nil {
		return // CLI 进程 / 断线:没有连接,状态由守护收敛上报
	}
	if line != "" && !m.subscribed(id) {
		return // 只对已订阅的任务推日志行(与控制台 tail 同款约定)
	}
	j := m.Get(id)
	if j == nil {
		return
	}
	frame := protocol.JobPush{
		Type: protocol.TypePush, Kind: protocol.KindJob, JobID: j.ID, Status: j.Status,
		Step: j.Step, StepIndex: j.StepIndex, StepTotal: j.StepTotal, Progress: j.Progress, Line: line,
	}
	if isTerminal(j.Status) {
		frame.Result = j.Result // 终态帧带 result(建删实例的 port/idx 等)
	}
	if err := push(frame); err != nil {
		// 断线不阻断任务:任务继续跑,状态落盘,重连后由守护 ReportPending 收敛
		m.logf("[job] push 失败(job %d,将重连后上报): %v", id, err)
	}
}

// JobReportFrame 把任务转成 job_report 帧。
// Result 只在终态带上(与 push kind:'job' 同规则);Instance 供面板任务列表显示实例列。
func JobReportFrame(j *Job) protocol.JobReport {
	row := protocol.JobReportRow{
		JobID: j.ID, Kind: j.Kind, GroupID: j.GroupID, Origin: j.Origin, Status: j.Status,
		Step: j.Step, StepIndex: j.StepIndex, StepTotal: j.StepTotal, Progress: j.Progress,
		StartedAt: j.StartedAt, FinishedAt: j.FinishedAt, Error: j.Error, CLIUser: j.CLIUser,
		Instance: j.Instance,
	}
	if isTerminal(j.Status) {
		row.Result = j.Result
	}
	return protocol.JobReport{Type: protocol.TypeJobReport, Job: row}
}

// ReportPending 把"未上报"的任务收敛给后端(重连后调用 + 每次 health tick 兜底)。
//
// 覆盖三种情况:① CLI 进程发起(无连接,只落盘);② 断线期间发起/结束;
// ③ 平台侧发起但终态帧丢失(重连后补)。已上报的**终态**任务不再重发;
// 进行中的任务每次都会重发当前快照(平台据此更新进度,幂等)。
func (m *Manager) ReportPending() int {
	push := m.pushFn()
	if push == nil {
		return 0
	}
	// 先合并**其他进程**(CLI)`cs` 命令写进 index.json 的任务 —— 否则守护看不到主机侧发起的任务
	m.Reload()
	todo := m.pendingReports()
	sent := 0
	for _, j := range todo {
		if err := push(JobReportFrame(j)); err != nil {
			m.logf("[job] job_report 发送失败(job %d): %v", j.ID, err)
			break // 连接有问题,剩下的留给下一轮
		}
		sent++
		if isTerminal(j.Status) {
			m.mutate(j.ID, func(cur *Job) { cur.Reported = true })
		}
	}
	return sent
}

// pendingReports 列出待上报任务:终态未上报的 + 进行中的。
func (m *Manager) pendingReports() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.order))
	for _, id := range m.order {
		j := m.jobs[id]
		if j == nil {
			continue
		}
		if isTerminal(j.Status) {
			if !j.Reported {
				out = append(out, copyJob(j))
			}
			continue
		}
		out = append(out, copyJob(j))
	}
	return out
}

func isTerminal(status string) bool {
	switch status {
	case protocol.JobDone, protocol.JobFailed, protocol.JobCancelled:
		return true
	}
	return false
}

// ---- 订阅(与控制台订阅同款:后端重连后按 offset 续推)------------------------

// Subscribe 记录订阅并立即补推 status 帧 + offset 之后的日志行。
func (m *Manager) Subscribe(id int64, offset int64) map[string]any {
	if m.Get(id) == nil {
		return map[string]any{"ok": false, "error": fmt.Sprintf("任务不存在(job %d)", id)}
	}
	m.subMu.Lock()
	m.subs[id] = true
	m.subMu.Unlock()
	m.pushStatus(id, "")
	lines, next, err := m.LogSince(id, offset)
	if err == nil && len(lines) > 0 {
		for _, ln := range lines {
			m.rawPush(id, ln)
		}
		m.subMu.Lock()
		m.subOffsets[id] = next
		m.subMu.Unlock()
	}
	return map[string]any{"ok": true, "jobId": id}
}

// Unsubscribe 取消订阅。
func (m *Manager) Unsubscribe(id int64) map[string]any {
	m.subMu.Lock()
	delete(m.subs, id)
	delete(m.subOffsets, id)
	m.subMu.Unlock()
	return map[string]any{"ok": true, "jobId": id}
}

// SubscribedIDs 返回当前订阅的任务号(job_subscribe 重放用)。
func (m *Manager) SubscribedIDs() []int64 {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	out := make([]int64, 0, len(m.subs))
	for id := range m.subs {
		out = append(out, id)
	}
	return out
}

func (m *Manager) subscribed(id int64) bool {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	return m.subs[id]
}

// rawPush 推一行日志(订阅补推时绕过节流)。
func (m *Manager) rawPush(id int64, line string) {
	push := m.pushFn()
	if push == nil {
		return
	}
	j := m.Get(id)
	if j == nil {
		return
	}
	frame := protocol.JobPush{
		Type: protocol.TypePush, Kind: protocol.KindJob, JobID: j.ID, Status: j.Status,
		Step: j.Step, StepIndex: j.StepIndex, StepTotal: j.StepTotal, Progress: j.Progress, Line: line,
	}
	if err := push(frame); err != nil {
		m.logf("[job] push(补推)失败(job %d): %v", id, err)
	}
}

// ---- 任务日志读取(job_log op)----------------------------------------------

// LogPath 返回任务日志的绝对路径。
func (m *Manager) LogPath(id int64) string { return filepath.Join(m.opt.Dir, LogName(id)) }

// LogSince 读 offset 之后的**完整行**(语义与 log op 的增量一致),返回 (行, 新 offset, err)。
// offset<=0 表示从头读。
func (m *Manager) LogSince(id int64, offset int64) ([]string, int64, error) {
	if offset < 0 {
		offset = 0
	}
	return fsx.ReadNewLines(m.LogPath(id), offset)
}

// LogTail 读末尾 n 行(任务已结束时 CLI 常用)。
func (m *Manager) LogTail(id int64, lines int) ([]string, error) {
	return fsx.ReadTail(m.LogPath(id), lines)
}

// LogSize 返回日志字节数(CLI/面板展示)。
func (m *Manager) LogSize(id int64) int64 {
	fi, err := os.Stat(m.LogPath(id))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// ---- 清理 ------------------------------------------------------------------

// GCBefore 删除早于 t 的终态任务索引项与日志(M6 的主机清理会复用;此处供 CLI --json 展示用)。
func (m *Manager) GCBefore(t time.Time) (removed int) {
	m.mu.Lock()
	keep := make([]int64, 0, len(m.order))
	for _, id := range m.order {
		j := m.jobs[id]
		if j == nil {
			continue
		}
		old := isTerminal(j.Status) && j.FinishedAt > 0 && time.UnixMilli(j.FinishedAt).Before(t)
		if old {
			delete(m.jobs, id)
			removed++
			continue
		}
		keep = append(keep, id)
	}
	m.order = keep
	if removed > 0 {
		m.saveLocked()
	}
	m.mu.Unlock()
	for _, id := range m.order {
		_ = id
	}
	return removed
}
