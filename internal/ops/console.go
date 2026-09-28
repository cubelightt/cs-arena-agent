// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package ops

import (
	"os"
	"strings"
	"sync"

	"arena/agent/internal/fsx"
	"arena/agent/internal/protocol"
)

// ConsoleHub 保存控制台订阅与 tail 游标。
//
// 与 v1 一致:订阅与 offset 是**进程级**状态(跨重连保留)—— 后端重连后会重放
// console_subscribe(其 payload.offset 被桥忽略),桥用自己记录的 offset 续读,不重复也不丢行。
type ConsoleHub struct {
	mu   sync.Mutex
	subs map[string]bool
	tail map[string]*tailState
	last map[string]bool // instance → 最近一次 running(变化时才推 console_state)
	push func(any) error // 由 agent 在连接建立时注入;断线清空

	o *Ops
}

type tailState struct {
	Path   string
	Offset int64
	// Running 记录上次推送的 running 值(nil 语义用 sent 表示)
	Running    bool
	RunningSet bool
}

func NewConsoleHub(o *Ops) *ConsoleHub {
	return &ConsoleHub{
		subs: map[string]bool{},
		tail: map[string]*tailState{},
		last: map[string]bool{},
		o:    o,
	}
}

// SetPush 注入当前连接的推送函数(连接建立时调用)。
func (h *ConsoleHub) SetPush(fn func(any) error) {
	h.mu.Lock()
	h.push = fn
	h.mu.Unlock()
}

// ClearPush 断线时清空(此时推送必然失败,留着只会刷日志)。
func (h *ConsoleHub) ClearPush() {
	h.mu.Lock()
	h.push = nil
	h.mu.Unlock()
}

// Subscribe 登记订阅;**不覆盖**已有的 tail 游标(setdefault,与 v1 相同)。
func (h *ConsoleHub) Subscribe(instance string) map[string]any {
	h.mu.Lock()
	h.subs[instance] = true
	if _, ok := h.tail[instance]; !ok {
		h.tail[instance] = &tailState{}
	}
	h.mu.Unlock()
	running := h.o.Runner.Status(instance, true) == "RUNNING"
	return map[string]any{"ok": true, "instance": instance, "running": running}
}

// Unsubscribe 取消订阅并清掉 tail/last 状态。
func (h *ConsoleHub) Unsubscribe(instance string) map[string]any {
	h.mu.Lock()
	delete(h.subs, instance)
	delete(h.tail, instance)
	delete(h.last, instance)
	h.mu.Unlock()
	return map[string]any{"ok": true}
}

// Subscribed 返回当前订阅的实例名(顺序不保证)。
func (h *ConsoleHub) Subscribed() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.subs))
	for n := range h.subs {
		out = append(out, n)
	}
	return out
}

// Tick 按 console_tail_ms 周期调用:推 running 变化、日志轮换、日志增量。
// 任一实例的异常只影响本次 tick(与 v1 相同)。
func (h *ConsoleHub) Tick() {
	for _, inst := range h.Subscribed() {
		h.tickOne(inst)
	}
}

func (h *ConsoleHub) tickOne(inst string) {
	running := h.o.Runner.Status(inst, true) == "RUNNING"

	h.mu.Lock()
	if prev, ok := h.last[inst]; !ok || prev != running {
		h.last[inst] = running
		push := h.push
		h.mu.Unlock()
		if push != nil {
			_ = push(protocol.ConsoleStatePush{
				Type:     protocol.TypePush,
				Kind:     protocol.KindConsoleState,
				Instance: inst,
				Running:  running,
			})
		}
	} else {
		h.mu.Unlock()
	}
	if !running {
		return
	}

	path, _ := h.o.Paths.FindLogPath(inst)
	if path == "" {
		return
	}

	h.mu.Lock()
	st, ok := h.tail[inst]
	if !ok {
		st = &tailState{}
		h.tail[inst] = st
	}
	reset := false
	if st.Path != "" && st.Path != path {
		reset = true
		st.Path = path
		st.Offset = 0
	}
	push := h.push
	h.mu.Unlock()
	if reset && push != nil {
		_ = push(protocol.ConsoleResetPush{
			Type:     protocol.TypePush,
			Kind:     protocol.KindConsoleReset,
			Instance: inst,
		})
	}

	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	h.mu.Lock()
	if fi.Size() < st.Offset {
		st.Offset = 0 // 文件被截断/轮换
	}
	offset := st.Offset
	h.mu.Unlock()

	lines, next, err := fsx.ReadNewLines(path, offset)
	if err != nil {
		return
	}

	h.mu.Lock()
	st.Path = path
	st.Offset = next
	push = h.push
	h.mu.Unlock()

	if len(lines) > 0 && push != nil {
		_ = push(protocol.ConsolePush{
			Type:     protocol.TypePush,
			Kind:     protocol.KindConsole,
			Instance: inst,
			Path:     path,
			Offset:   next,
			Lines:    lines,
		})
	}
}

// handleConsole 处理 console op:校验命令再下发(文案与 v1 逐字一致)。
func (o *Ops) handleConsole(instance string, payload map[string]any) map[string]any {
	cmd, _ := payload["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return map[string]any{"ok": false, "error": "command required"}
	}
	if strings.ContainsAny(cmd, "\n\r") {
		return map[string]any{"ok": false, "error": "command must be single line"}
	}
	// 长度按**未 strip 的原串**判(与 v1 一致),下发时 strip
	if len(cmd) > 2000 {
		return map[string]any{"ok": false, "error": "command too long (max 2000)"}
	}
	res, err := o.Runner.Run(instance, "send", []string{strings.TrimSpace(cmd)}, nil, true)
	return o.replyOrError(res, err, "console", map[string]any{"instance": instance})
}
