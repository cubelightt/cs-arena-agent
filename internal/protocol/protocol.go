// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package protocol 定义桥 ↔ 后端之间的 WS 帧。
//
// 字段名大小写敏感,与后端 lib/agentChannel.js 逐字对齐;
// 旧 17 个 op 的语义在 ops 包里实现,本包只管帧形状。
package protocol

// 帧 type(下行:后端 → 桥)
const (
	TypeCmd  = "cmd"
	TypePing = "ping"
	// v2 增补
	TypeHelloAck      = "hello_ack"
	TypeConfigSync    = "config_sync"
	TypeStateSync     = "state_sync"
	TypeArenaMatchAck = "arena_match_ack"
)

// 帧 type(上行:桥 → 后端)
const (
	TypeHello            = "hello"
	TypeResult           = "result"
	TypePush             = "push"
	TypeInstancesReport  = "instances_report" // v2 增补(PROTOCOL-V2 §1.4)
	TypeJobReport        = "job_report"       // v2 增补(PROTOCOL-V2 §1.6;M4)
	TypeArenaMatchResult = "arena_match_result"
)

// push kind(上行)
const (
	KindHealth       = "health"
	KindConsole      = "console"
	KindConsoleReset = "console_reset"
	KindConsoleState = "console_state"
	KindJob          = "job" // v2 增补(PROTOCOL-V2 §2.1;M4 落地)
)

// 能力名。桥只声明**已实现**的能力:
// 后端只对声明了能力的连接发新帧/调新 op,谎报会让后端调用不存在的 op。
const (
	CapConfigSync      = "config_sync"      // 能收下发的权威清单(hello_ack/config_sync)
	CapHostStatus      = "host_status"      // 实现主机级只读盘点 op(M3)
	CapInstancesReport = "instances_report" // 会上报实例清单增删/对账(M3)
	CapStateSync       = "state_sync"       // 能收锁/维护态快照(接收端已就绪,M3 声明)
	CapJobs            = "jobs"             // job 框架:job_* op + push kind job + job_report(M4)
	CapArenaMatchIPC   = "arena_match_ipc"
)

// Hello 上行握手帧。
// ServerID/Instances 为兼容字段:有 state 缓存时带上,后端用它做空组种子与交叉校验;
// v2 的权威来源是后端的 hello_ack。
type Hello struct {
	Type         string          `json:"type"`
	Token        string          `json:"token"`
	ServerID     string          `json:"serverId,omitempty"`
	Instances    []string        `json:"instances"`
	AgentVersion string          `json:"agentVersion,omitempty"`
	Capabilities []string        `json:"capabilities,omitempty"`
	Registry     []RegistryEntry `json:"registry,omitempty"`
}

// RegistryEntry 桥侧编号注册表(M2 由 cs new 维护;M0 起随 state.json 持久化,便于平台镜像)。
type RegistryEntry struct {
	Idx  int    `json:"idx"`
	Name string `json:"name"`
	Port int    `json:"port"`
}

// Inbound 是所有下行帧的并集结构:按 type 取用相关字段,缺失字段为各类型零值。
// 之所以不做成接口 + 多态解析,是因为后端帧字段少、且需要容忍未知 type(静默忽略)。
type Inbound struct {
	Type string `json:"type"`

	// cmd
	CmdID     string         `json:"cmdId"`
	Op        string         `json:"op"`
	Instance  string         `json:"instance"`
	Payload   map[string]any `json:"payload"`
	MatchID   int64          `json:"matchId"`   // arena_match_ack
	SHA256    string         `json:"sha256"`    // arena_match_ack
	ResultSeq int            `json:"resultSeq"` // arena_match_ack

	// ping
	At int64 `json:"at"`

	// hello_ack / config_sync
	ServerID      string         `json:"serverId"`
	Instances     []string       `json:"instances"`
	InstanceMeta  []InstanceMeta `json:"instanceMeta"`
	Maintenance   []Maintenance  `json:"maintenance"`
	ConfigVersion int64          `json:"configVersion"`
	DemoDir       string         `json:"demoDir"`

	// state_sync
	Locks map[string]LockState `json:"locks"`
}

// InstanceMeta 平台侧实例元数据(展示用;桥不改业务判断)。
type InstanceMeta struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	GotvPort   int    `json:"gotvPort"`
	Idx        int    `json:"idx"`
	AdminOnly  bool   `json:"adminOnly"`
	BotCapable bool   `json:"botCapable"`
	State      string `json:"state"`
	MatchID    *int64 `json:"matchId"`
}

// Maintenance 按服务器组的维护态。
type Maintenance struct {
	GroupID string `json:"groupId"`
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
	JobID   *int64 `json:"jobId"`
}

