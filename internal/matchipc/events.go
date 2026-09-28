// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package matchipc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// eventTask is fsynced before the plugin receives an IPC acknowledgement.
// Completed tasks remain as duplicate receipts across plugin/bridge restarts.
type eventTask struct {
	Version  int             `json:"version"`
	Instance string          `json:"instance"`
	MatchID  int64           `json:"matchId"`
	Seq      uint64          `json:"seq"`
	Payload  json.RawMessage `json:"payload"`
	Done     bool            `json:"done"`
}

func (s *Store) eventDir(instance string) (string, error) {
	dir, err := s.privateInstanceDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "events"), nil
}

func eventFile(dir string, seq uint64) string {
	return filepath.Join(dir, fmt.Sprintf("event_%020d.json", seq))
}

func (s *Store) eventTasks(instance string) ([]eventTask, error) {
	dir, err := s.eventDir(instance)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tasks := make([]eventTask, 0)
	for _, entry := range entries { // os.ReadDir is lexically sorted; sequence is zero padded.
		name := entry.Name()
		if !strings.HasPrefix(name, "event_") || !strings.HasSuffix(name, ".json") {
			continue
		}
		seq, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "event_"), ".json"), 10, 64)
		if err != nil || entry.IsDir() {
			return nil, errors.New("invalid event journal")
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var task eventTask
		if json.Unmarshal(raw, &task) != nil || task.Version != 1 || task.Instance != instance ||
			task.Seq != seq || !validID(task.MatchID) || len(task.Payload) == 0 {
			return nil, errors.New("invalid event journal")
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// PostEvent validates and journals one event; IPC success means durable local
// acceptance. Delivery to the website happens asynchronously in sequence.
func (s *Store) PostEvent(instance string, id int64, raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > 64*1024 {
		return errors.New("invalid_event")
	}
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		return errors.New("invalid_event")
	}
	name, _ := event["event"].(string)
	eventID, ok := NumberID(event["matchid"])
	if name == "" || len(name) > 64 || !ok || eventID != id {
		return errors.New("invalid_event")
	}
	// A closed, previously loaded match may still have a pending final event.
	if _, err := s.activeTransport(instance, id, true); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.eventTasks(instance)
	if err != nil {
		return err
	}
	var seq uint64
	for _, task := range tasks {
		if task.Seq >= seq {
			seq = task.Seq + 1
		}
		if task.MatchID == id && bytes.Equal(task.Payload, raw) {
			return nil // duplicate retry, including one already delivered.
		}
	}
	dir, err := s.eventDir(instance)
	if err != nil {
		return err
	}
	if err := secureDir(dir); err != nil {
		return err
	}
	task := eventTask{Version: 1, Instance: instance, MatchID: id, Seq: seq, Payload: append(json.RawMessage(nil), raw...)}
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return atomicPrivate(eventFile(dir, seq), data)
}

// DeliverPendingEvents serializes delivery across all instances and refuses to
// pass a failed older event. Website dedup handles a crash after HTTP success
// but before the completed marker is written.
func (s *Store) DeliverPendingEvents(instance string) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	tasks, err := s.eventTasks(instance)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.Done {
			continue
		}
		config, err := s.activeTransport(instance, task.MatchID, true)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.EventURL, bytes.NewReader(task.Payload))
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Arena-Token", config.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			return errors.New("delivery_failed")
		}
		var result struct {
			OK bool `json:"ok"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result)
		resp.Body.Close()
		cancel()
		if resp.StatusCode/100 != 2 || decodeErr != nil || !result.OK {
			return errors.New("delivery_failed")
		}
		task.Done = true
		data, err := json.Marshal(task)
		if err != nil {
			return err
		}
		dir, err := s.eventDir(instance)
		if err != nil {
			return err
		}
		if err := atomicPrivate(eventFile(dir, task.Seq), data); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RetryPendingEvents(logf func(string, ...any)) {
	for _, instance := range s.Names() {
		if err := s.DeliverPendingEvents(instance); err != nil && logf != nil {
			logf("ArenaMatch event delivery deferred %s: %v", instance, err)
		}
	}
}
