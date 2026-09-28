// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package agent 是桥的运行时:反向 WS 连接、握手与能力协商、保活、命令 worker。
//
// 时序与失败语义照抄 v1:
//   - 命令由**唯一 worker** 串行执行(FIFO),读循环不阻塞 —— 长命令期间 health 照常推;
//   - 建连后立即推一次 health,之后每 health_push_ms 一次(后端 10s ping / 30s stale 判定依赖它);
//   - 断线时丢弃未执行命令、**不杀**正在跑的子进程;重连后不补发旧命令(离线队列是 M3 的 job/上报机制)。
//
// 实现注记(踩坑):gorilla/websocket 的**读错误是永久的** —— 一旦 ReadMessage 返回
// 读超时,后续调用只会重复返回同一错误(累计 1000 次即 panic)。因此这里不设读超时、
// 也不在错误后再读:单一读协程 + 心跳/空闲自检两个 ticker,半开连接靠空闲自检主动断开。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"arena/agent/internal/config"
	"arena/agent/internal/matchipc"
	"arena/agent/internal/ops"
	"arena/agent/internal/protocol"
	"arena/agent/internal/registry"
)

// Options 是构造 Agent 所需的一切。
type Options struct {
	Cfg     *config.Store
	Ops     *ops.Ops
	State   *State
	Status  *StatusWriter // 心跳(CLI 的 cs status/cs doctor 读它)
	Version string
	Logger  *log.Logger
}
type Agent struct {
	opt Options

	sendMu sync.Mutex
	connMu sync.Mutex
	conn   *websocket.Conn // 当前连接(上报帧用;断线后置 nil)
}

// currentConn 返回当前连接(nil = 未连接)。
func (a *Agent) currentConn() *websocket.Conn {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.conn
}

func (a *Agent) setConn(conn *websocket.Conn) {
	a.connMu.Lock()
	a.conn = conn
	a.connMu.Unlock()
}

func New(opt Options) *Agent {
	if opt.Logger == nil {
		opt.Logger = log.Default()
	}
	return &Agent{opt: opt}
}

func (a *Agent) logf(format string, args ...any) {
	a.opt.Logger.Printf(format, args...)
}

