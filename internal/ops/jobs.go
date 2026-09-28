// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// job 框架的 op 层:job_start / job_status / job_log / job_cancel / job_subscribe(job_unsubscribe)
// 与按组批量启停 start_all / stop_all / restart_all。
package ops

import (
	"fmt"

	"arena/agent/internal/job"
	"arena/agent/internal/protocol"
)

// handleJobStart 起一个任务(平台触发路径;CLI 直接调 job.Manager)。
func (o *Ops) handleJobStart(payload map[string]any) map[string]any {
	if o.Jobs == nil {
		return notImplemented("job_start")
	}
	params, _ := payload["params"].(map[string]any)
	spec := job.Spec{
		Kind:    stringField(payload, "kind"),
		Params:  params,
		Origin:  job.OriginPlatform,
		JobID:   int64Field(payload, "jobId"),
		Confirm: stringField(payload, "confirm"),
	}
	j, err := o.Jobs.Start(spec)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	return map[string]any{"ok": true, "jobId": j.ID, "kind": j.Kind, "status": j.Status}
}

// handleJobStatus 查任务状态。
func (o *Ops) handleJobStatus(payload map[string]any) map[string]any {
	if o.Jobs == nil {
		return notImplemented("job_status")
	}
	id := int64Field(payload, "jobId")
	j := o.Jobs.Get(id)
	if j == nil {
		return map[string]any{"ok": false, "error": fmt.Sprintf("任务不存在(job %d)", id)}
	}
	return map[string]any{"ok": true, "job": jobView(j)}
}

// handleJobLog 按 offset 拉任务日志增量(offset 语义与 log op 一致)。
func (o *Ops) handleJobLog(payload map[string]any) map[string]any {
	if o.Jobs == nil {
		return notImplemented("job_log")
	}
	id := int64Field(payload, "jobId")
	if o.Jobs.Get(id) == nil {
		return map[string]any{"ok": false, "error": fmt.Sprintf("任务不存在(job %d)", id)}
	}
	if offset, ok := payload["offset"]; ok && offset != nil {
		lines, next, err := o.Jobs.LogSince(id, int64From(offset))
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}
		}
		return map[string]any{"ok": true, "jobId": id, "offset": next, "lines": lines}
	}
	// 无 offset:尾段读(lines 缺省 200,与 log op 的 1..2000 夹取一致)
	n := 200
	if v, ok := payload["lines"]; ok {
		n = int(int64From(v))
	}
	if n < 1 {
		n = 1
	}
	if n > 2000 {
		n = 2000
	}
	tail, err := o.Jobs.LogTail(id, n)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	// 尾段读不带 offset,但给出当前文件字节数,便于后端接着增量拉
	return map[string]any{"ok": true, "jobId": id, "lines": tail, "size": o.Jobs.LogSize(id)}
}

// handleJobCancel 取消任务:force=false → A(步骤边界);force=true → B(立即 kill 进程组)。
func (o *Ops) handleJobCancel(payload map[string]any) map[string]any {
	if o.Jobs == nil {
		return notImplemented("job_cancel")
	}
	id := int64Field(payload, "jobId")
	out, err := o.Jobs.Cancel(id, boolField(payload, "force"))
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	return out
}

func (o *Ops) handleJobSubscribe(payload map[string]any) map[string]any {
	if o.Jobs == nil {
		return notImplemented("job_subscribe")
	}
	id := int64Field(payload, "jobId")
	var offset int64
	if v, ok := payload["offset"]; ok && v != nil {
		offset = int64From(v)
	}
	return o.Jobs.Subscribe(id, offset)
}

func (o *Ops) handleJobUnsubscribe(payload map[string]any) map[string]any {
	if o.Jobs == nil {
		return notImplemented("job_unsubscribe")
	}
	return o.Jobs.Unsubscribe(int64Field(payload, "jobId"))
}

// jobView 把任务转成回包形状(与 jobs 表列对齐)。
func jobView(j *job.Job) map[string]any {
	out := map[string]any{
		"jobId": j.ID, "kind": j.Kind, "groupId": j.GroupID, "status": j.Status,
		"step": j.Step, "stepIndex": j.StepIndex, "stepTotal": j.StepTotal,
		"progress": j.Progress, "startedAt": j.StartedAt, "finishedAt": j.FinishedAt,
		"error": j.Error, "origin": j.Origin,
	}
	if j.Instance != "" {
		out["instance"] = j.Instance
	}
	if j.Result != nil {
		out["result"] = j.Result
	}
	if j.CLIUser != "" {
		out["cliUser"] = j.CLIUser
	}
	return out
}

// ---- 按组批量启停-------------------------

// handleBulk 执行 start_all / stop_all / restart_all:对本桥(一个组)的全部实例逐实例执行 msm。
func (o *Ops) handleBulk(op string, payload map[string]any) map[string]any {
	verb := map[string]string{"start_all": "start", "stop_all": "stop", "restart_all": "restart"}[op]
	group := stringField(payload, "group")
	if group != "" && o.Jobs != nil {
		if cur := o.Jobs.ActiveOfGroup(group); cur != nil {
			// 维护中:拒绝(与 cs CLI 的门禁同一条规则)
			return map[string]any{"ok": false, "error": fmt.Sprintf("该服务器组正在更新,已锁定(job %d)", cur.ID)}
		}
	}
	results := make([]map[string]any, 0, len(o.Names()))
	okAll := true
	for _, name := range o.Names() {
		res, err := o.Runner.Run(name, verb, nil, nil, true)
		row := map[string]any{"name": name}
		switch {
		case err != nil:
			row["ok"] = false
			row["error"] = err.Error()
			okAll = false
		case res == nil:
			row["ok"] = false
			row["error"] = "msm busy"
			okAll = false
		default:
			row["ok"] = res.Returncode == 0
			if res.Returncode != 0 {
				row["error"] = "msm 退出码 " + itoa(res.Returncode)
				okAll = false
			}
		}
		results = append(results, row)
	}
	return map[string]any{"ok": okAll, "results": results, "op": op}
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// 引用协议常量,避免能力名与 op 名在两处漂移。
var _ = protocol.CapJobs
