// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package ops 是桥的命令分派层:实例白名单、回包形状、异常收口。
//
// 分派规则:
//   - 回包 `ok` 一律由 msm returncode 决定 —— v1 曾因"缺省 true"把失败吞成成功;
//   - 任何 handler 异常(含 panic)都收口成 {ok:false,error},绝不冒泡断 WS;
//   - 实例白名单只对 INSTANCE_OPS 生效;probe/locate/ps/health 是主机级、不校验实例名。
package ops

import (
	"errors"
	"fmt"
	"sync"

	"arena/agent/internal/config"
	"arena/agent/internal/job"
	"arena/agent/internal/matchipc"
	"arena/agent/internal/msm"
	"arena/agent/internal/protocol"
	"arena/agent/internal/registry"
)

// InstanceOps 需要实例白名单校验的 op(与 v1 INSTANCE_OPS 逐项一致)。
var InstanceOps = map[string]bool{
	"status": true, "start": true, "stop": true, "restart": true, "send": true,
	"console": true, "matchfile": true, "log": true, "matchcleanup": true,
	"workshop_status": true, "mapfile_status": true,
	"console_subscribe": true, "console_unsubscribe": true,
	"arena_match_bind": true, "arena_match_close": true,
}

// AllOps 返回桥支持的操作名。
var AllOps = []string{
	"status", "start", "stop", "restart", "send", "console", "matchfile", "log",
	"probe", "locate", "ps", "health", "workshop_status", "mapfile_status",
	"matchcleanup", "console_subscribe", "console_unsubscribe",
	// v2 增补:主机级只读盘点
	"host_status", "instances_list",
	// v2 增补:job 框架与按组批量启停
	"start_all", "stop_all", "restart_all",
	"job_start", "job_status", "job_log", "job_cancel", "job_subscribe", "job_unsubscribe",
	"arena_match_bind", "arena_match_close",
}

// Ops 持有分派所需的依赖。
type Ops struct {
	Runner *msm.Runner
	Paths  *msm.Paths
	// Names 返回运行时实例白名单(来源:后端 hello_ack → 本地注册表 → state.json 缓存)。
	Names func() []string
	// ArchiveOverride 覆盖归档根(为空则用 ~/arena-data;配置 archive_dir 的落点)。
	ArchiveOverride string
	// Hub 是控制台订阅/tail 状态(进程级;由 cli 构造后注入)。
	Hub *ConsoleHub
	// Jobs 是任务框架(M4;由 cli 构造后注入)。
	Jobs *job.Manager
	// MatchIPC is the per-instance local ArenaMatch delivery service.
	MatchIPC  *matchipc.Server
	startEnvs sync.Map // 本进程收到的实例启动参数，插件模式切换时保留 MAXPLAYERS。

	// host ops依赖:配置/版本/注册表/能力/维护态/清单来源,由 cli 注入
	CfgFn    func() *config.Config
	Version  string
	Registry func() []registry.Instance
	// RegistryRemoved 返回本机已删除实例名(墓碑;守护上报 action:'removed' 用)
	RegistryRemoved func() []string
	Caps            func() []string
	MaintFn         func() []protocol.Maintenance
	FromBackend     func() bool
}

// Allowed 判断实例名是否在白名单内。
func (o *Ops) Allowed(instance string) bool {
	for _, n := range o.Names() {
		if n == instance {
			return true
		}
	}
	return false
}

// Reply 把 msm 结果转成回包(r == nil 表示只读路径拿不到锁)。
func Reply(r *msm.Result, extra map[string]any) map[string]any {
	if r == nil {
		return merge(map[string]any{
			"ok":    false,
			"error": "msm busy(实例正被其他命令占用),请重试",
		}, extra)
	}
	return merge(map[string]any{
		"ok":         r.Returncode == 0,
		"stdout":     r.Stdout,
		"stderr":     r.Stderr,
		"returncode": r.Returncode,
	}, extra)
}

// ReplyBusy 是 Reply(nil) 的直给形式(只读路径回退用)。
func ReplyBusy(extra map[string]any) map[string]any {
	return Reply(nil, extra)
}

