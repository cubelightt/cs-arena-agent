// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package matchipc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEventDeliveryAndDemoRetry(t *testing.T) {
	s := testStore(t)
	var events, uploads atomic.Int32
	failUpload := true
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Arena-Token") != "event-secret" {
			t.Errorf("missing token")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/events":
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("event content type")
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"matchid":117`) {
				t.Errorf("event body: %s", body)
			}
			events.Add(1)
		case "/api/demos":
			if r.Header.Get("Content-Type") != "application/octet-stream" ||
				r.Header.Get("MatchZy-FileName") != "match_117_map_0.dem" ||
				r.Header.Get("MatchZy-MatchId") != "117" ||
				r.Header.Get("MatchZy-MapNumber") != "0" ||
				r.Header.Get("MatchZy-RoundNumber") != "16" {
				t.Errorf("demo headers: %+v", r.Header)
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != "demo bytes" {
				t.Errorf("demo bytes: %q", body)
			}
			uploads.Add(1)
			if failUpload {
				w.WriteHeader(503)
				return
			}
		default:
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer httpServer.Close()
	input := testJSON(117)
	cvars := input["cvars"].(map[string]any)
	cvars["matchzy_remote_log_url"] = httpServer.URL + "/api/events"
	cvars["matchzy_demo_upload_url"] = httpServer.URL + "/api/demos"
	binding, err := s.Bind("main", 117, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PostEvent("main", 117, json.RawMessage(`{"event":"series_start","matchid":117}`)); err == nil || err.Error() != "match_not_loaded" {
		t.Fatalf("event before load accepted: %v", err)
	}
	if _, err := s.SetResult("main", 117, binding.SHA256, true, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PostEvent("main", 117, json.RawMessage(`{"event":"series_start","matchid":118}`)); err == nil || err.Error() != "invalid_event" {
		t.Fatalf("mismatched event accepted: %v", err)
	}
	if err := s.PostEvent("main", 117, json.RawMessage(`{"event":"series_start","matchid":117}`)); err != nil {
		t.Fatal(err)
	}
	if events.Load() != 0 {
		t.Fatal("event sent before durable IPC acknowledgement")
	}
	if err := s.DeliverPendingEvents("main"); err != nil {
		t.Fatal(err)
	}
	if events.Load() != 1 {
		t.Fatalf("events=%d", events.Load())
	}
	if err := s.PostEvent("main", 117, json.RawMessage(`{"event":"series_start","matchid":117}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverPendingEvents("main"); err != nil || events.Load() != 1 {
		t.Fatalf("duplicate event resent: %v, count=%d", err, events.Load())
	}
	game := s.Paths.StubDir
	if err := os.MkdirAll(filepath.Join(game, "demos"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(game, "demos", "match_117_map_0.dem")
	if err := os.WriteFile(path, []byte("demo bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDemo("main", 117, 0, 16, "../escape.dem"); err == nil {
		t.Fatal("traversal accepted")
	}
	if err := os.Symlink(path, filepath.Join(game, "outside.dem")); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDemo("main", 117, 0, 16, "demos/match_117_map_0.dem"); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDemo("main", 117, 0, 16, "demos/match_117_map_0.dem"); err != nil {
		t.Fatalf("duplicate notification: %v", err)
	}
	if err := s.QueueDemo("main", 117, 0, 17, "demos/match_117_map_0.dem"); err == nil || err.Error() != "demo_conflict" {
		t.Fatalf("conflicting notification: %v", err)
	}
	if len(s.PendingDemos()) != 1 {
		t.Fatal("demo task not durable")
	}
	task := s.PendingDemos()[0]
	if err := s.UploadDemo(task); err == nil {
		t.Fatal("failed upload acknowledged")
	}
	if len(s.PendingDemos()) != 1 {
		t.Fatal("failed upload lost")
	}
	if _, err := s.Close("main", 117); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDemo("main", 117, 0, 16, "demos/match_117_map_0.dem"); err != nil {
		t.Fatalf("duplicate after close: %v", err)
	}
	failUpload = false
	if err := s.UploadDemo(task); err != nil {
		t.Fatal(err)
	}
	if len(s.PendingDemos()) != 0 || uploads.Load() != 2 {
		t.Fatalf("retry state: %d %d", len(s.PendingDemos()), uploads.Load())
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "demo bytes" {
		t.Fatalf("source removed: %v", err)
	}
	private, _ := s.transportPath("main", 117)
	info, err := os.Stat(private)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("transport permissions: %v %v", info, err)
	}
}

func TestDemoRejectsSymlinkEscapeAndDisabledRecording(t *testing.T) {
	s := testStore(t)
	input := testJSON(117)
	input["record_demo"] = false
	binding, err := s.Bind("main", 117, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetResult("main", 117, binding.SHA256, true, "", ""); err != nil {
		t.Fatal(err)
	}
	game := s.Paths.StubDir
	outside := filepath.Join(t.TempDir(), "outside.dem")
	if err := os.WriteFile(outside, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(game, "escape.dem")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.openDemo("main", "escape.dem"); err == nil {
		t.Fatal("symlink escape accepted")
	}
	if err := s.QueueDemo("main", 117, 0, 16, "escape.dem"); err == nil || err.Error() != "demo_disabled" {
		t.Fatalf("disabled recording accepted: %v", err)
	}
}

func TestDemoCanBeQueuedAfterMatchClose(t *testing.T) {
	s := testStore(t)
	binding, err := s.Bind("main", 117, testJSON(117))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetResult("main", 117, binding.SHA256, true, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close("main", 117); err != nil {
		t.Fatal(err)
	}
	game := s.Paths.StubDir
	if err := os.MkdirAll(filepath.Join(game, "ArenaMatch"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(game, "ArenaMatch", "match_117_map_0.dem"), []byte("demo bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDemo("main", 117, 0, 16, "ArenaMatch/match_117_map_0.dem"); err != nil {
		t.Fatalf("late demo notification: %v", err)
	}
	if len(s.PendingDemos()) != 1 {
		t.Fatal("late demo task not persisted")
	}
	if _, _, err := s.Load("main", 117); err == nil {
		t.Fatal("closed match exposed to plugin load")
	}
}

func TestEventJournalRetriesAfterCloseAndRestartInOrder(t *testing.T) {
	s := testStore(t)
	seen := make(chan string, 4)
	var failFirst atomic.Bool
	failFirst.Store(true)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Arena-Token") != "event-secret" {
			t.Error("event token missing")
		}
		var payload struct {
			Event string `json:"event"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		seen <- payload.Event
		if failFirst.Swap(false) {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer httpServer.Close()
	input := testJSON(117)
	input["cvars"].(map[string]any)["matchzy_remote_log_url"] = httpServer.URL
	binding, err := s.Bind("main", 117, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetResult("main", 117, binding.SHA256, true, "", ""); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{
		`{"event":"series_start","matchid":117}`,
		`{"event":"going_live","matchid":117,"map_number":0}`,
	} {
		if err := s.PostEvent("main", 117, json.RawMessage(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 0 {
		t.Fatal("event sent before replay worker")
	}
	if _, err := s.Close("main", 117); err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverPendingEvents("main"); err == nil || err.Error() != "delivery_failed" {
		t.Fatalf("failed head not retained: %v", err)
	}
	if len(seen) != 1 || <-seen != "series_start" {
		t.Fatal("later event bypassed failed head")
	}
	// Simulate a bridge restart with the same private directory.
	restarted := &Store{Paths: s.Paths, Names: s.Names, PrivateDir: s.PrivateDir}
	if err := restarted.DeliverPendingEvents("main"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || <-seen != "series_start" || <-seen != "going_live" {
		t.Fatal("replay order after restart")
	}
	tasks, err := restarted.eventTasks("main")
	if err != nil || len(tasks) != 2 || !tasks[0].Done || !tasks[1].Done {
		t.Fatalf("journal not committed: %+v %v", tasks, err)
	}
}

func TestSocketEventAndDemoValidation(t *testing.T) {
	s := testStore(t)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Arena-Token") != "event-secret" {
			t.Error("token missing")
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer httpServer.Close()
	input := testJSON(117)
	input["cvars"].(map[string]any)["matchzy_remote_log_url"] = httpServer.URL + "/api/events"
	binding, err := s.Bind("main", 117, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetResult("main", 117, binding.SHA256, true, "", ""); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: s, listeners: map[string]*net.UnixListener{}, Authorize: func(string, int32, uint32) error { return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.reconcile(ctx)
	defer server.closeAll()
	path, err := s.SocketPath("main")
	if err != nil {
		t.Fatal(err)
	}
	good := ipcRequest(t, path, map[string]any{"version": 1, "op": "event", "matchId": 117,
		"event": map[string]any{"event": "series_start", "matchid": 117}})
	if good["ok"] != true {
		t.Fatalf("event IPC: %+v", good)
	}
	bad := ipcRequest(t, path, map[string]any{"version": 1, "op": "demo_ready", "matchId": 117,
		"mapNumber": 0, "roundNumber": 16, "path": "../escape.dem"})
	if bad["code"] != "invalid_demo_path" {
		t.Fatalf("demo IPC: %+v", bad)
	}
}
