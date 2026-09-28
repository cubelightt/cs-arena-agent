// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package ops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"arena/agent/internal/fsx"
)

var (
	filenameRe    = regexp.MustCompile(`^[\w.\-]+$`)
	subdirRe      = regexp.MustCompile(`^[\w-]{0,32}$`)
	matchIDRe     = regexp.MustCompile(`^\d+$`)
	matchIDNameRe = regexp.MustCompile(`^(?:matchzy_(\d+)_.*\.json|matchzy_(\d+)_.*\.txt|Match_(\d+)\.ini)$`)

	maxMatchfileBytes = 1024 * 1024 // 1 MiB(旧 http 端点曾有、reverse 缺;M0 起统一)
)

// handleMatchfile 写入实例目录下的文件(比赛 JSON / cfg 固化);分支与校验顺序照抄 v1。
func (o *Ops) handleMatchfile(instance string, payload map[string]any) map[string]any {
	filename, _ := payload["filename"].(string)
	subdirRaw, subdirPresent := payload["subdir"]
	subdir := ""
	if subdirPresent && subdirRaw != nil {
		s, ok := subdirRaw.(string)
		if !ok {
			return map[string]any{"ok": false, "error": "subdir invalid"}
		}
		subdir = s
	}
	textRaw, textPresent := payload["text"]

	if filename == "" || !filenameRe.MatchString(filename) {
		return map[string]any{"ok": false, "error": "bad filename"}
	}
	if !subdirRe.MatchString(subdir) {
		return map[string]any{"ok": false, "error": "subdir invalid"}
	}

	var data string
	if textPresent && textRaw != nil {
		s, ok := textRaw.(string)
		if !ok {
			return map[string]any{"ok": false, "error": "text must be a string"}
		}
		data = s
	} else {
		// text 优先于 json;无 text 时必须是 JSON 对象
		obj, ok := payload["json"].(map[string]any)
		if !ok {
			return map[string]any{"ok": false, "error": "json must be an object"}
		}
		raw, err := marshalNoEscape(obj)
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}
		}
		data = raw
	}
	if len(data) > maxMatchfileBytes {
		return map[string]any{"ok": false, "error": "payload too large"}
	}

	gdir, tried := o.Paths.FindGameDir(instance)
	if gdir == "" {
		return map[string]any{"ok": false, "error": "game dir not found", "tried": tried}
	}
	full := filepath.Join(gdir, filename)
	if subdir != "" {
		full = filepath.Join(gdir, subdir, filename)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	n, err := fsx.AtomicWriteText(full, data)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	return map[string]any{"ok": true, "instance": instance, "path": full, "bytes": n}
}