func merge(dst, src map[string]any) map[string]any {
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// Handle 执行一条命令,恒返回 dict。
// instance 已由调用方按 `frame.instance ?? payload.instance` 解析。
func (o *Ops) Handle(op, instance string, payload map[string]any) map[string]any {
	return o.handle(op, instance, payload)
}

// handle 里 recover 是安全网:一条畸形入参/一次 msm 超时都不能拖断整条 WS 通道。
func (o *Ops) handle(op, instance string, payload map[string]any) (out map[string]any) {
	defer func() {
		if rec := recover(); rec != nil {
			out = map[string]any{"ok": false, "error": fmt.Sprintf("panic: %v", rec), "op": op}
		}
	}()

	if payload == nil {
		payload = map[string]any{}
	}
	if instance == "" {
		if v, ok := payload["instance"].(string); ok {
			instance = v
		}
	}

	if InstanceOps[op] && !o.Allowed(instance) {
		return map[string]any{"ok": false, "error": "instance not allowed: " + instance}
	}

	switch op {
	case "status", "start", "stop", "restart":
		return o.doLifecycle(op, instance, payload)
	case "send":
		// cmd 缺省为空串、无单行/长度校验(与 v1 一致);非字符串属契约外,退化为空串
		res, err := o.Runner.Run(instance, "send", []string{stringField(payload, "cmd")}, nil, true)
		return o.replyOrError(res, err, op, map[string]any{"instance": instance})
	case "console":
		return o.handleConsole(instance, payload)
	case "matchfile":
		return o.handleMatchfile(instance, payload)
	case "arena_match_bind":
		if o.MatchIPC == nil {
			return map[string]any{"ok": false, "error": "arena match IPC unavailable"}
		}
		id, valid := matchipc.NumberID(payload["matchId"])
		if !valid {
			return map[string]any{"ok": false, "error": "invalid match ID"}
		}
		input, valid := payload["json"].(map[string]any)
		if !valid {
			return map[string]any{"ok": false, "error": "json must be an object"}
		}
		binding, err := o.MatchIPC.Store.Bind(instance, id, input)
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}
		}
		if binding.Status == "bound" {
			if err := o.prepareMatchPlugins(instance); err != nil {
				return map[string]any{"ok": false, "error": err.Error()}
			}
		}
		return map[string]any{"ok": true, "instance": instance, "matchId": id,
			"sha256": binding.SHA256, "status": binding.Status}
	case "arena_match_close":
		if o.MatchIPC == nil {
			return map[string]any{"ok": false, "error": "arena match IPC unavailable"}
		}
		id, valid := matchipc.NumberID(payload["matchId"])
		if !valid {
			return map[string]any{"ok": false, "error": "invalid match ID"}
		}
		binding, err := o.MatchIPC.Store.Close(instance, id)
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}
		}
		return map[string]any{"ok": true, "instance": instance, "matchId": id, "status": binding.Status}
	case "log":
		return o.handleLog(instance, payload)
	case "probe":
		return o.handleProbe(payload)
	case "locate":
		return o.handleLocate()
	case "ps":
		return o.handlePS()
	case "workshop_status":
		return o.handleWorkshopStatus(instance, payload)
	case "mapfile_status":
		return o.handleMapfileStatus(instance, payload)
	case "matchcleanup":
		keep := []string{}
		if raw, ok := payload["keepMatchIds"].([]any); ok {
			for _, v := range raw {
				keep = append(keep, msm.ScalarString(v))
			}
		}
		matchID := msm.ScalarString(payload["matchId"])
		return o.matchCleanup(instance, matchID, boolField(payload, "all"), keep)
	case "health":
		return o.doHealth()
	case "host_status":
		return o.handleHostStatus(payload)
	case "instances_list":
		return o.handleInstancesList()
	case "console_subscribe":
		if o.Hub == nil {
			return notImplemented(op)
		}
		return o.Hub.Subscribe(instance)
	case "console_unsubscribe":
		if o.Hub == nil {
			return notImplemented(op)
		}
		return o.Hub.Unsubscribe(instance)
	case "start_all", "stop_all", "restart_all":
		return o.handleBulk(op, payload)
	case "job_start":
		return o.handleJobStart(payload)
	case "job_status":
		return o.handleJobStatus(payload)
	case "job_log":
		return o.handleJobLog(payload)
	case "job_cancel":
		return o.handleJobCancel(payload)
	case "job_subscribe":
		return o.handleJobSubscribe(payload)
	case "job_unsubscribe":
		return o.handleJobUnsubscribe(payload)
	default:
		return map[string]any{"ok": false, "error": "unknown op: " + op}
	}
}

