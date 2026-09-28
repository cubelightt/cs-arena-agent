// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package matchipc owns per-instance ArenaMatch configuration and local sockets.
// It never exposes the platform token or backend URL to the game plugin.
package matchipc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"arena/agent/internal/msm"
)

const MaxMatchBytes = 1024 * 1024

var idRe = regexp.MustCompile(`^[1-9][0-9]*$`)
var instanceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Binding struct {
	Version   int    `json:"version"`
	MatchID   int64  `json:"matchId"`
	Instance  string `json:"instance"`
	SHA256    string `json:"sha256"`
	File      string `json:"file"`
	Status    string `json:"status"` // bound / loaded / failed / closed
	Code      string `json:"code,omitempty"`
	Reason    string `json:"reason,omitempty"`
	ResultOK  *bool  `json:"resultOk,omitempty"`
	Acked     bool   `json:"acked,omitempty"`
	ResultSeq int    `json:"resultSeq,omitempty"`
}

type Store struct {
	mu         sync.Mutex
	eventMu    sync.Mutex
	Paths      *msm.Paths
	Names      func() []string
	PrivateDir string // bridge-owned config directory; never under the game directory
}

func (s *Store) allowed(instance string) bool {
	for _, name := range s.Names() {
		if name == instance {
			return true
		}
	}
	return false
}

func (s *Store) instanceDir(instance string) (string, error) {
	if !instanceRe.MatchString(instance) {
		return "", errors.New("invalid instance name")
	}
	if !s.allowed(instance) {
		return "", fmt.Errorf("instance not allowed: %s", instance)
	}
	if s.Paths.StubDir != "" {
		return filepath.Join(s.Paths.StubDir, "arena-match", instance), nil
	}
	game, _ := s.Paths.FindGameDir(instance)
	if game == "" {
		return "", errors.New("game dir not found")
	}
	return filepath.Join(game, ".arena-match"), nil
}

func validID(id int64) bool { return id > 0 && idRe.MatchString(strconv.FormatInt(id, 10)) }

func (s *Store) metaPath(instance string, id int64) (string, error) {
	if !validID(id) {
		return "", errors.New("invalid match ID")
	}
	dir, err := s.instanceDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("match_%d.meta.json", id)), nil
}

func (s *Store) SocketPath(instance string) (string, error) {
	dir, err := s.instanceDir(instance)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "bridge.sock")
	if len(path) >= 108 {
		return "", fmt.Errorf("Unix socket path too long: %s", path)
	}
	return path, nil
}

// Bind atomically stores plugin-safe JSON and its digest. Rebinding the same
// match with identical bytes is idempotent; another active match is rejected.
func (s *Store) Bind(instance string, id int64, input map[string]any) (Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	metaPath, err := s.metaPath(instance, id)
	if err != nil {
		return Binding{}, err
	}
	if input == nil {
		return Binding{}, errors.New("json must be an object")
	}
	matchID, ok := NumberID(input["matchid"])
	if !ok || matchID != id {
		return Binding{}, errors.New("matchid mismatch")
	}
	// Copy at the top level and inside cvars. The original input remains intact
	// for the old MatchZy path, while the plugin receives no transport secrets.
	clean := make(map[string]any, len(input))
	for k, v := range input {
		clean[k] = v
	}
	cvars, ok := input["cvars"].(map[string]any)
	if !ok {
		return Binding{}, errors.New("cvars must be an object")
	}
	transport, err := transportFromCvars(cvars)
	if err != nil {
		return Binding{}, err
	}
	filtered := make(map[string]any, len(cvars))
	for k, v := range cvars {
		if strings.HasPrefix(k, "matchzy_remote_log_") || strings.HasPrefix(k, "matchzy_demo_upload_") {
			continue
		}
		filtered[k] = v
	}
	clean["cvars"] = filtered
	data, err := json.Marshal(clean)
	if err != nil {
		return Binding{}, err
	}
	if len(data) == 0 || len(data) > MaxMatchBytes {
		return Binding{}, errors.New("match JSON exceeds 1 MiB")
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	if current, err := s.readMeta(metaPath); err == nil {
		if current.SHA256 == hash && current.Status != "closed" {
			if err := s.writeTransport(instance, id, transport); err != nil {
				return Binding{}, err
			}
			return current, nil
		}
		return Binding{}, errors.New("match ID already bound with different digest")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Binding{}, err
	}
	dir := filepath.Dir(metaPath)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Binding{}, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".meta.json") || entry.Name() == filepath.Base(metaPath) {
			continue
		}
		other, err := s.readMeta(filepath.Join(dir, entry.Name()))
		if err != nil {
			return Binding{}, err
		}
		if other.Status != "closed" {
			return Binding{}, fmt.Errorf("instance_busy: match %d", other.MatchID)
		}
	}
	if err := secureDir(dir); err != nil {
		return Binding{}, err
	}
	if err := s.writeTransport(instance, id, transport); err != nil {
		return Binding{}, err
	}
	file := fmt.Sprintf("match_%d.json", id)
	if err := atomicPrivate(filepath.Join(dir, file), data); err != nil {
		return Binding{}, err
	}
	binding := Binding{Version: 1, MatchID: id, Instance: instance, SHA256: hash, File: file, Status: "bound"}
	if err := s.writeMeta(metaPath, binding); err != nil {
		return Binding{}, err
	}
	return binding, nil
}

