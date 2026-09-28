// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package ops

import (
	"arena/agent/internal/msm"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnhancedProfilesRoundTrip(t *testing.T) {
	game := t.TempDir()
	put := func(relative string) {
		p := filepath.Join(game, relative)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("original"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	put("addons/counterstrikesharp/plugins/RoundDamageRecap/RoundDamageRecap.dll")
	put("addons/counterstrikesharp/plugins/ArenaMatch/ArenaMatch.dll")
	put("addons/counterstrikesharp/plugins/WeaponPaints/WeaponPaints.dll")
	put("addons/metamod/BotHider.vdf")
	moves, e := pluginMoves(game, false)
	if e != nil || len(moves) != 2 {
		t.Fatalf("moves=%v err=%v", moves, e)
	}
	if e = applyPluginMoves(moves); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(game, "addons/counterstrikesharp/plugins/RoundDamageRecap")); !os.IsNotExist(e) {
		t.Fatal("ordinary profile still loads bot recap")
	}
	for _, name := range []string{"ArenaMatch", "WeaponPaints"} {
		if _, e = os.Stat(filepath.Join(game, "addons/counterstrikesharp/plugins", name)); e != nil {
			t.Fatal(e)
		}
	}
	moves, e = pluginMoves(game, false)
	if e != nil || len(moves) != 0 {
		t.Fatal("ordinary profile not idempotent")
	}
	moves, e = pluginMoves(game, true)
	if e != nil || len(moves) != 2 {
		t.Fatal("enhanced restore missing")
	}
	if e = applyPluginMoves(moves); e != nil {
		t.Fatal(e)
	}
	content, e := os.ReadFile(filepath.Join(game, "addons/metamod/BotHider.vdf"))
	if e != nil || string(content) != "original" {
		t.Fatal("original files changed")
	}
	if _, e = os.Stat(filepath.Join(game, "addons/metamod/BotVision.vdf")); !os.IsNotExist(e) {
		t.Fatal("uninstalled BotVision was enabled")
	}
}
func TestEnhancedProfileCollisionDoesNotMove(t *testing.T) {
	game := t.TempDir()
	for _, p := range []string{"addons/counterstrikesharp/plugins/BotAI", ".arena-match/plugin-profiles/enhanced/plugins/BotAI"} {
		if e := os.MkdirAll(filepath.Join(game, p), 0755); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := pluginMoves(game, false); e == nil {
		t.Fatal("collision accepted")
	}
	if _, e := os.Stat(filepath.Join(game, "addons/counterstrikesharp/plugins/BotAI")); e != nil {
		t.Fatal(e)
	}
}
func TestEnhancedBotCfgDetectsCommandsOnly(t *testing.T) {
	for _, s := range []string{"// bot_add_ct fake\n", "// arena: no bots (non-bot match)\n", "bot_quota 0\n", "echo bot_add_ct"} {
		if enhancedBotCfg(s) {
			t.Fatalf("false bot mode: %q", s)
		}
	}
	for _, s := range []string{"bot_add_ct \"Bot A\"\n", "  bot_add_t \"Bot B\" // cfg\n"} {
		if !enhancedBotCfg(s) {
			t.Fatalf("missing bot mode: %q", s)
		}
	}
}

func TestProfileRestartPreservesStartEnvAndWaitsForNewLog(t *testing.T) {
	game := t.TempDir()
	active := filepath.Join(game, "addons/counterstrikesharp/plugins/RoundDamageRecap")
	if err := os.MkdirAll(active, 0755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(game, "fake-msm")
	body := `#!/bin/sh
if [ "$2" = start ]; then
 echo "$MAXPLAYERS" > received-maxplayers
 echo '[ArenaMatch 0.6.3 LOADED]' >> main.log
 echo 'SV:  Spawn Server: de_mirage' >> main.log
fi
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	o := &Ops{Paths: &msm.Paths{StubDir: game}, Runner: msm.NewRunner(func() msm.RunnerConfig { return msm.RunnerConfig{MSM: script, MSMDir: game, TimeoutS: 2} })}
	result := o.doLifecycle("start", "main", map[string]any{"env": map[string]any{"MAXPLAYERS": "12"}})
	if result["ok"] != true {
		t.Fatal(result)
	}
	if err := o.prepareMatchPlugins("main"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(game, "received-maxplayers"))
	if err != nil || strings.TrimSpace(string(raw)) != "12" {
		t.Fatalf("lost MAXPLAYERS: %s %v", raw, err)
	}
	if _, err := os.Stat(active); !os.IsNotExist(err) {
		t.Fatal("enhancement still active after restart")
	}
}
