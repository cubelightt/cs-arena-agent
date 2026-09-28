// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 仅切换已安装的增强人机模块；皮肤与通用菜单/存储插件不属于此集合。
var enhancedManaged = []string{"BotAI", "BotAimImprover", "BotBuy", "BotControllerImpl", "BotHiderImpl", "BotRandomizer", "BotState", "NadeSystem", "RayTraceImpl", "RoundDamageRecap"}
var enhancedNative = []string{"BotController.vdf", "BotHider.vdf", "BotVision.vdf", "RayTrace.vdf"}

type pluginMove struct{ from, to string }

func enhancedBotCfg(raw string) bool {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(strings.SplitN(line, "//", 2)[0])
		if len(fields) > 0 && (fields[0] == "bot_add_ct" || fields[0] == "bot_add_t") {
			return true
		}
	}
	return false
}

func pluginMoves(game string, enhanced bool) ([]pluginMove, error) {
	var moves []pluginMove
	bank := filepath.Join(game, ".arena-match", "plugin-profiles", "enhanced")
	groups := []struct {
		active, saved string
		names         []string
	}{
		{filepath.Join(game, "addons", "counterstrikesharp", "plugins"), filepath.Join(bank, "plugins"), enhancedManaged},
		{filepath.Join(game, "addons", "metamod"), filepath.Join(bank, "metamod"), enhancedNative},
	}
	for _, group := range groups {
		for _, name := range group.names {
			from, to := filepath.Join(group.active, name), filepath.Join(group.saved, name)
			if enhanced {
				from, to = to, from
			}
			if _, err := os.Lstat(from); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return nil, err
			}
			if _, err := os.Lstat(to); err == nil {
				return nil, fmt.Errorf("plugin profile collision: %s", name)
			} else if !os.IsNotExist(err) {
				return nil, err
			}
			moves = append(moves, pluginMove{from, to})
		}
	}
	return moves, nil
}

func applyPluginMoves(moves []pluginMove) error {
	for i, m := range moves {
		err := os.MkdirAll(filepath.Dir(m.to), 0755)
		if err == nil {
			err = os.Rename(m.from, m.to)
		}
		if err != nil {
			for j := i - 1; j >= 0; j-- {
				_ = os.Rename(moves[j].to, moves[j].from)
			}
			return err
		}
	}
	return nil
}

// 后端先写本场 arena_bots.cfg，再 bind；无需新增后端参数。
// 模式变化时在装载比赛前重启，原生 hook 不尝试运行中卸载。
func (o *Ops) prepareMatchPlugins(instance string) error {
	if o.Paths == nil {
		return nil
	}
	game, _ := o.Paths.FindGameDir(instance)
	if game == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(game, "cfg", "arena_bots.cfg"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	moves, err := pluginMoves(game, enhancedBotCfg(string(raw)))
	if err != nil || len(moves) == 0 {
		return err
	}
	if o.Runner == nil {
		return fmt.Errorf("plugin profile switch requires instance runner")
	}
	oldLog, _ := o.Paths.FindLogPath(instance)
	var oldSize int64
	if info, e := os.Stat(oldLog); e == nil {
		oldSize = info.Size()
	}
	result, err := o.Runner.Run(instance, "stop", nil, nil, true)
	if err != nil {
		return err
	}
	if result.Returncode != 0 {
		return fmt.Errorf("plugin profile stop failed: %s", result.Stderr)
	}
	if err = applyPluginMoves(moves); err != nil {
		return err
	}
	env := map[string]string{}
	if stored, ok := o.startEnvs.Load(instance); ok {
		env = stored.(map[string]string)
	}
	result, err = o.Runner.Run(instance, "start", nil, env, true)
	if err != nil {
		return err
	}
	if result.Returncode != 0 {
		return fmt.Errorf("plugin profile start failed: %s", result.Stderr)
	}
	// bind 的现有超时为 30 秒；只读取本次启动新增的日志，避免误认旧 LOADED。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		path, _ := o.Paths.FindLogPath(instance)
		data, e := os.ReadFile(path)
		if e == nil {
			if path == oldLog && int64(len(data)) >= oldSize {
				data = data[oldSize:]
			}
			text := string(data)
			if strings.Contains(text, "[ArenaMatch ") && strings.Contains(text, " LOADED]") && strings.Contains(text, "SV:  Spawn Server:") {
				fmt.Printf("[ArenaMatch] plugin profile instance=%s enhanced=%t switched=%d\n", instance, enhancedBotCfg(string(raw)), len(moves))
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("plugin profile restart: ArenaMatch readiness timed out")
}
