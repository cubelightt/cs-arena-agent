// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package job 是桥的任务框架。
//
// 三件事:
//   - **按组独占**:一个服务器组同时只允许一个 running/cancelling 任务(flock `<dir>/<groupId>.lock`,
//     跨进程生效 —— 守护与 `cs` CLI 是两个进程,CLI 起的任务守护也能看见、平台也能查到);
//   - **步骤化执行**:每个 kind 由若干步骤组成,步骤边界是取消点(默认 A 语义);
//     `--force`/`force:true` 走 B 语义:kill 进程组(msm 侧见 RunStream);
//   - **可收敛的落盘状态**:`<dir>/index.json`(任务快照 + 自增编号)+ `<dir>/<id>.log`(行式日志,
//     offset 语义与 log op 一致)。任务状态文件是**唯一真源**:平台侧 `job_status`/`job_log`
//     即使任务由 CLI 进程发起也能读到;守护负责把未上报的任务收敛给后端(这正是"待上报队列",
//     放 jobs/ 而非 state.json，避免 CLI 与守护并发写同一文件。
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"arena/agent/internal/config"
	"arena/agent/internal/fsx"
	"arena/agent/internal/msm"
	"arena/agent/internal/protocol"
)

// pidAlive 判断记录的执行进程是否还在(PID ≤ 0 = 未知 → 视为不在)。
// 用于区分"另一个进程仍在跑这个任务"与"进程已死、任务被中断"。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	// EPERM = 进程存在但不属于本用户(理论上不会发生:同一台机同一账号)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// CLIJobIDBase 是 CLI 侧分配的任务编号起点:平台 jobs 表用 AUTOINCREMENT(小整数),
// 两边用不同区段,避免编号撞车后要靠 (kind,groupId,startedAt) 归并。
const CLIJobIDBase = 1000000

// 任务 kind。
const (
	KindGameUpdate     = "game_update"
	KindPluginSync     = "plugin_sync"
	KindPluginDeploy   = "plugin_deploy"
	KindDemoCollect    = "demo_collect"
	KindHostCleanup    = "host_cleanup"
	KindInstanceCreate = "instance_create"
	KindInstanceDelete = "instance_delete"
)

// 任务来源。
const (
	OriginPlatform = "platform"
	OriginCLI      = "cli"
)

