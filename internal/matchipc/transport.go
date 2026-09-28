// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package matchipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type transportConfig struct {
	EventURL string `json:"eventUrl"`
	DemoURL  string `json:"demoUrl"`
	Token    string `json:"token"`
}

type demoTask struct {
	Version     int    `json:"version"`
	Instance    string `json:"instance"`
	MatchID     int64  `json:"matchId"`
	MapNumber   int    `json:"mapNumber"`
	RoundNumber int    `json:"roundNumber"`
	Path        string `json:"path"` // relative to the instance csgo directory
	Done        bool   `json:"done"`
}

var demoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.dem$`)

func transportFromCvars(cvars map[string]any) (transportConfig, error) {
	field := func(key string) string { v, _ := cvars[key].(string); return v }
	eventURL, demoURL := field("matchzy_remote_log_url"), field("matchzy_demo_upload_url")
	if field("matchzy_remote_log_header_key") != "X-Arena-Token" ||
		field("matchzy_demo_upload_header_key") != "X-Arena-Token" ||
		field("matchzy_remote_log_header_value") == "" ||
		field("matchzy_remote_log_header_value") != field("matchzy_demo_upload_header_value") {
		return transportConfig{}, errors.New("invalid transport authentication")
	}
	for _, raw := range []string{eventURL, demoURL} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
			return transportConfig{}, errors.New("invalid transport URL")
		}
	}
	return transportConfig{EventURL: eventURL, DemoURL: demoURL, Token: field("matchzy_remote_log_header_value")}, nil
}

func (s *Store) privateInstanceDir(instance string) (string, error) {
	if !instanceRe.MatchString(instance) || !s.allowed(instance) || s.PrivateDir == "" {
		return "", errors.New("private transport directory unavailable")
	}
	return filepath.Join(s.PrivateDir, instance), nil
}

func (s *Store) transportPath(instance string, id int64) (string, error) {
	if !validID(id) {
		return "", errors.New("invalid match ID")
	}
	dir, err := s.privateInstanceDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("transport_%d.json", id)), nil
}

func (s *Store) writeTransport(instance string, id int64, config transportConfig) error {
	path, err := s.transportPath(instance, id)
	if err != nil {
		return err
	}
	if err := secureDir(s.PrivateDir); err != nil {
		return err
	}
	if err := secureDir(filepath.Dir(path)); err != nil {
		return err
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return atomicPrivate(path, raw)
}

func (s *Store) activeTransport(instance string, id int64, allowClosed bool) (transportConfig, error) {
	path, err := s.metaPath(instance, id)
	if err != nil {
		return transportConfig{}, err
	}
	binding, err := s.readMeta(path)
	if err != nil {
		return transportConfig{}, err
	}
	if binding.Instance != instance || binding.MatchID != id ||
		(binding.Status != "loaded" && !(allowClosed && binding.Status == "closed")) ||
		binding.ResultOK == nil || !*binding.ResultOK {
		return transportConfig{}, errors.New("match_not_loaded")
	}
	path, err = s.transportPath(instance, id)
	if err != nil {
		return transportConfig{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return transportConfig{}, err
	}
	var config transportConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return transportConfig{}, err
	}
	if config.Token == "" {
		return transportConfig{}, errors.New("transport token missing")
	}
	return config, nil
}

func (s *Store) openDemo(instance, relative string) (*os.File, error) {
	if relative == "" || len(relative) > 512 || filepath.IsAbs(relative) || strings.Contains(relative, "\\") ||
		filepath.Clean(relative) != relative || relative == "." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) ||
		!demoName.MatchString(filepath.Base(relative)) {
		return nil, errors.New("invalid_demo_path")
	}
	game, _ := s.Paths.FindGameDir(instance)
	if game == "" {
		return nil, errors.New("invalid_demo_path")
	}
	root, err := os.OpenRoot(game)
	if err != nil {
		return nil, errors.New("invalid_demo_path")
	}
	defer root.Close()
	file, err := root.Open(relative)
	if err != nil {
		return nil, errors.New("invalid_demo_path")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		file.Close()
		return nil, errors.New("invalid_demo_path")
	}
	return file, nil
}

func (s *Store) taskPath(instance string, id int64, mapNumber int) (string, error) {
	if !validID(id) || mapNumber < 0 || mapNumber > 2 {
		return "", errors.New("invalid_request")
	}
	dir, err := s.privateInstanceDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("demo_%d_%d.json", id, mapNumber)), nil
}

func (s *Store) QueueDemo(instance string, id int64, mapNumber, roundNumber int, relative string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if roundNumber < 0 || roundNumber > 200 {
		return errors.New("invalid_request")
	}
	path, err := s.taskPath(instance, id, mapNumber)
	if err != nil {
		return err
	}
	if raw, err := os.ReadFile(path); err == nil {
		var current demoTask
		if json.Unmarshal(raw, &current) != nil || current.Version != 1 || current.Instance != instance ||
			current.MatchID != id || current.MapNumber != mapNumber || current.Path != relative || current.RoundNumber != roundNumber {
			return errors.New("demo_conflict")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// GOTV 需在停录并确认文件非空后才上报；系列可能已经关闭。
	// 仅允许曾成功装载的绑定，关闭状态不撤销本场录像上传权限。
	_, err = s.activeTransport(instance, id, true)
	if err != nil {
		return err
	}
	data, _, err := s.load(instance, id, true)
	if err != nil {
		return err
	}
	var match struct {
		RecordDemo *bool `json:"record_demo"`
	}
	if err := json.Unmarshal(data, &match); err != nil {
		return err
	}
	if match.RecordDemo != nil && !*match.RecordDemo {
		return errors.New("demo_disabled")
	}
	file, err := s.openDemo(instance, relative)
	if err != nil {
		return err
	}
	file.Close()
	task := demoTask{Version: 1, Instance: instance, MatchID: id, MapNumber: mapNumber, RoundNumber: roundNumber, Path: relative}
	raw, _ := json.Marshal(task)
	if err := secureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return atomicPrivate(path, raw)
}

func (s *Store) PendingDemos() []demoTask {
	var tasks []demoTask
	for _, instance := range s.Names() {
		dir, err := s.privateInstanceDir(instance)
		if err != nil {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "demo_") || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			var task demoTask
			if json.Unmarshal(raw, &task) == nil && task.Version == 1 && !task.Done && task.Instance == instance {
				tasks = append(tasks, task)
			}
		}
	}
	return tasks
}

func (s *Store) UploadDemo(task demoTask) error {
	path, err := s.taskPath(task.Instance, task.MatchID, task.MapNumber)
	if err != nil {
		return err
	}
	// Revalidate persisted metadata and source on each attempt, including after restart.
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var current demoTask
	if json.Unmarshal(raw, &current) != nil || current != task || current.Done {
		return errors.New("demo_conflict")
	}
	config, err := s.activeTransport(task.Instance, task.MatchID, true)
	if err != nil {
		return err
	}
	file, err := s.openDemo(task.Instance, task.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("invalid_demo_path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.DemoURL, file)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Arena-Token", config.Token)
	req.Header.Set("MatchZy-FileName", filepath.Base(task.Path))
	req.Header.Set("MatchZy-MatchId", fmt.Sprint(task.MatchID))
	req.Header.Set("MatchZy-MapNumber", fmt.Sprint(task.MapNumber))
	req.Header.Set("MatchZy-RoundNumber", fmt.Sprint(task.RoundNumber))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New("delivery_failed")
	}
	defer resp.Body.Close()
	var result struct {
		OK bool `json:"ok"`
	}
	if resp.StatusCode/100 != 2 || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result) != nil || !result.OK {
		return errors.New("delivery_failed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current.Done = true
	raw, _ = json.Marshal(current)
	return atomicPrivate(path, raw)
}