// marshalNoEscape 等价于 Python 的 json.dumps(..., ensure_ascii=False):
// 中文原样落盘、不转义 <>&(Go 默认会转义,实例侧读到的内容会不一致)。
func marshalNoEscape(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// handleLog 读实例控制台日志:有 offset → 增量;否则尾段。lines 夹取 1..2000。
func (o *Ops) handleLog(instance string, payload map[string]any) map[string]any {
	lines := linesParam(payload)

	if raw, present := payload["offset"]; present && raw != nil {
		offset := intFromAny(raw, 0)
		path, tried := o.Paths.FindLogPath(instance)
		if path == "" {
			return map[string]any{"ok": false, "error": "log not found", "tried": tried}
		}
		fi, err := os.Stat(path)
		if err != nil {
			return map[string]any{"ok": false, "error": "log not found", "tried": tried}
		}
		if offset < 0 || int64(offset) > fi.Size() {
			offset = 0 // 文件轮换/缩小 → 从头读
		}
		data, next, err := fsx.ReadNewLines(path, int64(offset))
		if err != nil {
			return map[string]any{"ok": false, "error": "log not found", "tried": tried}
		}
		return map[string]any{"ok": true, "path": path, "offset": next, "lines": data}
	}

	path, tried := o.Paths.FindLogPath(instance)
	if path == "" {
		return map[string]any{"ok": false, "error": "log not found", "tried": tried}
	}
	data, err := fsx.ReadTail(path, lines)
	if err != nil {
		return map[string]any{"ok": false, "error": "log not found", "tried": tried}
	}
	return map[string]any{"ok": true, "path": path, "lines": data}
}

// handleMapfileStatus 检查实例本地地图文件(本地自维护社区图的开赛预检)。
func (o *Ops) handleMapfileStatus(instance string, payload map[string]any) map[string]any {
	filename, _ := payload["filename"].(string)
	if filename == "" || !filenameRe.MatchString(filename) {
		return map[string]any{"ok": false, "error": "bad filename"}
	}
	if o.Paths.StubDir != "" {
		p := filepath.Join(o.Paths.StubDir, "maps", filename)
		fi, err := os.Stat(p)
		present := err == nil && fi.Mode().IsRegular()
		size := int64(0)
		if present {
			size = fi.Size()
		}
		return map[string]any{"ok": true, "instance": instance, "filename": filename,
			"present": present, "path": p, "size": size, "tried": []string{p}}
	}
	gdir, tried := o.Paths.FindGameDir(instance)
	if gdir == "" {
		return map[string]any{"ok": false, "error": "game dir not found", "tried": tried}
	}
	p := filepath.Join(gdir, "maps", filename)
	tried = append(tried, p)
	fi, err := os.Stat(p)
	present := err == nil && fi.Mode().IsRegular()
	size := int64(0)
	if present {
		size = fi.Size()
	}
	return map[string]any{"ok": true, "instance": instance, "filename": filename,
		"present": present, "path": p, "size": size, "tried": tried}
}

var workshopItemRe = regexp.MustCompile(`^\d{6,20}$`)

// handleWorkshopStatus 盘点工坊共享目录 content/730(文件系统真源,供后端判定下载进度)。
func (o *Ops) handleWorkshopStatus(instance string, payload map[string]any) map[string]any {
	inst := instance
	if v, ok := payload["instance"].(string); ok && v != "" {
		inst = v
	}
	if !o.Allowed(inst) {
		return map[string]any{"ok": false, "error": "instance not allowed: " + inst}
	}
	root := o.Paths.FindWorkshopDir(inst)
	tried := o.Paths.WorkshopDirCandidates(inst)
	items := map[string]any{}
	if root != "" {
		entries, err := os.ReadDir(root)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() || !workshopItemRe.MatchString(e.Name()) {
					continue
				}
				dir := filepath.Join(root, e.Name())
				var total int64
				var mtime int64
				_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
					if err != nil || info == nil || info.IsDir() {
						return nil
					}
					total += info.Size()
					if m := info.ModTime().Unix(); m > mtime {
						mtime = m
					}
					return nil
				})
				items[e.Name()] = map[string]any{"size": total, "mtime": mtime}
			}
		}
	}
	var dirAny any = nil
	if root != "" {
		dirAny = root
	}
	return map[string]any{"ok": true, "instance": inst, "dir": dirAny, "items": items, "tried": tried}
}

// skipDirs 与 v1 的 SKIP_DIRS 相同(locate 剪枝)。
var skipDirs = map[string]bool{
	"steamapps": true, "node_modules": true, ".git": true, ".steam": true,
	"bin": true, "compiler_cache": true, "platform": true,
}

// handleLocate 在 search_dirs 下找日志文件(排障用):mtime 降序、上限 30。
func (o *Ops) handleLocate() map[string]any {
	const maxDepth = 8
	const limit = 30

	type fileInfo struct {
		Path  string `json:"path"`
		Size  int64  `json:"size"`
		Mtime int64  `json:"mtime"`
	}
	var found []fileInfo
	seen := map[string]bool{}
	for _, root := range o.Paths.SearchDirs() {
		fi, err := os.Stat(root)
		if err != nil || !fi.IsDir() {
			continue
		}
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			depth := 0
			if rel != "." {
				depth = strings.Count(rel, string(os.PathSeparator)) + 1
			}
			if info.IsDir() {
				if depth >= maxDepth {
					return filepath.SkipDir
				}
				if skipDirs[info.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			name := info.Name()
			if name != "console.log" && !strings.HasSuffix(name, ".log") {
				return nil
			}
			if seen[path] {
				return nil
			}
			seen[path] = true
			found = append(found, fileInfo{Path: path, Size: info.Size(), Mtime: info.ModTime().Unix()})
			return nil
		})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Mtime > found[j].Mtime })
	if len(found) > limit {
		found = found[:limit]
	}
	if found == nil {
		found = []fileInfo{}
	}
	return map[string]any{"ok": true, "files": found}
}