// Run 是守护主循环:连 → 跑 → 断开退避重连(1→2→4…封顶 30s,连上即重置)。
// ctx 取消即退出(SIGTERM 走这条路径,退出码 0)。
func (a *Agent) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return nil
		}
		connected, err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		a.noteConnected(false, len(a.opt.State.Names()), a.instancesSource())
		if connected {
			backoff = time.Second
		}
		if err != nil {
			a.logf("[bridge] reverse error: %v", err)
		}
		a.logf("[bridge] reverse reconnect in %s", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// session 建立一次连接并服务到断开;返回 (是否完成过握手, 错误)。
func (a *Agent) session(ctx context.Context) (bool, error) {
	cfg := a.opt.Cfg.Get()
	st := a.opt.State

	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	conn, _, err := dialer.DialContext(ctx, cfg.BackendWS, nil)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	a.setConn(conn)
	defer a.setConn(nil)

	// hello 必须是建连后的第一帧(后端 10s 内收不到就 close(1008))
	snap := st.Snapshot()
	hello := protocol.Hello{
		Type:         protocol.TypeHello,
		Token:        cfg.Token,
		ServerID:     snap.ServerID,
		Instances:    normalizeNames(snap.Instances),
		AgentVersion: a.opt.Version,
		Capabilities: Capabilities,
		Registry:     registryEntries(a.registryItems()),
	}
	if err := a.sendJSON(conn, hello); err != nil {
		return false, err
	}
	a.noteConnected(true, len(hello.Instances), a.instancesSource())
	// 清单对账上报:每次连上都发一次全量,平台按需收敛(旧后端忽略该帧)
	a.reportInstances("reconcile")
	who := snap.ServerID
	if who == "" {
		who = "(待 hello_ack 下发)"
	}
	a.logf("[bridge] reverse connected to %s (server %s)", cfg.BackendWS, who)

	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	q := newQueue()
	go func() {
		<-sessCtx.Done()
		q.Close()
	}()
	go a.worker(sessCtx, conn, q)

	// 控制台 tail:把推送挂到本连接上(订阅与 offset 是进程级状态,断线重连后继续续读)
	if a.opt.Ops.Hub != nil {
		a.opt.Ops.Hub.SetPush(func(v any) error { return a.sendJSON(conn, v) })
		defer a.opt.Ops.Hub.ClearPush()
	}
	// 任务推送同理:断线期间任务照跑(状态落盘),重连后由 ReportPending 收敛
	if a.opt.Ops.Jobs != nil {
		a.opt.Ops.Jobs.SetPush(func(v any) error { return a.sendJSON(conn, v) })
		defer a.opt.Ops.Jobs.ClearPush()
	}

	var lastInbound atomic.Int64
	lastInbound.Store(time.Now().UnixNano())

	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.ReadMessage() // 单一读方;出错即退出,绝不再读
			if err != nil {
				readErr <- err
				return
			}
			lastInbound.Store(time.Now().UnixNano())
			a.handleFrame(conn, data, q)
		}
	}()

	health := time.NewTicker(time.Duration(cfg.HealthPushMS) * time.Millisecond)
	defer health.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	tail := time.NewTicker(time.Duration(cfg.ConsoleTailMS) * time.Millisecond)
	defer tail.Stop()

	// 建连立即推一次 health(否则后端 30s stale 判定可能在首个周期前就超时)
	a.pushHealth(conn)
	// 任务收敛上报:CLI 发起/断线期间跑的任务在这里补报给平台
	a.reportJobs()
	a.reportArenaMatchResults()

	for {
		select {
		case <-ctx.Done():
			return true, nil
		case err := <-readErr:
			return true, err
		case <-health.C:
			a.pushHealth(conn)
			// 兜底收敛:CLI 进程发起/结束的任务没有连接,由守护周期补报(幂等)
			a.reportJobs()
			a.reportArenaMatchResults()
			// 实例清单收敛:`cs new`/`cs del` 是 CLI 进程,改动靠守护周期全量对账上报
			// (origin=reconcile;平台按 name 幂等收敛 —— 这就是实例维度的"待上报队列")
			a.reportInstances("reconcile")
		case <-tail.C:
			// 控制台 tail:读已订阅实例的日志增量并推送(异常只影响本次 tick)
			if a.opt.Ops.Hub != nil {
				a.opt.Ops.Hub.Tick()
			}
		case <-tick.C:
			a.syncMaintenanceStatus()
			// 空闲自检:超过 idle_timeout_s 没收到后端任何数据帧(含 ping)即主动断开重连
			idle := cfg.IdleTimeoutS
			if idle > 0 && time.Since(time.Unix(0, lastInbound.Load())) > time.Duration(idle)*time.Second {
				_ = conn.Close()
				return true, fmt.Errorf("idle timeout(%ds): 未收到后端任何数据帧,主动重连", idle)
			}
		}
	}
}

// handleFrame 处理一条下行帧(在唯一读协程里调用,处理必须快:慢活交给 worker)。
func (a *Agent) handleFrame(conn *websocket.Conn, data []byte, q *queue) {
	var in protocol.Inbound
	if err := json.Unmarshal(data, &in); err != nil {
		// 解析失败不算"收到帧"(后端 LAST_SEEN 同款语义),也不断连
		return
	}

	a.opt.Status.touchFrame()
	switch in.Type {
	case protocol.TypeCmd:
		instance := in.Instance
		if instance == "" && in.Payload != nil {
			if v, ok := in.Payload["instance"].(string); ok {
				instance = v
			}
		}
		q.Push(cmdItem{CmdID: in.CmdID, Op: in.Op, Instance: instance, Payload: in.Payload})

	case protocol.TypePing:
		// 保活:收到即刷新 lastInbound,不回 pong(后端只要求"有帧")

	case protocol.TypeHelloAck:
		a.opt.State.ApplyHelloAck(&in)
		if err := a.opt.State.Save(); err != nil {
			a.logf("[bridge] state save failed: %v", err)
		}
		a.logf("[bridge] hello_ack: %d 个实例(server %s, configVersion %d)",
			len(in.Instances), in.ServerID, in.ConfigVersion)
		a.noteConnected(true, len(a.opt.State.Names()), "backend")
		// 白名单刚变 → 立即补一次 health(否则首个周期前后端只能看到空清单)
		a.pushHealth(conn)

	case protocol.TypeConfigSync:
		// 后端只在桥声明 config_sync 能力后才发;清单/元数据整体替换,白名单立即生效
		a.opt.State.ApplyHelloAck(&in)
		if err := a.opt.State.Save(); err != nil {
			a.logf("[bridge] state save failed: %v", err)
		}
		a.logf("[bridge] config_sync: %d 个实例(configVersion %d)", len(in.Instances), in.ConfigVersion)
		a.pushHealth(conn)

	case protocol.TypeStateSync:
		a.opt.State.ApplyStateSync(&in)
		if err := a.opt.State.Save(); err != nil {
			a.logf("[bridge] state save failed: %v", err)
		}

	case protocol.TypeArenaMatchAck:
		if a.opt.Ops.MatchIPC != nil {
			if err := a.opt.Ops.MatchIPC.Store.AckResult(in.Instance, in.MatchID, in.SHA256, in.ResultSeq); err != nil {
				a.logf("[bridge] ArenaMatch ack rejected %s/%d: %v", in.Instance, in.MatchID, err)
			}
		}

	default:
		// 未知 type 静默忽略(不能断连 —— 后端可能先行上线新帧)
	}
}

