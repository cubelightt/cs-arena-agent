// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package ops

import (
	"testing"

	"arena/agent/internal/matchipc"
	"arena/agent/internal/msm"
)

func TestArenaMatchBindDispatchAndWhitelist(t *testing.T) {
	names := func() []string { return []string{"main"} }
	store := &matchipc.Store{Paths: &msm.Paths{StubDir: t.TempDir()}, Names: names, PrivateDir: t.TempDir()}
	o := &Ops{Names: names, MatchIPC: &matchipc.Server{Store: store}}
	json := map[string]any{"matchid": float64(117), "cvars": map[string]any{
		"mp_friendlyfire": "1", "matchzy_remote_log_url": "http://example.test/api/events",
		"matchzy_remote_log_header_key": "X-Arena-Token", "matchzy_remote_log_header_value": "secret",
		"matchzy_demo_upload_url":        "http://example.test/api/demos",
		"matchzy_demo_upload_header_key": "X-Arena-Token", "matchzy_demo_upload_header_value": "secret"}}
	payload := map[string]any{"matchId": float64(117), "json": json}
	if got := o.Handle("arena_match_bind", "other", payload); got["ok"] != false {
		t.Fatalf("unlisted instance accepted: %+v", got)
	}
	got := o.Handle("arena_match_bind", "main", payload)
	if got["ok"] != true || got["sha256"] == "" {
		t.Fatalf("bind failed: %+v", got)
	}
	if got := o.Handle("arena_match_close", "main", map[string]any{"matchId": float64(117)}); got["ok"] != true || got["status"] != "closed" {
		t.Fatalf("close failed: %+v", got)
	}
}
