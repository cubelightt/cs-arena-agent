// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package matchipc

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"arena/agent/internal/msm"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	return &Store{Paths: &msm.Paths{StubDir: root}, PrivateDir: t.TempDir(), Names: func() []string { return []string{"main", "match1"} }}
}

func testJSON(id int64) map[string]any {
	return map[string]any{
		"matchid": id, "maplist": []any{"de_mirage"}, "num_maps": 1,
		"cvars": map[string]any{
			"mp_friendlyfire":                  "1",
			"matchzy_remote_log_url":           "http://example.test/api/events",
			"matchzy_remote_log_header_key":    "X-Arena-Token",
			"matchzy_remote_log_header_value":  "event-secret",
			"matchzy_demo_upload_url":          "http://example.test/api/demos",
			"matchzy_demo_upload_header_key":   "X-Arena-Token",
			"matchzy_demo_upload_header_value": "event-secret",
		},
	}
}

func TestBindingIsolationAndDigest(t *testing.T) {
	s := testStore(t)
	b, err := s.Bind("main", 117, testJSON(117))
	if err != nil {
		t.Fatal(err)
	}
	if b.MatchID != 117 || len(b.SHA256) != 64 {
		t.Fatalf("binding: %+v", b)
	}
	data, _, err := s.Load("main", 117)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "event-secret") || strings.Contains(string(data), "demo-secret") {
		t.Fatal("transport token leaked to plugin JSON")
	}
	if !strings.Contains(string(data), "mp_friendlyfire") {
		t.Fatal("room cvar missing")
	}
	again, err := s.Bind("main", 117, testJSON(117))
	if err != nil || again.SHA256 != b.SHA256 {
		t.Fatalf("idempotent bind: %+v %v", again, err)
	}
	if _, err := s.Bind("main", 118, testJSON(118)); err == nil || !strings.Contains(err.Error(), "instance_busy") {
		t.Fatalf("expected instance_busy, got %v", err)
	}
	if _, err := s.Bind("match1", 117, testJSON(117)); err != nil {
		t.Fatalf("other instance: %v", err)
	}
	if _, err := s.Bind("main", 120, testJSON(119)); err == nil {
		t.Fatal("accepted mismatched matchid")
	}
	path, _ := s.metaPath("main", 117)
	gameJSON := filepath.Join(filepath.Dir(path), "match_117.json")
	if err := os.WriteFile(gameJSON, []byte(`{"matchid":117}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load("main", 117); err == nil || err.Error() != "digest_mismatch" {
		t.Fatalf("tamper not rejected: %v", err)
	}
	if _, err := s.Close("main", 117); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Bind("main", 118, testJSON(118)); err != nil {
		t.Fatalf("new match after close: %v", err)
	}
	if _, err := os.Stat(gameJSON); err != nil {
		t.Fatalf("close removed old JSON: %v", err)
	}
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("private directory mode: %v %v", fi, err)
	}
}

func ipcRequest(t *testing.T, path string, input any) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, _ := json.Marshal(input)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(line, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// Unauthorized peers receive a rejection before the server reads a request.
func ipcUnauthorized(t *testing.T, path string) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(line, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSocketLoadResultAndAck(t *testing.T) {
	store := testStore(t)
	b, err := store.Bind("main", 117, testJSON(117))
	if err != nil {
		t.Fatal(err)
	}
	var reported []LoadResult
	var deny atomic.Bool
	server := &Server{Store: store, listeners: map[string]*net.UnixListener{},
		Authorize: func(string, int32, uint32) error {
			if deny.Load() {
				return errors.New("denied")
			}
			return nil
		},
		Report: func(r LoadResult) error { reported = append(reported, r); return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.reconcile(ctx)
	defer server.closeAll()
	path, err := store.SocketPath("main")
	if err != nil {
		t.Fatal(err)
	}
	load := ipcRequest(t, path, map[string]any{"version": 1, "op": "load_match", "matchId": 117})
	if load["ok"] != true || load["sha256"] != b.SHA256 {
		t.Fatalf("load: %+v", load)
	}
	decoded, err := base64.StdEncoding.DecodeString(load["jsonBase64"].(string))
	if err != nil || strings.Contains(string(decoded), "secret") {
		t.Fatalf("unsafe payload: %v", err)
	}
	result := ipcRequest(t, path, map[string]any{"version": 1, "op": "load_result", "matchId": 117,
		"sha256": b.SHA256, "ok": true})
	if result["ok"] != true || len(reported) != 1 || reported[0].ResultSeq != 1 {
		t.Fatalf("report: %+v %+v", result, reported)
	}
	if pending := store.PendingResults(); len(pending) != 1 || !pending[0].OK {
		t.Fatalf("pending: %+v", pending)
	}
	status := ipcRequest(t, path, map[string]any{"version": 1, "op": "load_status", "matchId": 117, "sha256": b.SHA256})
	if status["ok"] != true || status["acked"] != false || status["status"] != "loaded" || status["resultSeq"] != float64(1) {
		t.Fatalf("status before ack: %+v", status)
	}
	if err := store.AckResult("main", 117, b.SHA256, 1); err != nil {
		t.Fatal(err)
	}
	status = ipcRequest(t, path, map[string]any{"version": 1, "op": "load_status", "matchId": 117, "sha256": b.SHA256})
	if status["acked"] != true {
		t.Fatalf("status after ack: %+v", status)
	}
	if pending := store.PendingResults(); len(pending) != 0 {
		t.Fatalf("ack not applied: %+v", pending)
	}
	bad := ipcRequest(t, path, map[string]any{"version": 1, "op": "load_match", "matchId": 118})
	if bad["ok"] != false || bad["code"] != "match_not_bound" {
		t.Fatalf("unbound: %+v", bad)
	}
	deny.Store(true)
	denied := ipcUnauthorized(t, path)
	if denied["code"] != "unauthorized_peer" {
		t.Fatalf("unauthorized: %+v", denied)
	}
}

func TestFailedLoadCanRetrySameBinding(t *testing.T) {
	store := testStore(t)
	b, err := store.Bind("main", 117, testJSON(117))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetResult("main", 117, b.SHA256, false, "apply_failed", "temporary"); err != nil {
		t.Fatal(err)
	}
	if err := store.AckResult("main", 117, b.SHA256, 1); err != nil {
		t.Fatal(err)
	}
	meta, err := store.SetResult("main", 117, b.SHA256, true, "", "")
	if err != nil || meta.Status != "loaded" || meta.Acked || meta.ResultSeq != 2 {
		t.Fatalf("retry not pending: %+v %v", meta, err)
	}
	if err := store.AckResult("main", 117, b.SHA256, 1); err == nil {
		t.Fatal("stale failure ack accepted for successful retry")
	}
	if err := store.AckResult("main", 117, b.SHA256, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetResult("main", 117, b.SHA256, false, "apply_failed", "again"); err == nil || err.Error() != "result_conflict" {
		t.Fatalf("success overwritten: %v", err)
	}
}