// syncMaintenanceStatus 把本地进行中的任务写进心跳:cs status 与写操作门禁据此判定"维护中"。
func (a *Agent) syncMaintenanceStatus() {
	if a.opt.Status == nil || a.opt.Ops.Jobs == nil {
		return
	}
	active := a.opt.Ops.Jobs.ActiveGroups()
	group := ""
	if len(active) > 0 {
		group = active[0].GroupID
	}
	a.opt.Status.Update(func(st *RuntimeStatus) { st.MaintenanceGroup = group })
}

// reportJobs 把未上报的任务收敛给平台(CLI 发起、断线期间跑的任务;进行中的每次刷新快照)。
func (a *Agent) reportJobs() {
	if a.opt.Ops.Jobs == nil {
		return
	}
	if n := a.opt.Ops.Jobs.ReportPending(); n > 0 {
		a.logf("[bridge] job_report 上报 %d 个任务", n)
	}
}

// SendArenaMatchResult immediately offers a load result to the backend. The
// per-instance metadata remains pending until arena_match_ack is received.
func (a *Agent) SendArenaMatchResult(result matchipc.LoadResult) error {
	return a.sendJSONCurrent(struct {
		Type string `json:"type"`
		matchipc.LoadResult
	}{Type: protocol.TypeArenaMatchResult, LoadResult: result})
}

func (a *Agent) reportArenaMatchResults() {
	if a.opt.Ops.MatchIPC == nil {
		return
	}
	for _, result := range a.opt.Ops.MatchIPC.Store.PendingResults() {
		if err := a.SendArenaMatchResult(result); err != nil {
			a.logf("[bridge] ArenaMatch pending result %s/%d: %v", result.Instance, result.MatchID, err)
			return
		}
	}
}

// pushHealth 推送全量实例健康(走状态缓存,避免每个 tick 都 fork msm)。
func (a *Agent) pushHealth(conn *websocket.Conn) {
	instances := map[string]string{}
	for _, n := range a.opt.Ops.Names() {
		instances[n] = a.opt.Ops.Runner.Status(n, true)
	}
	if err := a.sendJSON(conn, protocol.HealthPush{
		Type:      protocol.TypePush,
		Kind:      protocol.KindHealth,
		Instances: instances,
	}); err != nil {
		a.logf("[bridge] health push failed: %v", err)
	}
}

// worker 串行执行命令(FIFO)。命令异常已由 ops 层收口,这里只负责回帧。
func (a *Agent) worker(ctx context.Context, conn *websocket.Conn, q *queue) {
	for {
		item, ok := q.Pop()
		if !ok {
			return
		}
		data := a.opt.Ops.Handle(item.Op, item.Instance, item.Payload)
		if errStr, _ := data["error"].(string); len(errStr) > 7 && errStr[:7] == "panic: " {
			a.logf("[bridge] dispatch error op=%s instance=%s: %s", item.Op, item.Instance, errStr)
		}
		okFlag := true
		if v, exists := data["ok"].(bool); exists {
			okFlag = v
		}
		frame := protocol.ResultFrame{Type: protocol.TypeResult, CmdID: item.CmdID, OK: okFlag, Data: data}
		if err := a.sendJSON(conn, frame); err != nil {
			a.logf("[bridge] result send failed (channel gone?): %v", err)
			return
		}
	}
}

