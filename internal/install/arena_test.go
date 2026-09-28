// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"arena/agent/internal/host"
)

// fixtureLayout 造一个最小 msm 布局:base + 各实例的游戏目录、MatchZy/ArenaDuel 插件位、
// MatchZy cfg、gamemode cfg、gameinfo.gi。返回 msmDir 与 home。
func fixtureLayout(t *testing.T, instances []string) (msmDir, home string) {
	t.Helper()
	tmp := t.TempDir()
	msmDir = filepath.Join(tmp, "cs2-multiserver-new")
	home = filepath.Join(tmp, "home")
	mustMkdir(t, msmDir)
	mustMkdir(t, home)

	write := func(p, body string) {
		t.Helper()
		mustMkdir(t, filepath.Dir(p))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("写夹具 %s: %v", p, err)
		}
	}

	sites := append([]string{"base"}, prefixInst(instances)...)
	for _, s := range sites {
		dir := siteDir(msmDir, s)
		write(filepath.Join(dir, "game", "csgo", "addons", "counterstrikesharp", "plugins", "MatchZy", "MatchZy.dll"), "official matchzy dll")
		write(filepath.Join(dir, "game", "csgo", "addons", "counterstrikesharp", "plugins", "ArenaDuel", "ArenaDuel.dll"), "old arena duel")
		write(filepath.Join(dir, "game", "csgo", "addons", "counterstrikesharp", "plugins", "ArenaDuel", "ArenaDuel.deps.json"), "old deps")
		write(filepath.Join(dir, "game", "csgo", "gameinfo.gi"), "GameInfo\n{\n\t\t\tGame_LowViolence\tcsgo_lv // Perfect World content override\n\n\t\t\tGame\tcsgo\n}\n")
		write(filepath.Join(dir, "game", "csgo", "cfg", "gamemode_competitive.cfg"), "bot_quota\t\t\t\t\t\t\t\t\t\t\t1\nbot_quota_mode\t\t\t\t\t\t\t\t\t\tcompetitive\n")
		write(filepath.Join(dir, "game", "csgo", "cfg", "gamemode_competitive_offline.cfg"), "// offline\nbot_quota\t\t\t\t\t\t\t\t\t\t\t10\nbot_quota_mode\t\t\t\t\t\t\t\t\t\tfill\n")
		write(filepath.Join(dir, "game", "csgo", "cfg", "MatchZy", "warmup.cfg"), "mp_maxrounds 24\nmp_warmup_start\nmp_warmuptime 9999\n")
		write(filepath.Join(dir, "game", "csgo", "cfg", "MatchZy", "live_override.cfg"), "\n")
		write(filepath.Join(dir, "game", "csgo", "cfg", "MatchZy", "live_wingman_override.cfg"), "\n")
	}
	// bot mod 的调优 cfg(只有"增强人机实例"才有:match3)
	write(filepath.Join(siteDir(msmDir, "inst-match3"), "game", "csgo", "cfg", "my_bot_normal_config.cfg"), "sv_cheats 1\nbot_difficulty 5\n")
	// 工坊图共享源:main 是实目录
	mustMkdir(t, filepath.Join(siteDir(msmDir, "inst-main"), "game", "bin", "linuxsteamrt64", "steamapps"))
	return msmDir, home
}

func prefixInst(instances []string) []string {
	out := make([]string, 0, len(instances))
	for _, i := range instances {
		out = append(out, "inst-"+i)
	}
	return out
}

func siteDir(msmDir, site string) string {
	if site == "base" {
		return host.BaseDir(msmDir)
	}
	return host.InstanceDir(msmDir, strings.TrimPrefix(site, "inst-"))
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录 %s: %v", dir, err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	return string(raw)
}

func countBak(t *testing.T, root string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(filepath.Base(p), ".bak-") {
			n++
		}
		return nil
	})
	return n
}

func applyFixture(t *testing.T, msmDir, home string, instances []string, dryRun bool) []ArenaPatchItem {
	t.Helper()
	items, err := ApplyArenaPatches(ArenaPatchOptions{
		MSMDir: msmDir, Instances: instances, Home: home, DryRun: dryRun,
	})
	if err != nil {
		t.Fatalf("ApplyArenaPatches: %v", err)
	}
	return items
}

