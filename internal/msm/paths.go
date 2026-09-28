// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package msm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Paths 是由 msm_dir 推导出来的路径模板(v2 不再让用户堆配置,决策 9)。
//
// 约定与 v1 默认模板同值:`<msm_dir>/../msm.d/cs2/...`;非标准布局由 `cs doctor` 指路
// ,而不是在 config.yaml 里再开一组 log_dirs / game_dirs / workshop_dirs。
type Paths struct {
	MSMDir       string
	StubDir      string // ARENA_STUB_DIR:stub 测试时命中本地夹具目录(COMPAT §4.2 四处)
	LogGlobs     []string
	GameDirs     []string
	WorkshopDirs []string
}

// NewPaths 按 msm_dir 推导模板;StubDir 取自环境(导入时读一次,与 v1 一致)。
func NewPaths(msmDir string) *Paths {
	return &Paths{
		MSMDir:  msmDir,
		StubDir: os.Getenv("ARENA_STUB_DIR"),
		LogGlobs: []string{
			"{msm_dir}/../msm.d/cs2/log/inst-{instance}/*-server.log",
			"{msm_dir}/log/inst-{instance}/*-server.log",
			"{msm_dir}/instances/{instance}/game/csgo/console.log",
			"{msm_dir}/instances/{instance}/csgo/console.log",
			"{msm_dir}/{instance}/game/csgo/console.log",
			"{msm_dir}/{instance}/csgo/console.log",
		},
		GameDirs: []string{
			"{msm_dir}/../msm.d/cs2/inst-{instance}/game/csgo",
			"{msm_dir}/instances/{instance}/game/csgo",
			"{msm_dir}/{instance}/game/csgo",
		},
		WorkshopDirs: []string{
			"{msm_dir}/../msm.d/cs2/inst-{instance}/game/bin/linuxsteamrt64/steamapps/workshop/content/730",
			"{msm_dir}/instances/{instance}/game/bin/linuxsteamrt64/steamapps/workshop/content/730",
			"{msm_dir}/{instance}/game/bin/linuxsteamrt64/steamapps/workshop/content/730",
		},
	}
}

// expand 插值 {msm_dir} 与 {instance}(只有这两个变量,与 v1 相同)。
func (p *Paths) expand(tmpl, instance string) string {
	s := strings.ReplaceAll(tmpl, "{msm_dir}", p.MSMDir)
	s = strings.ReplaceAll(s, "{instance}", instance)
	return filepath.Clean(s)
}

// LogPathCandidates 返回控制台日志候选路径(不含 stub 命中,stub 由 FindLogPath 优先处理)。
func (p *Paths) LogPathCandidates(instance string) []string {
	out := make([]string, 0, len(p.LogGlobs))
	for _, t := range p.LogGlobs {
		out = append(out, p.expand(t, instance))
	}
	return out
}

// GameDirCandidates 返回实例 csgo 目录候选。
func (p *Paths) GameDirCandidates(instance string) []string {
	out := make([]string, 0, len(p.GameDirs))
	for _, t := range p.GameDirs {
		out = append(out, p.expand(t, instance))
	}
	return out
}

// WorkshopDirCandidates 返回 content/730 共享目录候选。
func (p *Paths) WorkshopDirCandidates(instance string) []string {
	out := make([]string, 0, len(p.WorkshopDirs))
	for _, t := range p.WorkshopDirs {
		out = append(out, p.expand(t, instance))
	}
	return out
}

// FindLogPath 取第一个命中的日志文件;glob 命中多个时取 mtime 最新(引擎会轮换日志名)。
// 返回 (路径, tried)。stub 目录存在时优先 `<stub>/<instance>.log`。
func (p *Paths) FindLogPath(instance string) (string, []string) {
	var tried []string
	if p.StubDir != "" {
		cand := filepath.Join(p.StubDir, instance+".log")
		if fi, err := os.Stat(cand); err == nil && fi.Mode().IsRegular() {
			return cand, tried
		}
	}
	for _, c := range p.LogPathCandidates(instance) {
		tried = append(tried, c)
		if !strings.Contains(c, "*") {
			if fi, err := os.Stat(c); err == nil && fi.Mode().IsRegular() {
				return c, tried
			}
			continue
		}
		matches, _ := filepath.Glob(c)
		var best string
		var bestMod int64 = -1
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			if fi.ModTime().UnixNano() > bestMod {
				best, bestMod = m, fi.ModTime().UnixNano()
			}
		}
		if best != "" {
			return best, tried
		}
	}
	return "", tried
}

// FindGameDir 取第一个存在的实例 csgo 目录(跟随符号链接)。
// ARENA_STUB_DIR 是目录时直接返回它本身。
func (p *Paths) FindGameDir(instance string) (string, []string) {
	var tried []string
	if p.StubDir != "" {
		if fi, err := os.Stat(p.StubDir); err == nil && fi.IsDir() {
			return p.StubDir, tried
		}
	}
	for _, c := range p.GameDirCandidates(instance) {
		tried = append(tried, c)
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c, tried
		}
	}
	return "", tried
}

// FindWorkshopDir 取实例的 content/730 目录;stub 下只认 `<stub>/workshop/content/730`,不回退模板。
func (p *Paths) FindWorkshopDir(instance string) string {
	if p.StubDir != "" {
		cand := filepath.Join(p.StubDir, "workshop", "content", "730")
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand
		}
		return ""
	}
	for _, c := range p.WorkshopDirCandidates(instance) {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}
	return ""
}

// MapFile 本地自维护社区图的预检路径(stub 下为 `<stub>/maps/<filename>`)。
func (p *Paths) MapFile(instance, filename string) (string, bool) {
	if p.StubDir != "" {
		cand := filepath.Join(p.StubDir, "maps", filename)
		if fi, err := os.Stat(cand); err == nil && fi.Mode().IsRegular() {
			return cand, true
		}
		return cand, false
	}
	gdir, _ := p.FindGameDir(instance)
	if gdir == "" {
		return "", false
	}
	cand := filepath.Join(gdir, "maps", filename)
	if fi, err := os.Stat(cand); err == nil && fi.Mode().IsRegular() {
		return cand, true
	}
	return cand, false
}

// SearchDirs 是 locate 的搜索根(v1 默认:msm_dir 与其同级 msm.d)。
func (p *Paths) SearchDirs() []string {
	return []string{
		p.MSMDir,
		filepath.Join(p.MSMDir, "..", "msm.d"),
	}
}

// Describe 供 doctor/日志展示(路径未命中时给出 tried 清单,而不是让人猜)。
func (p *Paths) Describe(instance string) string {
	log, _ := p.FindLogPath(instance)
	game, _ := p.FindGameDir(instance)
	if log == "" {
		log = "(未命中)"
	}
	if game == "" {
		game = "(未命中)"
	}
	return fmt.Sprintf("msm_dir=%s log=%s game=%s", p.MSMDir, log, game)
}