func boolField(payload map[string]any, key string) bool {
	v, ok := payload[key].(bool)
	return ok && v
}

// doLifecycle 处理 status/start/stop/restart(env 只对 start/restart 生效)。
func (o *Ops) doLifecycle(op, instance string, payload map[string]any) map[string]any {
	env, envErr := startEnv(op, payload)
	if envErr != "" {
		return map[string]any{"ok": false, "error": envErr}
	}
	res, err := o.Runner.Run(instance, op, nil, env, true)
	if err == nil && res != nil && res.Returncode == 0 && (op == "start" || op == "restart") {
		o.startEnvs.Store(instance, env)
	}
	extra := map[string]any{"instance": instance}
	if op == "status" {
		st := ""
		if res != nil {
			st = msm.ParseState(res.Stdout)
		} else {
			st = "UNKNOWN"
		}
		extra["state"] = st
		// 只缓存三个确定状态(UNKNOWN/ERROR 不入缓存,与 v1 一致)
		if st == "RUNNING" || st == "BOOTING" || st == "STOPPED" {
			o.Runner.NoteState(instance, st)
		}
	}
	return o.replyOrError(res, err, op, extra)
}

// doHealth 遍历白名单全量取状态(走缓存路径,避免每 5s 对每个实例 fork msm)。
func (o *Ops) doHealth() map[string]any {
	out := map[string]any{}
	instances := map[string]string{}
	for _, n := range o.Names() {
		instances[n] = o.Runner.Status(n, true)
	}
	out["ok"] = true
	out["instances"] = instances
	return out
}

// replyOrError 统一把 msm 层错误转成回包形态(超时文案与 v1 一致)。
func (o *Ops) replyOrError(res *msm.Result, err error, op string, extra map[string]any) map[string]any {
	if err == nil {
		return Reply(res, extra)
	}
	var te *msm.TimeoutError
	if errors.As(err, &te) {
		return merge(map[string]any{"ok": false, "error": te.Error(), "op": op}, extra)
	}
	if errors.Is(err, msm.ErrBusy) {
		return Reply(nil, extra)
	}
	// 起不来(可执行不存在/权限等):Python 版这里是 "<ExcType>: <msg>",Go 版给出可读文案
	return merge(map[string]any{"ok": false, "error": err.Error(), "op": op}, extra)
}

// startEnv 校验启动项覆盖:只有 start/restart 接受 env(与 v1 start_env 一致)。
func startEnv(op string, payload map[string]any) (map[string]string, string) {
	if op != "start" && op != "restart" {
		return map[string]string{}, ""
	}
	raw, ok := payload["env"]
	if !ok || raw == nil {
		return map[string]string{}, ""
	}
	m, isMap := raw.(map[string]any)
	if !isMap {
		return nil, fmt.Sprintf("env must be an object with at most %d keys", msm.MaxEnvKeys)
	}
	return msm.SanitizeEnv(m)
}

func isKnown(op string) bool {
	for _, k := range AllOps {
		if k == op {
			return true
		}
	}
	return false
}

func notImplemented(op string) map[string]any {
	return map[string]any{"ok": false, "error": "op not implemented yet: " + op, "op": op}
}

// int64Field 从 payload 取整数(JSON 数值都是 float64)。
func int64Field(payload map[string]any, key string) int64 {
	v, ok := payload[key]
	if !ok {
		return 0
	}
	return int64From(v)
}

func int64From(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case string:
		var n int64
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n
		}
	}
	return 0
}

func stringField(payload map[string]any, key string) string {
	if v, ok := payload[key].(string); ok {
		return v
	}
	return ""
}