// 首次执行必须把全清单补齐;第二次执行必须全 ok(幂等,不产生任何写入)。
func TestArenaPatchesRepairThenIdempotent(t *testing.T) {
	instances := []string{"main", "match1", "match3"}
	msmDir, home := fixtureLayout(t, instances)

	items := applyFixture(t, msmDir, home, instances, false)
	if Changed(items) == 0 {
		t.Fatalf("首次执行应当修复多项,实际 0:%+v", items)
	}

	// 现有插件由用户管理，主机补丁不得覆盖它们。
	for _, site := range []string{"base", "inst-main", "inst-match1", "inst-match3"} {
		dir := filepath.Join(siteDir(msmDir, site), "game", "csgo", "addons", "counterstrikesharp", "plugins")
		for rel, want := range map[string]string{"MatchZy/MatchZy.dll": "official matchzy dll", "ArenaDuel/ArenaDuel.dll": "old arena duel", "ArenaDuel/ArenaDuel.deps.json": "old deps"} {
			if got := read(t, filepath.Join(dir, rel)); got != want {
				t.Fatalf("不应覆盖用户插件 %s/%s: %q", site, rel, got)
			}
		}
	}

	// 人机 cfg 链(match3):三份都要挂上 arena_bots.cfg,且 bot_quota 0 在位
	mz := filepath.Join(siteDir(msmDir, "inst-match3"), "game", "csgo", "cfg", "MatchZy")
	for _, f := range []string{"warmup.cfg", "live_override.cfg", "live_wingman_override.cfg"} {
		body := read(t, filepath.Join(mz, f))
		if !strings.Contains(body, "exec arena_bots.cfg") || !strings.Contains(body, "bot_quota 0") {
			t.Fatalf("%s 未补齐 bot 就位块:\n%s", f, body)
		}
	}
	if !strings.Contains(read(t, filepath.Join(mz, "warmup.cfg")), "exec my_bot_normal_config.cfg") {
		t.Fatalf("装了 bot mod 的实例应当带上 exec my_bot_normal_config.cfg")
	}

	// gamemode 配额归零(缩进/行尾保留)
	for _, f := range []string{"gamemode_competitive.cfg", "gamemode_competitive_offline.cfg"} {
		body := read(t, filepath.Join(siteDir(msmDir, "inst-match3"), "game", "csgo", "cfg", f))
		if !strings.Contains(body, "bot_quota\t\t\t\t\t\t\t\t\t\t\t0\n") {
			t.Fatalf("%s 的 bot_quota 未归零且未保留缩进:\n%s", f, body)
		}
		if !strings.Contains(body, "bot_quota_mode\t\t\t\t\t\t\t\t\t\tnormal") {
			t.Fatalf("%s 的 bot_quota_mode 未置 normal:\n%s", f, body)
		}
	}

	// gameinfo 搜索路径
	if !strings.Contains(read(t, filepath.Join(siteDir(msmDir, "inst-match3"), "game", "csgo", "gameinfo.gi")), "Game\tcsgo/overrides/botprofile.vpk") {
		t.Fatalf("gameinfo.gi 未插入 botprofile 搜索路径")
	}
	// 非人机实例不该被动
	if strings.Contains(read(t, filepath.Join(siteDir(msmDir, "inst-match1"), "game", "csgo", "gameinfo.gi")), "botprofile") {
		t.Fatalf("非人机实例的 gameinfo.gi 不应被改")
	}

	// 日志外移 + 工坊图共享 + arena_bots.cfg 占位
	for _, inst := range instances {
		for _, sp := range []struct{ rel, arch string }{{filepath.Join("game", "csgo", "logs"), "logs"}, {filepath.Join("game", "csgo", "addons", "counterstrikesharp", "logs"), "cssharp-logs"}} {
			link := filepath.Join(host.InstanceDir(msmDir, inst), sp.rel)
			want := filepath.Join(home, "arena-data", sp.arch, inst)
			if got, err := os.Readlink(link); err != nil || got != want {
				t.Fatalf("%s 的 %s 应为链接 → %s(实际 %q,err %v)", inst, sp.rel, want, got, err)
			}
		}
	}
	ownerRel := filepath.Join("game", "bin", "linuxsteamrt64", "steamapps")
	for _, inst := range []string{"match1", "match3"} {
		got, err := os.Readlink(filepath.Join(host.InstanceDir(msmDir, inst), ownerRel))
		if err != nil || got != filepath.Join(host.InstanceDir(msmDir, "main"), ownerRel) {
			t.Fatalf("%s 的 steamapps 应链接到 main(实际 %q,err %v)", inst, got, err)
		}
	}
	if !strings.Contains(read(t, filepath.Join(siteDir(msmDir, "inst-match3"), "game", "csgo", "cfg", "arena_bots.cfg")), "no bots") {
		t.Fatalf("arena_bots.cfg 占位未写入")
	}

	// 第二次:全 ok,且不再产生 .bak
	bakBefore := countBak(t, msmDir)
	items2 := applyFixture(t, msmDir, home, instances, false)
	if n := Changed(items2); n != 0 {
		t.Fatalf("第二次执行应当零改动,实际 %d 项:%+v", n, items2)
	}
	if bakAfter := countBak(t, msmDir); bakAfter != bakBefore {
		t.Fatalf("幂等执行不应再产生备份(%d → %d)", bakBefore, bakAfter)
	}
}