// LockState 平台侧的实例锁快照(state_sync;桥仅缓存展示)。
type LockState struct {
	State   string `json:"state"`
	MatchID *int64 `json:"matchId"`
}

// ResultFrame 上行命令回包:data 与 op 的整个回包对象同体,ok 取自 data.ok(缺省 true)。
type ResultFrame struct {
	Type  string         `json:"type"`
	CmdID string         `json:"cmdId"`
	OK    bool           `json:"ok"`
	Data  map[string]any `json:"data"`
}

// HealthPush 上行健康推送:instances 为 name → 状态串全量映射。
type HealthPush struct {
	Type      string            `json:"type"`
	Kind      string            `json:"kind"`
	Instances map[string]string `json:"instances"`
}

// ConsolePush 控制台增量行(offset 为下次读取起点)。
type ConsolePush struct {
	Type     string   `json:"type"`
	Kind     string   `json:"kind"`
	Instance string   `json:"instance"`
	Path     string   `json:"path"`
	Offset   int64    `json:"offset"`
	Lines    []string `json:"lines"`
}

// ConsoleResetPush 日志文件轮换(offset 归零前先发)。
type ConsoleResetPush struct {
	Type     string `json:"type"`
	Kind     string `json:"kind"`
	Instance string `json:"instance"`
}

// ConsoleStatePush 实例 running 状态变化(仅变化时发)。
type ConsoleStatePush struct {
	Type     string `json:"type"`
	Kind     string `json:"kind"`
	Instance string `json:"instance"`
	Running  bool   `json:"running"`
}

// InstancesReport 是上行实例清单上报:
// origin=cli(cs new/del 后即时) / reconcile(每次连上的全量对账)。
type InstancesReport struct {
	Type      string              `json:"type"`
	Origin    string              `json:"origin"`
	Instances []InstanceReportRow `json:"instances"`
}

// InstanceReportRow 一行增删/对账(action 仅 cli 上报时有意义)。
type InstanceReportRow struct {
	Idx      int    `json:"idx"`
	Name     string `json:"name"`
	Port     int    `json:"port"`
	GotvPort int    `json:"gotvPort"`
	Action   string `json:"action,omitempty"`
}

// ---- job 框架--------------------------------

// 任务状态(与后端 jobs.status 取值逐字一致)。
const (
	JobQueued     = "queued"
	JobRunning    = "running"
	JobCancelling = "cancelling"
	JobDone       = "done"
	JobFailed     = "failed"
	JobCancelled  = "cancelled"
)

// JobReport 上行任务上报(离线/主机侧发起的任务重连后收敛;平台侧权威记录)。
type JobReport struct {
	Type string       `json:"type"`
	Job  JobReportRow `json:"job"`
}

// JobReportRow 是 job_report 的 job 字段(与 jobs 表列一一对应)。
type JobReportRow struct {
	JobID      int64  `json:"jobId"`
	Kind       string `json:"kind"`
	GroupID    string `json:"groupId"`
	Origin     string `json:"origin"` // platform | cli
	Status     string `json:"status"`
	Step       string `json:"step,omitempty"`
	StepIndex  int    `json:"stepIndex,omitempty"`
	StepTotal  int    `json:"stepTotal,omitempty"`
	Progress   int    `json:"progress,omitempty"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	Error      string `json:"error,omitempty"`
	CLIUser    string `json:"cliUser,omitempty"`
	// Instance 是任务作用的实例名(建删实例任务)—— 平台写 jobs.instance_name,面板任务列表显示实例列。
	Instance string `json:"instance,omitempty"`
	// Result 只随**终态**上报(建删实例给 port/idx/gotvPort/freedBytes 等,平台免回源);
	// 进行中的帧不带,避免每轮重复大对象。
	Result map[string]any `json:"result,omitempty"`
}

// JobPush 是 push kind:'job':状态变化与增量日志行。
// Line 可缺省(仅状态变化时发);增量行同时落盘 <config_dir>/jobs/<jobId>.log,可经 job_log 补拉。
type JobPush struct {
	Type      string `json:"type"`
	Kind      string `json:"kind"`
	JobID     int64  `json:"jobId"`
	Status    string `json:"status"`
	Step      string `json:"step,omitempty"`
	StepIndex int    `json:"stepIndex,omitempty"`
	StepTotal int    `json:"stepTotal,omitempty"`
	Progress  int    `json:"progress,omitempty"`
	Line      string `json:"line,omitempty"`
	// Result 只在**终态帧**携带(M5:建删实例的 port/idx/freedBytes 等 —— 平台无需再 job_status 回源;
	// 进行中的帧不带,避免每帧重复大对象)
	Result map[string]any `json:"result,omitempty"`
}
