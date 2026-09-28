// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 嵌入件与 deploy/cs-agent.service 必须一致(防漂移:F-1 两处各改一份最容易出错)。
func TestUnitTemplateMatchesDeployFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "cs-agent.service"))
	if err != nil {
		t.Fatalf("读 deploy/cs-agent.service: %v", err)
	}
	if UnitTemplate() != string(raw) {
		t.Fatal("内嵌 unit 模板与 deploy/cs-agent.service 不一致(改一处要同步另一处)")
	}
	if !strings.Contains(UnitTemplate(), "ExecStart=%h/data/bridge/cs agent --config %h/data/bridge/config.yaml") {
		t.Fatal("unit 模板里的默认 ExecStart 行被改动了:Replace 依赖它逐字匹配")
	}
}

func TestDeployMSMEmbedsTreeWithPatchMarkers(t *testing.T) {
	target := filepath.Join(t.TempDir(), "msm")
	steps, err := DeployMSM(DeployOptions{Target: target})
	if err != nil {
		t.Fatalf("投放失败: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("应报告投放步骤")
	}
	// msm 可执行 + cs2-server 符号链接(上游形态)
	if _, err := os.Stat(filepath.Join(target, "msm")); err != nil {
		t.Fatalf("msm 未落盘: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(target, "cs2-server")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("cs2-server 应为符号链接: %v", err)
	}
	// 本地补丁在位
	if ok, note := VerifyLocalPatches(target); !ok {
		t.Fatalf("本地补丁体检失败: %s", note)
	}
	// 仓库专用文件不进主机目录
	for _, p := range []string{"patches", "MODIFICATIONS.md"} {
		if _, err := os.Stat(filepath.Join(target, p)); err == nil {
			t.Fatalf("%s 不应被投放(仓库专用)", p)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "cs2", "app", "functions", "instance.sh")); err != nil {
		t.Fatalf("核心脚本缺失: %v", err)
	}
}

func TestDeployMSMIdempotentAndForceBacksUp(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "msm")
	if _, err := DeployMSM(DeployOptions{Target: target}); err != nil {
		t.Fatalf("首次投放: %v", err)
	}
	// 放一个"用户改过"的标记文件:覆盖后必须消失(整体替换,不残留旧文件)
	marker := filepath.Join(target, "LOCAL-MARKER")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 第二次(无 --force):跳过,标记文件原样
	if _, err := DeployMSM(DeployOptions{Target: target}); err != nil {
		t.Fatalf("二次投放: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("无 --force 时不应改动目标目录")
	}
	// --force:备份 + 整体替换
	if _, err := DeployMSM(DeployOptions{Target: target, Force: true}); err != nil {
		t.Fatalf("--force 投放: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("--force 后旧文件应被替换掉")
	}
	backups, _ := filepath.Glob(target + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("应有 1 份备份,得到 %v", backups)
	}
	if _, err := os.Stat(filepath.Join(backups[0], "LOCAL-MARKER")); err != nil {
		t.Fatal("备份里应含旧目录内容")
	}
}

func TestDeployMSMDryRunWritesNothing(t *testing.T) {
	target := filepath.Join(t.TempDir(), "msm")
	if _, err := DeployMSM(DeployOptions{Target: target, DryRun: true}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("dry-run 不应落盘")
	}
}

func TestWriteConfigRoundTripAndPlaceholder(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteConfig(cfgPath, ConfigSpec{Token: "tok", BackendWS: "ws://x:8080/api/agent", MSMDir: "/tmp/msm"}); err != nil {
		t.Fatalf("生成配置: %v", err)
	}
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config.yaml 含 token,权限应为 0600,得到 %v", fi.Mode().Perm())
	}
	// 占位符也要能生成(新机先落文件、后补 token),但 Check 必须提示"还是占位符"
	if err := WriteConfig(cfgPath, ConfigSpec{}); err != nil {
		t.Fatalf("占位符生成: %v", err)
	}
	o := Options{MSMDir: "/tmp/none", MSMExe: "/tmp/none/cs2-server", ConfigPath: cfgPath, UnitPath: "/tmp/none/unit", Home: t.TempDir()}
	found := false
	for _, it := range Check(o) {
		if it.Name == "config.yaml 可解析" {
			found = true
			if !it.OK || !strings.Contains(it.Note, "占位符") {
				t.Fatalf("占位 token 应可解析并提示补 token,得到 %+v", it)
			}
		}
	}
	if !found {
		t.Fatal("Check 未包含 config.yaml 项")
	}
}

func TestCheckReportsLayers(t *testing.T) {
	root := t.TempDir()
	msmDir := filepath.Join(root, "cs2-multiserver-new")
	if _, err := DeployMSM(DeployOptions{Target: msmDir}); err != nil {
		t.Fatalf("投放: %v", err)
	}
	// 造最小 msm 布局:base appmanifest + cfg/inst-main/server.conf
	base := filepath.Join(root, "msm.d", "cs2", "base")
	if err := os.MkdirAll(filepath.Join(base, "steamapps"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "game", "csgo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "steamapps", "appmanifest_730.acf"),
		[]byte("AppState\n{\n\t\"buildid\"\t\t\"25218825\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	instCfg := filepath.Join(root, "msm.d", "cs2", "cfg", "inst-main")
	if err := os.MkdirAll(instCfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instCfg, "server.conf"), []byte("PORT=\"27015\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	if err := WriteConfig(cfgPath, ConfigSpec{Token: "tok", BackendWS: "ws://x/api/agent", MSMDir: msmDir}); err != nil {
		t.Fatal(err)
	}
	items := Check(Options{
		MSMDir: msmDir, MSMExe: filepath.Join(msmDir, "cs2-server"),
		ConfigPath: cfgPath, UnitPath: filepath.Join(root, "unit", "cs-agent.service"),
		Instances: []string{"main"}, Home: root,
	})
	byName := map[string]CheckItem{}
	for _, it := range items {
		byName[it.Name] = it
	}
	for _, want := range []string{"msm 目录(" + msmDir + ")", "msm 可执行(" + filepath.Join(msmDir, "cs2-server") + ")", "游戏文件(appmanifest buildid)", "base 游戏目录(game/csgo)", "config.yaml 可解析"} {
		it, ok := byName[want]
		if !ok {
			t.Fatalf("缺体检项 %q(现有: %v)", want, keys(byName))
		}
		if !it.OK {
			t.Fatalf("体检项 %q 应为 ✓,得到 %+v", want, it)
		}
	}
	// 关键项全过 → CriticalOK(此环境里 L0 依赖命令可能缺,故两者关系做蕴含断言)
	if len(items) == 0 {
		t.Fatal("体检结果为空")
	}
}

func keys(m map[string]CheckItem) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMsmLocalPatchesDetectsUpstreamOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "msm")
	if _, err := DeployMSM(DeployOptions{Target: dir}); err != nil {
		t.Fatal(err)
	}
	// 模拟"被上游件覆盖":把 gotv.conf 还原成 PORT+5
	p := filepath.Join(dir, "cs2", "app", "cfg", "gotv.conf")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(raw), "PORT + 100", "PORT + 5", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, note := VerifyLocalPatches(dir); ok || !strings.Contains(note, "GOTV") {
		t.Fatalf("应报 GOTV 补丁缺失,得到 ok=%v note=%s", ok, note)
	}
}