// dry-run 必须一个字节都不写。
func TestArenaPatchesDryRunWritesNothing(t *testing.T) {
	instances := []string{"main", "match3"}
	msmDir, home := fixtureLayout(t, instances)
	before := map[string]string{}
	_ = filepath.WalkDir(msmDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			before[p] = read(t, p)
		}
		return nil
	})

	items := applyFixture(t, msmDir, home, instances, true)
	if Changed(items) == 0 {
		t.Fatalf("dry-run 也应报告将修复项")
	}
	after := map[string]string{}
	_ = filepath.WalkDir(msmDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			after[p] = read(t, p)
		}
		return nil
	})
	if len(before) != len(after) {
		t.Fatalf("dry-run 改了文件数:%d → %d", len(before), len(after))
	}
	for p, body := range before {
		if after[p] != body {
			t.Fatalf("dry-run 改写了 %s", p)
		}
	}
	if _, err := os.Lstat(filepath.Join(home, "arena-data", "logs", "main")); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不应建归档目录")
	}
}

// 缺少 Game_LowViolence 锚点时如实报警告,不乱插。
func TestInsertBotprofilePathNoAnchor(t *testing.T) {
	raw := []byte("GameInfo\n{\n\tGame\tcsgo\n}\n")
	out, changed, note := insertBotprofilePath(raw)
	if changed || string(out) != string(raw) {
		t.Fatalf("无锚点不应改动:%q", string(out))
	}
	if !strings.Contains(note, "需人工") {
		t.Fatalf("应提示需人工核对,实际:%s", note)
	}
}

// gameinfo 插入保留缩进与 CRLF,且幂等。
func TestInsertBotprofilePathCRLFAndIdempotent(t *testing.T) {
	raw := []byte("GameInfo\r\n{\r\n\t\t\tGame_LowViolence\tcsgo_lv\r\n\t\t\tGame\tcsgo\r\n}\r\n")
	out, changed, _ := insertBotprofilePath(raw)
	if !changed {
		t.Fatal("应当插入")
	}
	s := string(out)
	if !strings.Contains(s, "\t\t\tGame\tcsgo/overrides/botprofile.vpk\r\n") {
		t.Fatalf("插入行缩进/行尾不对:%q", s)
	}
	if strings.Index(s, "Game_LowViolence") > strings.Index(s, "botprofile.vpk") {
		t.Fatalf("插入位置应在 Game_LowViolence 之后")
	}
	again, changed2, _ := insertBotprofilePath(out)
	if changed2 || string(again) != s {
		t.Fatalf("第二次插入应当 no-op")
	}
}

// gamemode 配额:缩进/行尾保留,缺行补行,幂等。
func TestZeroBotQuota(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		changed bool
	}{
		{"已归零", "bot_quota 0\nbot_quota_mode normal\n", []string{"bot_quota 0\n", "bot_quota_mode normal\n"}, false},
		{"tab 缩进保留", "bot_quota\t\t1\nbot_quota_mode\t\tcompetitive\n", []string{"bot_quota\t\t0\n", "bot_quota_mode\t\tnormal\n"}, true},
		{"CRLF 保留", "bot_quota 10\r\n", []string{"bot_quota 0\r\n", "bot_quota_mode normal\n"}, true},
		{"缺行补行", "// offline mode\n", []string{"// offline mode\n", "bot_quota 0\n", "bot_quota_mode normal\n"}, true},
		{"大小写变体也认并统一写小写", "BOT_QUOTA 3\n", []string{"bot_quota 0\n", "bot_quota_mode normal\n"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed, _ := zeroBotQuota([]byte(tc.in))
			if changed != tc.changed {
				t.Fatalf("changed=%v 期望 %v", changed, tc.changed)
			}
			if string(out) != strings.Join(tc.want, "") {
				t.Fatalf("结果不对:\n got %q\nwant %q", string(out), strings.Join(tc.want, ""))
			}
		})
	}
}