func NumberID(raw any) (int64, bool) {
	switch n := raw.(type) {
	case float64:
		v := int64(n)
		return v, float64(v) == n
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		v, err := n.Int64()
		return v, err == nil
	default:
		return 0, false
	}
}

func (s *Store) Load(instance string, id int64) ([]byte, Binding, error) {
	return s.load(instance, id, false)
}

// 已完成的录像可能在 series_end/close 后才落盘；只供桥内部核对旧比赛配置。
func (s *Store) load(instance string, id int64, allowClosed bool) ([]byte, Binding, error) {
	path, err := s.metaPath(instance, id)
	if err != nil {
		return nil, Binding{}, err
	}
	meta, err := s.readMeta(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Binding{}, errors.New("match_not_bound")
		}
		return nil, Binding{}, err
	}
	if meta.Instance != instance || meta.MatchID != id || meta.Version != 1 || (meta.Status == "closed" && !allowClosed) {
		return nil, Binding{}, errors.New("match_not_bound")
	}
	if meta.File != fmt.Sprintf("match_%d.json", id) {
		return nil, Binding{}, errors.New("invalid match file binding")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), meta.File))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Binding{}, errors.New("file_missing")
		}
		return nil, Binding{}, err
	}
	if len(data) == 0 || len(data) > MaxMatchBytes {
		return nil, Binding{}, errors.New("match JSON size invalid")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != meta.SHA256 {
		return nil, Binding{}, errors.New("digest_mismatch")
	}
	return data, meta, nil
}

func (s *Store) SetResult(instance string, id int64, hash string, success bool, code, reason string) (Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.metaPath(instance, id)
	if err != nil {
		return Binding{}, err
	}
	meta, err := s.readMeta(path)
	if err != nil {
		return Binding{}, err
	}
	if meta.SHA256 != hash || meta.Instance != instance || meta.Status == "closed" {
		return Binding{}, errors.New("digest_mismatch")
	}
	if meta.ResultOK != nil {
		if *meta.ResultOK == success && meta.Code == code {
			return meta, nil
		}
		if *meta.ResultOK {
			return Binding{}, errors.New("result_conflict")
		}
	}
	meta.ResultOK, meta.Reason, meta.Acked = &success, reason, false
	meta.ResultSeq++
	if success {
		meta.Status, meta.Code = "loaded", ""
	} else {
		meta.Status, meta.Code = "failed", code
	}
	return meta, s.writeMeta(path, meta)
}

func (s *Store) ResultStatus(instance string, id int64, hash string) (Binding, error) {
	path, err := s.metaPath(instance, id)
	if err != nil {
		return Binding{}, err
	}
	meta, err := s.readMeta(path)
	if err != nil {
		return Binding{}, err
	}
	if len(hash) != 64 || meta.SHA256 != hash || meta.Instance != instance || meta.Status == "closed" {
		return Binding{}, errors.New("digest_mismatch")
	}
	return meta, nil
}

func (s *Store) PendingResults() []LoadResult {
	var pending []LoadResult
	for _, instance := range s.Names() {
		dir, err := s.instanceDir(instance)
		if err != nil {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".meta.json") {
				continue
			}
			meta, err := s.readMeta(filepath.Join(dir, entry.Name()))
			if err == nil && meta.ResultOK != nil && !meta.Acked && meta.Instance == instance {
				pending = append(pending, LoadResult{Instance: instance, MatchID: meta.MatchID,
					SHA256: meta.SHA256, ResultSeq: meta.ResultSeq, OK: *meta.ResultOK, Code: meta.Code, Reason: meta.Reason})
			}
		}
	}
	return pending
}

func (s *Store) AckResult(instance string, id int64, hash string, resultSeq int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.metaPath(instance, id)
	if err != nil {
		return err
	}
	meta, err := s.readMeta(path)
	if err != nil {
		return err
	}
	if meta.ResultOK == nil || meta.SHA256 != hash || resultSeq <= 0 || meta.ResultSeq != resultSeq {
		return errors.New("result not found")
	}
	meta.Acked = true
	return s.writeMeta(path, meta)
}

func (s *Store) Close(instance string, id int64) (Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.metaPath(instance, id)
	if err != nil {
		return Binding{}, err
	}
	meta, err := s.readMeta(path)
	if err != nil {
		return Binding{}, err
	}
	meta.Status = "closed"
	return meta, s.writeMeta(path, meta)
}

func (s *Store) readMeta(path string) (Binding, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Binding{}, err
	}
	var meta Binding
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Binding{}, err
	}
	return meta, nil
}

func (s *Store) writeMeta(path string, meta Binding) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return atomicPrivate(path, raw)
}

func secureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func atomicPrivate(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".arena-match-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
