// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package matchipc

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxFrameBytes = 2*MaxMatchBytes + 4096

type LoadResult struct {
	Instance  string `json:"instance"`
	MatchID   int64  `json:"matchId"`
	SHA256    string `json:"sha256"`
	ResultSeq int    `json:"resultSeq"`
	OK        bool   `json:"ok"`
	Code      string `json:"code,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type Server struct {
	Store  *Store
	Logf   func(string, ...any)
	Report func(LoadResult) error // bridge -> backend delivery; nil retains local result pending
	// Authorize is injected only by tests. Production uses SO_PEERCRED + /proc.
	Authorize func(instance string, pid int32, uid uint32) error

	mu        sync.Mutex
	uploadMu  sync.Mutex
	listeners map[string]*net.UnixListener
}

type request struct {
	Version     int             `json:"version"`
	Op          string          `json:"op"`
	MatchID     int64           `json:"matchId"`
	SHA256      string          `json:"sha256"`
	OK          bool            `json:"ok"`
	Code        string          `json:"code"`
	Reason      string          `json:"reason"`
	Event       json.RawMessage `json:"event"`
	MapNumber   int             `json:"mapNumber"`
	RoundNumber int             `json:"roundNumber"`
	Path        string          `json:"path"`
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Run reconciles sockets after hello_ack/config_sync updates the instance list.
// It never listens on TCP. A disconnected backend does not stop local delivery.
func (s *Server) Run(ctx context.Context) {
	s.mu.Lock()
	s.listeners = make(map[string]*net.UnixListener)
	s.mu.Unlock()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	demoTicker := time.NewTicker(10 * time.Second)
	defer demoTicker.Stop()
	s.reconcile(ctx)
	go s.retryDemos()
	go s.Store.RetryPendingEvents(s.logf)
	for {
		select {
		case <-ctx.Done():
			s.closeAll()
			return
		case <-ticker.C:
			s.reconcile(ctx)
		case <-demoTicker.C:
			go s.retryDemos()
			go s.Store.RetryPendingEvents(s.logf)
		}
	}
}

func (s *Server) reconcile(ctx context.Context) {
	want := map[string]bool{}
	for _, instance := range s.Store.Names() {
		want[instance] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for instance, listener := range s.listeners {
		if !want[instance] {
			listener.Close()
			delete(s.listeners, instance)
		}
	}
	for instance := range want {
		if s.listeners[instance] != nil {
			continue
		}
		path, err := s.Store.SocketPath(instance)
		if err != nil {
			s.logf("ArenaMatch socket %s: %v", instance, err)
			continue
		}
		if err := secureDir(filepath.Dir(path)); err != nil {
			s.logf("ArenaMatch socket %s: %v", instance, err)
			continue
		}
		if fi, err := os.Lstat(path); err == nil {
			if fi.Mode()&os.ModeSocket == 0 {
				s.logf("ArenaMatch socket path occupied by non-socket: %s", path)
				continue
			}
			conn, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
			if dialErr == nil {
				conn.Close()
				s.logf("ArenaMatch socket already active: %s", path)
				continue
			}
			if err := os.Remove(path); err != nil {
				s.logf("ArenaMatch stale socket remove failed: %v", err)
				continue
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			s.logf("ArenaMatch socket stat failed: %v", err)
			continue
		}
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			s.logf("ArenaMatch listen %s: %v", instance, err)
			continue
		}
		if err := os.Chmod(path, 0o600); err != nil {
			listener.Close()
			s.logf("ArenaMatch socket chmod %s: %v", instance, err)
			continue
		}
		s.listeners[instance] = listener
		go s.accept(ctx, instance, listener)
	}
}

func (s *Server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, listener := range s.listeners {
		listener.Close()
		delete(s.listeners, name)
	}
}

func (s *Server) accept(ctx context.Context, instance string, listener *net.UnixListener) {
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				s.logf("ArenaMatch accept %s: %v", instance, err)
			}
			return
		}
		go s.handle(instance, conn)
	}
}

func (s *Server) handle(instance string, conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := s.authorize(instance, conn); err != nil {
		s.reply(conn, map[string]any{"version": 1, "ok": false, "matchId": 0, "code": "unauthorized_peer"})
		s.logf("ArenaMatch unauthorized peer on %s: %v", instance, err)
		return
	}
	reader := bufio.NewReader(io.LimitReader(conn, maxFrameBytes+1))
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) > maxFrameBytes {
		s.reply(conn, map[string]any{"version": 1, "ok": false, "matchId": 0, "code": "invalid_request"})
		return
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil || req.Version != 1 || !validID(req.MatchID) {
		s.reply(conn, map[string]any{"version": 1, "ok": false, "matchId": req.MatchID, "code": "invalid_request"})
		return
	}
	switch req.Op {
	case "load_match":
		data, binding, err := s.Store.Load(instance, req.MatchID)
		if err != nil {
			s.reply(conn, map[string]any{"version": 1, "ok": false, "matchId": req.MatchID, "code": errorCode(err)})
			return
		}
		s.reply(conn, map[string]any{"version": 1, "ok": true, "matchId": req.MatchID,
			"sha256": binding.SHA256, "jsonBase64": base64.StdEncoding.EncodeToString(data)})
	case "load_result":
		if len(req.SHA256) != 64 || len(req.Code) > 64 || len(req.Reason) > 256 {
			s.reply(conn, map[string]any{"version": 1, "ok": false, "matchId": req.MatchID, "code": "invalid_request"})
			return
		}
		meta, err := s.Store.SetResult(instance, req.MatchID, req.SHA256, req.OK, req.Code, req.Reason)
		if err != nil {
			s.reply(conn, map[string]any{"version": 1, "ok": false, "matchId": req.MatchID, "code": errorCode(err)})
			return
		}
		result := LoadResult{Instance: instance, MatchID: req.MatchID, SHA256: req.SHA256, ResultSeq: meta.ResultSeq,
			OK: req.OK, Code: req.Code, Reason: req.Reason}
		if s.Report != nil {
			if err := s.Report(result); err != nil {
				s.logf("ArenaMatch result report deferred %s/%d: %v", instance, req.MatchID, err)
			}
		}
		s.reply(conn, map[string]any{"version": 1, "ok": true, "matchId": req.MatchID})
	case "load_status":
		meta, err := s.Store.ResultStatus(instance, req.MatchID, req.SHA256)
		if err != nil {
			s.reply(conn, map[string]any{"version": 1, "ok": false, "matchId": req.MatchID, "code": errorCode(err)})
			return
		}
		s.reply(conn, map[string]any{"version": 1, "ok": true, "matchId": req.MatchID,
			"status": meta.Status, "acked": meta.Acked, "resultSeq": meta.ResultSeq})
	case "event":
		if err := s.Store.PostEvent(instance, req.MatchID, req.Event); err != nil {
			s.reply(conn, map[string]any{"version": 1, "ok": false, "matchId": req.MatchID, "code": errorCode(err)})
			return
		}
		s.reply(conn, map[string]any{"version": 1, "ok": true, "matchId": req.MatchID})
		go s.Store.RetryPendingEvents(s.logf)
	case "demo_ready":
		if err := s.Store.QueueDemo(instance, req.MatchID, req.MapNumber, req.RoundNumber, req.Path); err != nil {
			s.reply(conn, map[string]any{"version": 1, "ok": false, "matchId": req.MatchID, "code": errorCode(err)})
			return
		}
		s.reply(conn, map[string]any{"version": 1, "ok": true, "matchId": req.MatchID})
		go s.retryDemos()
	default:
		s.reply(conn, map[string]any{"version": 1, "ok": false, "matchId": req.MatchID, "code": "invalid_request"})
	}
}

func errorCode(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "match_not_bound"
	}
	message := err.Error()
	for _, code := range []string{"digest_mismatch", "match_not_bound", "match_not_loaded", "file_missing", "instance_busy", "result_conflict", "invalid_event", "invalid_demo_path", "demo_disabled", "demo_conflict", "delivery_failed", "invalid_request"} {
		if strings.HasPrefix(message, code) {
			return code
		}
	}
	return "invalid_match_json"
}

func (s *Server) retryDemos() {
	if !s.uploadMu.TryLock() {
		return
	}
	defer s.uploadMu.Unlock()
	for _, task := range s.Store.PendingDemos() {
		if err := s.Store.UploadDemo(task); err != nil {
			s.logf("ArenaMatch demo upload deferred %s/%d map %d: %v", task.Instance, task.MatchID, task.MapNumber, err)
		}
	}
}

func (s *Server) reply(conn *net.UnixConn, value any) {
	raw, _ := json.Marshal(value)
	raw = append(raw, '\n')
	_, _ = conn.Write(raw)
}

func (s *Server) authorize(instance string, conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		cred, controlErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if controlErr != nil || cred == nil {
		return fmt.Errorf("SO_PEERCRED: %v", controlErr)
	}
	if s.Authorize != nil {
		return s.Authorize(instance, cred.Pid, cred.Uid)
	}
	if cred.Uid != uint32(os.Getuid()) {
		return errors.New("UID mismatch")
	}
	gameDir, _ := s.Store.Paths.FindGameDir(instance)
	if gameDir == "" || s.Store.Paths.StubDir != "" {
		return errors.New("game dir unavailable for peer verification")
	}
	gameRoot, err := filepath.EvalSymlinks(filepath.Dir(gameDir))
	if err != nil {
		return err
	}
	proc := fmt.Sprintf("/proc/%d", cred.Pid)
	cwd, err := os.Readlink(filepath.Join(proc, "cwd"))
	if err != nil {
		return err
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(gameRoot, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("peer cwd is outside instance game directory")
	}
	exe, err := os.Readlink(filepath.Join(proc, "exe"))
	if err != nil {
		return err
	}
	if filepath.Base(exe) != "cs2" {
		return errors.New("peer executable is not CS2")
	}
	return nil
}