// 追加块必须与 2026-08-28 固化时写入主机的字节逐一致(否则每次都会判定为"需修复")。
func TestBotCfgBlockMatchesHost(t *testing.T) {
	wantWarmup := "\n// bot-improver: restore sv_cheats cvars & bot tuning\nexec my_bot_normal_config.cfg\nbot_quota_mode normal\nbot_quota 0\n\n// arena: bot placement (cfg rewritten per match by backend)\nexec arena_bots.cfg\n"
	if got := botCfgBlock("warmup.cfg", true, true); got != wantWarmup {
		t.Fatalf("warmup 块不一致:\n got %q\nwant %q", got, wantWarmup)
	}
	wantLive := "\nsv_cheats 1\nexec my_bot_normal_config.cfg\nbot_quota_mode normal\nbot_quota 0\n\n// arena: bot placement (cfg rewritten per match by backend)\nexec arena_bots.cfg\n"
	if got := botCfgBlock("live_override.cfg", true, true); got != wantLive {
		t.Fatalf("live_override 块不一致:\n got %q\nwant %q", got, wantLive)
	}
	// stock 的 live_override/live_wingman 本身就是一个空行 → 不再补前导空行
	if got := botCfgBlock("live_override.cfg", true, false); strings.HasPrefix(got, "\n") {
		t.Fatalf("已有空行时不补前导换行:%q", got)
	}
	// 没有 bot mod 的实例:不 exec 不存在的文件
	if got := botCfgBlock("warmup.cfg", false, true); strings.Contains(got, "my_bot_normal_config") {
		t.Fatalf("无 bot mod 时不应带 my_bot_normal_config:%q", got)
	}
}

func TestNeedsLeadingBlank(t *testing.T) {
	cases := map[string]bool{
		"":                    false, // 空文件
		"\n":                  false, // 本身就是一个空行
		"mp_maxrounds 24\n":   true,  // 末尾是内容行
		"mp_maxrounds 24\n\n": false, // 已有空行结尾
		"mp_maxrounds 24":     true,  // 无换行结尾
	}
	for in, want := range cases {
		if got := needsLeadingBlank([]byte(in)); got != want {
			t.Fatalf("needsLeadingBlank(%q)=%v 期望 %v", in, got, want)
		}
	}
}

// 真机上追加块拼出来的文件必须与主机现状逐字节一致(用主机 match3 的实测字节做回归)。
func TestBotCfgChainProducesHostBytes(t *testing.T) {
	instances := []string{"match3"}
	msmDir, home := fixtureLayout(t, instances)
	applyFixture(t, msmDir, home, instances, false)

	// warmup:stock 末尾是内容行 → 带分隔空行(主机 warmup.cfg 1015 字节的尾部形态)
	got := read(t, filepath.Join(host.InstanceDir(msmDir, "match3"), "game", "csgo", "cfg", "MatchZy", "warmup.cfg"))
	want := "mp_maxrounds 24\nmp_warmup_start\nmp_warmuptime 9999\n" + botCfgBlock("warmup.cfg", true, true)
	if got != want {
		t.Fatalf("warmup.cfg 结果不一致:\n got %q\nwant %q", got, want)
	}

	// live_override:stock 是一字节空行 → 结果必须正好 159 字节(与主机 match3 现状同形)
	live := read(t, filepath.Join(host.InstanceDir(msmDir, "match3"), "game", "csgo", "cfg", "MatchZy", "live_override.cfg"))
	wantLive := "\nsv_cheats 1\nexec my_bot_normal_config.cfg\nbot_quota_mode normal\nbot_quota 0\n\n// arena: bot placement (cfg rewritten per match by backend)\nexec arena_bots.cfg\n"
	if live != wantLive {
		t.Fatalf("live_override.cfg 结果不一致:\n got %q (%d 字节)\nwant %q (%d 字节)", live, len(live), wantLive, len(wantLive))
	}
}

// 自动判定增强人机实例:先认已挂 arena_bots.cfg 的那个,否则退回 match3。
func TestPickBotInstance(t *testing.T) {
	instances := []string{"main", "match1", "match3"}
	msmDir, home := fixtureLayout(t, instances)
	if got := pickBotInstance(msmDir, instances); got != "match3" {
		t.Fatalf("无标记时应退回 match3,实际 %q", got)
	}
	applyFixture(t, msmDir, home, instances, false)
	if got := pickBotInstance(msmDir, instances); got != "match3" {
		t.Fatalf("有标记时应认出 match3,实际 %q", got)
	}
	// 把标记挪到 match2:应改为认 match2
	mz := filepath.Join(host.InstanceDir(msmDir, "match2"), "game", "csgo", "cfg", "MatchZy")
	mustMkdir(t, mz)
	if err := os.WriteFile(filepath.Join(mz, "live_override.cfg"), []byte("exec arena_bots.cfg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pickBotInstance(msmDir, []string{"main", "match1", "match2"}); got != "match2" {
		t.Fatalf("应认出挂载在 match2,实际 %q", got)
	}
}