// sendJSONCurrent 用当前连接发送(上报帧:可能发生在结果回包之外的时机)。
func (a *Agent) sendJSONCurrent(v any) error {
	conn := a.currentConn()
	if conn == nil {
		return fmt.Errorf("未连接")
	}
	return a.sendJSON(conn, v)
}

// sendJSON 串行化 WS 发送(推送与 result 可能并发)。
func (a *Agent) sendJSON(conn *websocket.Conn, v any) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	return conn.WriteJSON(v)
}

// cmdItem 是一条待执行命令。
type cmdItem struct {
	CmdID    string
	Op       string
	Instance string
	Payload  map[string]any
}

// queue 是无界 FIFO(读循环永不因队满阻塞)+ 关闭语义(断线丢弃未执行命令)。
type queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []cmdItem
	closed bool
}

func newQueue() *queue {
	q := &queue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *queue) Push(it cmdItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.items = append(q.items, it)
	q.cond.Signal()
}

func (q *queue) Pop() (cmdItem, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return cmdItem{}, false
	}
	it := q.items[0]
	q.items = q.items[1:]
	return it, true
}

func (q *queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.items = nil
	q.mu.Unlock()
	q.cond.Broadcast()
}

// Capabilities 是 v2 当前**已实现**的能力清单(只声明实现的,谎报会让后端调用不存在的 op)。
var Capabilities = []string{
	protocol.CapConfigSync,
	protocol.CapHostStatus,
	protocol.CapInstancesReport,
	protocol.CapJobs,
	protocol.CapArenaMatchIPC,
}

// registryItems 取本机编号注册表(扫描 cfg/inst-* + registry.json 已落盘编号;由 cli 注入)。
// M5 起编号持久化在 <config_dir>/registry.json,不再放 state.json(守护整份重写 state.json 会覆盖 CLI 的写入)。
func (a *Agent) registryItems() []registry.Instance {
	if a.opt.Ops == nil || a.opt.Ops.Registry == nil {
		return nil
	}
	return a.opt.Ops.Registry()
}

// reportInstances 发送实例清单上报(pending 队列 + 每次连接的 reconcile 全量)。
func (a *Agent) reportInstances(origin string) {
	items := a.registryItems()
	if len(items) == 0 {
		return
	}
	rows := make([]protocol.InstanceReportRow, 0, len(items))
	for _, it := range items {
		rows = append(rows, protocol.InstanceReportRow{Idx: it.Idx, Name: it.Name, Port: it.Port, GotvPort: it.GotvPort, Action: "added"})
	}
	// 主机侧 `cs del` 的墓碑:带 action:'removed'(平台幂等收敛:行已在则删,不在则无操作)
	if a.opt.Ops != nil && a.opt.Ops.RegistryRemoved != nil {
		for _, name := range a.opt.Ops.RegistryRemoved() {
			rows = append(rows, protocol.InstanceReportRow{Name: name, Action: "removed"})
		}
	}
	frame := protocol.InstancesReport{Type: protocol.TypeInstancesReport, Origin: origin, Instances: rows}
	if err := a.sendJSONCurrent(frame); err != nil {
		a.logf("[bridge] instances_report(%s) 发送失败: %v", origin, err)
	}
}

// registryEntries 把桥侧注册表转成 hello 的协议形状。
func registryEntries(items []registry.Instance) []protocol.RegistryEntry {
	if len(items) == 0 {
		return nil
	}
	out := make([]protocol.RegistryEntry, 0, len(items))
	for _, it := range items {
		out = append(out, protocol.RegistryEntry{Idx: it.Idx, Name: it.Name, Port: it.Port})
	}
	return out
}

// instancesSource 报告清单来源(能力降级可读:backend / cache / none)。
func (a *Agent) instancesSource() string {
	snap := a.opt.State.Snapshot()
	if snap.FromBackend {
		return "backend"
	}
	if len(snap.Instances) > 0 {
		return "cache"
	}
	return "none"
}

func (a *Agent) noteConnected(connected bool, count int, source string) {
	if a.opt.Status == nil {
		return
	}
	a.opt.Status.Update(func(st *RuntimeStatus) {
		st.Connected = connected
		st.InstanceCount = count
		if source != "" {
			st.InstancesSource = source
		}
		if connected {
			st.ConnectedAt = time.Now().UnixMilli()
			st.LastFrameAt = st.ConnectedAt
		} else {
			st.Reconnects++
		}
	})
}
