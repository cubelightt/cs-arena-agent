// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package ops

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

func timeoutCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

var urlRe = regexp.MustCompile(`^https?://`)

// handleProbe 主机侧探针:GET 目标 URL,只读前 512 B(排障用,后端 debug 路由)。
//
// 注意:**该 handler 的回包没有 `ok` 键**(与 v1 一致)→ 帧上的 ok 取缺省 true;
// HTTP 4xx/5xx 与网络错误都不算"命令失败",分别体现在 http_code / error 字段里。
func (o *Ops) handleProbe(payload map[string]any) map[string]any {
	url, _ := payload["url"].(string)
	if !urlRe.MatchString(url) {
		return map[string]any{"ok": false, "error": "url must start with http(s)://"}
	}
	const probeTimeout = 15 * time.Second
	client := &http.Client{Timeout: probeTimeout}

	resp, err := client.Get(url)
	if err != nil {
		return map[string]any{"http_code": nil, "ok_body": nil, "error": err.Error()}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	okBody := string(body)
	if len(okBody) > 200 {
		okBody = okBody[:200]
	}
	return map[string]any{"http_code": resp.StatusCode, "ok_body": okBody, "error": nil}
}

// handlePS 列出 CS2/服务器相关进程(ps 过滤);出错也回 ok:true + 错误文本(与 v1 一致)。
func (o *Ops) handlePS() map[string]any {
	ctx, cancel := timeoutCtx(10 * time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-eo", "pid,etime,cmd").Output()
	if err != nil {
		return map[string]any{"ok": true, "processes": []string{"<error: " + err.Error() + ">"}}
	}
	procs := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "srcds") || strings.Contains(lower, "cs2") || strings.Contains(lower, "counterstrike") {
			procs = append(procs, line)
		}
		if len(procs) >= 40 {
			break
		}
	}
	return map[string]any{"ok": true, "processes": procs}
}