// Job 是任务的运行态 + 展示态(整体落 index.json)。
type Job struct {
	ID         int64          `json:"id"`
	Kind       string         `json:"kind"`
	GroupID    string         `json:"groupId"`
	Instance   string         `json:"instance,omitempty"`
	Origin     string         `json:"origin"`
	CLIUser    string         `json:"cliUser,omitempty"`
	Status     string         `json:"status"`
	Step       string         `json:"step,omitempty"`
	StepIndex  int            `json:"stepIndex"`
	StepTotal  int            `json:"stepTotal"`
	Progress   int            `json:"progress"`
	StartedAt  int64          `json:"startedAt"`
	FinishedAt int64          `json:"finishedAt,omitempty"`
	Error      string         `json:"error,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
	Params     map[string]any `json:"params,omitempty"`
	// Reported:终态是否已上报给平台(false 且已终态 = 待上报队列里的一项)。
	Reported bool `json:"reported,omitempty"`
	// PID:执行该任务的进程(CLI 发起时是 CLI 进程),排障用。
	PID int `json:"pid,omitempty"`

	// 运行期(不入盘)
	cancelReq bool
	forceReq  bool
}

// LogName 是任务日志文件名(与 job_log 的 path 一致)。
func LogName(id int64) string { return fmt.Sprintf("%d.log", id) }

// Options 是构造 Manager 的依赖。
type Options struct {
	// Dir = <config_dir>/jobs(任务索引、日志、组锁都在这里)。
	Dir string
	// Cfg/Paths/Runner 与 ops 层共用同一份运行时(配置热重载、msm 串行锁)。
	Cfg    func() *config.Config
	Paths  *msm.Paths
	Runner *msm.Runner
	// Names 返回实例白名单(后端下发 → 缓存 → 注册表)。
	Names func() []string
	// GroupID 返回本桥当前 server_id(= 本桥服务的那一个服务器组)。
	GroupID func() string
	// RegistryPath 是编号注册表文件(<config_dir>/registry.json;建/删实例任务要写它)。
	// 为空时退化为 <Dir>/../registry.json。
	RegistryPath string
	// Push 发送上行帧(守护注入;CLI 进程为 nil → 只落盘,由守护收敛上报)。
	Push   func(any) error
	Logger func(format string, args ...any)
}

// Manager 管理本主机上的全部任务(同一份 jobs/ 目录,跨进程可见)。
type Manager struct {
	opt Options

	mu     sync.Mutex
	jobs   map[int64]*Job
	order  []int64
	nextID int64
	// 正在跑的组的 flock(进程内持有;进程退出由内核释放)
	locks map[string]*os.File
	// ctx/cancel:force 取消时用来 kill 进程组
	ctxs    map[int64]context.Context
	cancels map[int64]context.CancelFunc

	// own 记录**本进程**发起执行的任务号。Reload 只对 own 里的任务用内存状态兜底;
	// 其他任务(CLI 进程发起、本进程只是经 Reload 缓存了快照)一律以盘上为准 ——
	// 否则 CLI 的长任务(时长 > 守护轮询间隔)会被永久钉在 running:
	// 守护看到过它的 running 快照 → CLI 写盘 done → Reload 因"内存里是 running"跳过合并。
	own map[int64]bool

	subMu      sync.Mutex
	subs       map[int64]bool
	subOffsets map[int64]int64

	// pushMu 保护 opt.Push:守护每建一次连接就换一次推送目标(与控制台 Hub 同款)
	pushMu sync.RWMutex
}

// SetPush 绑定上行发送(守护在每次连接建立时注入,断线时置 nil)。
func (m *Manager) SetPush(fn func(any) error) {
	m.pushMu.Lock()
	m.opt.Push = fn
	m.pushMu.Unlock()
}

// ClearPush 解绑上行发送(断线:任务继续跑,状态落盘,重连后 ReportPending 收敛)。
func (m *Manager) ClearPush() { m.SetPush(nil) }

// pushFn 取当前上行发送(可能为 nil:CLI 进程 / 断线期间)。
func (m *Manager) pushFn() func(any) error {
	m.pushMu.RLock()
	defer m.pushMu.RUnlock()
	return m.opt.Push
}

// New 构造 Manager 并从 index.json 载入历史任务。
func New(opt Options) *Manager {
	if opt.Logger == nil {
		opt.Logger = func(string, ...any) {}
	}
	m := &Manager{
		opt: opt, jobs: map[int64]*Job{}, nextID: CLIJobIDBase,
		locks: map[string]*os.File{}, ctxs: map[int64]context.Context{}, cancels: map[int64]context.CancelFunc{},
		subs: map[int64]bool{}, subOffsets: map[int64]int64{}, own: map[int64]bool{},
	}
	m.load()
	return m
}

func (m *Manager) logf(format string, args ...any) { m.opt.Logger(format, args...) }

// ---- 索引持久化 ------------------------------------------------------------

type indexFile struct {
	NextID int64  `json:"nextId"`
	Jobs   []*Job `json:"jobs"`
}

func (m *Manager) indexPath() string { return filepath.Join(m.opt.Dir, "index.json") }

// Dir 是任务目录(<config_dir>/jobs);清桥自身目录冗余件的 bridge:old 模式据它推导 <config_dir>。
func (m *Manager) Dir() string { return m.opt.Dir }

// ---- 跨进程取消请求(独立文件,不入 index.json)--------------------------------
//
// 取消请求写**独立文件** `<jobs>/<id>.cancel` 而不是 index.json:索引由执行进程频繁重写
// (进度/步骤每次变化),请求写在索引里会被执行进程的下一次重写盖掉;独立文件只由请求方创建、
// 执行方消费,语义干净。执行进程在 run() 里 500ms 轮询一次(见 consumeCancelRequest)。

// cancelFilePath 是任务 <id> 的取消请求文件路径。
func (m *Manager) cancelFilePath(id int64) string {
	return filepath.Join(m.opt.Dir, fmt.Sprintf("%d.cancel", id))
}

// cancelRequest 是取消请求体(by 只用于排障:谁请求的)。
type cancelRequest struct {
	Force bool   `json:"force"`
	By    string `json:"by,omitempty"`
	At    int64  `json:"at"`
}

// WriteCancelRequest 把取消请求落盘(供**执行该任务的进程**轮询处理;跨进程取消的通道)。
func (m *Manager) WriteCancelRequest(id int64, force bool, by string) error {
	if err := os.MkdirAll(m.opt.Dir, 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(cancelRequest{Force: force, By: by, At: time.Now().UnixMilli()})
	if err != nil {
		return err
	}
	if _, err := fsx.AtomicWriteText(m.cancelFilePath(id), string(raw)+"\n"); err != nil {
		return err
	}
	return nil
}

// readCancelRequest 读取消请求(文件不存在/不可解析 = 无请求)。
func (m *Manager) readCancelRequest(id int64) (force bool, ok bool) {
	raw, err := os.ReadFile(m.cancelFilePath(id))
	if err != nil {
		return false, false
	}
	var req cancelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return false, false
	}
	return req.Force, true
}

// clearCancelRequest 任务终态后清掉请求文件(编号不重复,留着只是垃圾)。
func (m *Manager) clearCancelRequest(id int64) {
	if err := os.Remove(m.cancelFilePath(id)); err != nil && !os.IsNotExist(err) {
		m.logf("[job] 清理取消请求文件失败(job %d): %v", id, err)
	}
}

// load 读取 index.json;执行进程已消失的 running/cancelling 任务标记为 failed
// (任务真被中断,不谎报"还在跑";实例状态由 cs status 如实反映)。
func (m *Manager) load() {
	raw, err := os.ReadFile(m.indexPath())
	if err != nil {
		return
	}
	var f indexFile
	if err := json.Unmarshal(raw, &f); err != nil {
		m.logf("[job] index.json 解析失败(忽略): %v", err)
		return
	}
	if f.NextID > m.nextID {
		m.nextID = f.NextID
	}
	changed := false
	for _, j := range f.Jobs {
		if j == nil || j.ID == 0 {
			continue
		}
		j.cancelReq, j.forceReq = false, false
		nonTerminal := j.Status == protocol.JobRunning || j.Status == protocol.JobCancelling || j.Status == protocol.JobQueued
		// 只有**记录的执行进程已不在**才算"被中断":另一个进程(CLI 长任务 / 守护)仍在跑时,
		// 状态归它写。只按状态判定会把并发运行的任务误标 failed —— 任何构造 Manager 的命令
		// (`cs jobs`/`cs job log`/`cs job cancel`/`cs new`…) 都会把它写坏,而守护 Reload 会读到并上报。
		if nonTerminal && !pidAlive(j.PID) {
			j.Status = protocol.JobFailed
			j.Error = "桥进程重启,任务中断"
			j.FinishedAt = time.Now().UnixMilli()
			j.Reported = false
			changed = true
		}
		m.jobs[j.ID] = j
		m.order = append(m.order, j.ID)
		// nextID 必须推过**实际存在的最大号**:平台任务号可能落在 CLI 区段(见 allocIDLocked),
		// 只信文件里的 nextId 会让下一次 CLI 分配撞号。
		if j.ID >= m.nextID {
			m.nextID = j.ID + 1
		}
	}
	sort.Slice(m.order, func(i, b int) bool { return m.order[i] < m.order[b] })
	if changed {
		m.saveLocked()
	}
}

// Reload 重新读取 index.json,合并在**其他进程**(CLI 的 `cs` 命令)里创建/推进的任务。
// 守护每轮收敛上报前调用;本进程正在跑的任务不被覆盖(它的状态在内存里是最新的)。
func (m *Manager) Reload() int {
	raw, err := os.ReadFile(m.indexPath())
	if err != nil {
		return 0
	}
	var f indexFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	added := 0
	for _, j := range f.Jobs {
		if j == nil || j.ID == 0 {
			continue
		}
		j.cancelReq, j.forceReq = false, false
		cur := m.jobs[j.ID]
		if cur == nil {
			m.jobs[j.ID] = j
			m.order = append(m.order, j.ID)
			added++
			continue
		}
		if cur.Status == protocol.JobRunning || cur.Status == protocol.JobCancelling {
			// 只有**本进程发起**的任务才以内存为准;CLI 进程的任务盘上才有真实终态
			// (见 own 字段的注释:否则长任务会被永久钉在 running)。
			if m.own[j.ID] {
				continue
			}
		}
		cur.Status, cur.Step, cur.StepIndex, cur.StepTotal = j.Status, j.Step, j.StepIndex, j.StepTotal
		cur.Progress, cur.Error, cur.Result = j.Progress, j.Error, j.Result
		cur.FinishedAt, cur.Reported, cur.StartedAt = j.FinishedAt, j.Reported, j.StartedAt
	}
	if f.NextID > m.nextID {
		m.nextID = f.NextID
	}
	if added > 0 {
		sort.Slice(m.order, func(i, b int) bool { return m.order[i] < m.order[b] })
	}
	return added
}

// saveLocked 原子写索引(调用方持锁)。只保留最近 50 条任务。
func (m *Manager) saveLocked() {
	ids := append([]int64(nil), m.order...)
	sort.Slice(ids, func(i, b int) bool { return ids[i] > ids[b] }) // 新的在前
	if len(ids) > 50 {
		ids = ids[:50]
	}
	f := indexFile{NextID: m.nextID, Jobs: make([]*Job, 0, len(ids))}
	for _, id := range ids {
		if j := m.jobs[id]; j != nil {
			f.Jobs = append(f.Jobs, j)
		}
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		m.logf("[job] 索引序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(m.opt.Dir, 0o755); err != nil {
		m.logf("[job] 创建任务目录失败: %v", err)
		return
	}
	if _, err := fsx.AtomicWriteText(m.indexPath(), string(raw)); err != nil {
		m.logf("[job] 索引写入失败: %v", err)
	}
}

// allocIDLocked 分配任务编号:平台给定则用平台的(守护里 jobs.id 与桥日志同名),
// 否则用 CLI 区段自增(平台编号是小整数区段,两者不会撞)。
//
// 两个坑(真机 + e2e 实测):
//   - 平台给的号可能**落在 CLI 区段**:平台把 CLI 上报的任务按原号插进 jobs 表后,
//     其 AUTOINCREMENT 序列会超过 1000000,之后面板任务就会拿到 1000001… —— 必须把
//     nextID 推过它,否则下一次 CLI 分配必然撞号(`cs new` 直接 "任务编号已存在")。
//   - 索引文件可能落后于实际任务(多进程交替保存):分配时跳过已占用的号。
func (m *Manager) allocIDLocked(want int64) int64 {
	if want > 0 {
		if want >= m.nextID {
			m.nextID = want + 1
		}
		return want
	}
	for {
		id := m.nextID
		m.nextID++
		if _, taken := m.jobs[id]; !taken {
			return id
		}
	}
}

// ---- 查询 ------------------------------------------------------------------

// Get 返回任务快照(拷贝;调用方可安全读取)。
func (m *Manager) Get(id int64) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copyJob(m.jobs[id])
}

// List 返回全部任务(新 → 旧)。
func (m *Manager) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := append([]int64(nil), m.order...)
	sort.Slice(ids, func(i, b int) bool { return ids[i] > ids[b] })
	out := make([]*Job, 0, len(ids))
	for _, id := range ids {
		if j := copyJob(m.jobs[id]); j != nil {
			out = append(out, j)
		}
	}
	return out
}

// ActiveOfGroup 返回该组正在跑的任务(用于幂等判定与 cs status 展示)。
func (m *Manager) ActiveOfGroup(groupID string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.order {
		j := m.jobs[id]
		if j == nil || j.GroupID != groupID {
			continue
		}
		if j.Status == protocol.JobRunning || j.Status == protocol.JobCancelling || j.Status == protocol.JobQueued {
			return copyJob(j)
		}
	}
	return nil
}

// ActiveGroups 返回"维护中"的组(有进行中任务)→ cs status 展示。
func (m *Manager) ActiveGroups() []protocol.Maintenance {
	m.mu.Lock()
	defer m.mu.Unlock()
	byGroup := map[string]*Job{}
	for _, id := range m.order {
		j := m.jobs[id]
		if j == nil {
			continue
		}
		if j.Status == protocol.JobRunning || j.Status == protocol.JobCancelling || j.Status == protocol.JobQueued {
			if cur := byGroup[j.GroupID]; cur == nil || j.ID > cur.ID {
				byGroup[j.GroupID] = j
			}
		}
	}
	out := make([]protocol.Maintenance, 0, len(byGroup))
	for g, j := range byGroup {
		id := j.ID
		out = append(out, protocol.Maintenance{GroupID: g, Enabled: true, Reason: reasonOf(j), JobID: &id})
	}
	sort.Slice(out, func(i, b int) bool { return out[i].GroupID < out[b].GroupID })
	return out
}

func reasonOf(j *Job) string {
	switch j.Kind {
	case KindGameUpdate:
		return fmt.Sprintf("游戏更新(job %d)", j.ID)
	case KindPluginSync, KindPluginDeploy:
		return fmt.Sprintf("插件同步(job %d)", j.ID)
	case KindDemoCollect:
		return fmt.Sprintf("录像归集(job %d)", j.ID)
	case KindHostCleanup:
		return fmt.Sprintf("主机清理(job %d)", j.ID)
	case KindInstanceCreate:
		return fmt.Sprintf("创建实例(job %d)", j.ID)
	case KindInstanceDelete:
		return fmt.Sprintf("删除实例(job %d)", j.ID)
	default:
		return fmt.Sprintf("任务进行中(job %d)", j.ID)
	}
}

// registryPath 取编号注册表文件路径(Options.RegistryPath 缺省时按 jobs/ 目录推断)。
func (m *Manager) registryPath() string {
	if m.opt.RegistryPath != "" {
		return m.opt.RegistryPath
	}
	return filepath.Join(filepath.Dir(m.opt.Dir), "registry.json")
}

func copyJob(j *Job) *Job {
	if j == nil {
		return nil
	}
	c := *j
	if j.Result != nil {
		c.Result = map[string]any{}
		for k, v := range j.Result {
			c.Result[k] = v
		}
	}
	return &c
}

func (m *Manager) mutate(id int64, fn func(*Job)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil {
		fn(j)
		m.saveLocked()
	}
}