// 匹配规则参数:单场次删除的三类 + 共享的引擎回合备份
func (o *Ops) matchCleanup(instance string, matchID string, sweep bool, keep []string) map[string]any {
	gdir, tried := o.Paths.FindGameDir(instance)
	if gdir == "" {
		return map[string]any{"ok": false, "error": "game dir not found", "tried": tried}
	}

	archiveRoot := o.ArchiveDir()
	keepSet := map[string]bool{}
	for _, k := range keep {
		keepSet[k] = true
	}
	deleted := []string{}
	moved := []string{}
	errs := []string{}

	removeGlob := func(pat string, idExtract bool) {
		matches, _ := filepath.Glob(pat)
		sort.Strings(matches)
		for _, p := range matches {
			if idExtract {
				if m := matchIDNameRe.FindStringSubmatch(filepath.Base(p)); m != nil {
					id := firstNonEmpty(m[1:]...)
					if id != "" && keepSet[id] {
						continue
					}
				}
			}
			if err := os.Remove(p); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", filepath.Base(p), err))
				continue
			}
			if rel, err := filepath.Rel(gdir, p); err == nil {
				deleted = append(deleted, rel)
			}
		}
	}

	type movePair struct{ Src, Name string }
	var moves []movePair

	if sweep {
		removeGlob(filepath.Join(gdir, "MatchZyDataBackup", "matchzy_*.json"), true)
		removeGlob(filepath.Join(gdir, "matchzy_*_round*.txt"), true)
		removeGlob(filepath.Join(gdir, "MatchZyPlayerNames", "Match_*.ini"), true)
		// 引擎回合备份不参与 keepMatchIds(全删)
		removeGlob(filepath.Join(gdir, "backup_round*.txt"), false)

		loads, _ := filepath.Glob(filepath.Join(gdir, "matchzy_load_*.json"))
		sort.Strings(loads)
		for _, p := range loads {
			name := filepath.Base(p)
			skip := false
			for k := range keepSet {
				if name == "matchzy_load_"+k+".json" {
					skip = true
					break
				}
			}
			if !skip {
				moves = append(moves, movePair{Src: p, Name: name})
			}
		}
	} else {
		if !matchIDRe.MatchString(matchID) {
			return map[string]any{"ok": false, "error": "matchId must be digits"}
		}
		removeGlob(filepath.Join(gdir, "MatchZyDataBackup", "matchzy_"+matchID+"_*.json"), false)
		removeGlob(filepath.Join(gdir, "matchzy_"+matchID+"_*.txt"), false)
		removeGlob(filepath.Join(gdir, "MatchZyPlayerNames", "Match_"+matchID+".ini"), false)
		removeGlob(filepath.Join(gdir, "backup_round*.txt"), false)
		name := "matchzy_load_" + matchID + ".json"
		moves = append(moves, movePair{Src: filepath.Join(gdir, name), Name: name})
	}

	for _, mv := range moves {
		fi, err := os.Stat(mv.Src)
		if err != nil || fi.IsDir() {
			continue
		}
		destDir := filepath.Join(archiveRoot, "matchjson", instance)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", filepath.Base(mv.Src), err))
			continue
		}
		dest := filepath.Join(destDir, mv.Name)
		if _, err := os.Stat(dest); err == nil {
			// 同名已归档(重复触发/后端重放):删源即可,仍计入 moved
			if err := os.Remove(mv.Src); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", filepath.Base(mv.Src), err))
				continue
			}
		} else if err := os.Rename(mv.Src, dest); err != nil {
			// 跨挂载点(EXDEV)→ 退化为 copy+unlink
			if err := copyFile(mv.Src, dest); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", filepath.Base(mv.Src), err))
				continue
			}
			_ = os.Remove(mv.Src)
		}
		if rel, err := filepath.Rel(gdir, mv.Src); err == nil {
			moved = append(moved, rel)
		}
	}

	return map[string]any{
		"ok": true, "instance": instance,
		"deleted": deleted, "moved": moved, "errors": errs, "archive_dir": archiveRoot,
	}
}

// ArchiveDir 归档根(比赛 JSON 与日志的落点,实例目录之外)。
func (o *Ops) ArchiveDir() string {
	if o.ArchiveOverride != "" {
		return o.ArchiveOverride
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "arena-data"
	}
	return filepath.Join(home, "arena-data")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// intField 取整数字段(转换失败即默认值)。
func intField(payload map[string]any, key string, def int) int {
	raw, ok := payload[key]
	if !ok || raw == nil {
		return def
	}
	return intFromAny(raw, def)
}

// linesParam 复刻 v1 的 `max(1, min(int(payload.get("lines") or 100), 2000))`:
// 注意 Python 的 `or` 作用在**原始值**上 —— 数值 0 / 空串 / false / None 都算"缺省 = 100",
// 而字符串 "0" 是真值 → int("0") = 0 → 夹取成 1。
func linesParam(payload map[string]any) int {
	raw, ok := payload["lines"]
	if !ok {
		return 100
	}
	var n int
	switch t := raw.(type) {
	case nil:
		return 100
	case float64:
		if t == 0 {
			return 100
		}
		n = int(t)
	case int:
		if t == 0 {
			return 100
		}
		n = t
	case bool:
		if !t {
			return 100
		}
		n = 1
	case string:
		if t == "" {
			return 100
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 100
		}
		n = parsed
	default:
		return 100
	}
	if n < 1 {
		return 1
	}
	if n > 2000 {
		return 2000
	}
	return n
}

func intFromAny(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return def
		}
		return n
	default:
		return def
	}
}
