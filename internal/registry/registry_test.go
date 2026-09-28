// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// 端口真源 = cfg/inst-*/server.conf 的 PORT=(不是弱默认 __PORT__);
// GOTV = PORT + 100(gotv.conf 默认),显式写字面值时以后者为准。
func TestScanPortsAndGotv(t *testing.T) {
	root := t.TempDir()
	msmDir := filepath.Join(root, "cs2-multiserver-new")
	cfgDir := filepath.Join(root, "msm.d", "cs2", "cfg")
	mustWrite(t, filepath.Join(cfgDir, "inst-main", "server.conf"), "__PORT__=\"27015\"\nPORT=\"27015\"\n")
	mustWrite(t, filepath.Join(cfgDir, "inst-match1", "server.conf"), "__PORT__=\"27015\"\nPORT=\"27016\"\n")
	mustWrite(t, filepath.Join(cfgDir, "inst-gotv", "server.conf"), "PORT=\"27020\"\n")
	mustWrite(t, filepath.Join(cfgDir, "inst-gotv", "gotv.conf"), "TV_PORT=\"27222\"\n")
	mustWrite(t, filepath.Join(cfgDir, "defaults.conf"), "PORT=\"1\"\n") // 非 inst-* 目录必须忽略
	if err := os.MkdirAll(msmDir, 0o755); err != nil {
		t.Fatal(err)
	}

	items, err := Scan(msmDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("实例数应为 3,得到 %d: %+v", len(items), items)
	}
	byName := map[string]Instance{}
	for _, it := range items {
		byName[it.Name] = it
	}
	if byName["main"].Port != 27015 || byName["main"].GotvPort != 27115 {
		t.Fatalf("main 端口解析错误: %+v", byName["main"])
	}
	if byName["match1"].Port != 27016 || byName["match1"].GotvPort != 27116 {
		t.Fatalf("match1 端口解析错误: %+v", byName["match1"])
	}
	if byName["gotv"].GotvPort != 27222 {
		t.Fatalf("显式 TV_PORT 未生效: %+v", byName["gotv"])
	}
}

// clone 出来的 server.conf 里有**两条** PORT=(msm 用"末尾追加强覆盖"改端口,clone 是
// create 追加 → 来源配置覆盖 → 再追加)。真端口 = **最后**一条;取第一条会读到来源实例的端口。
func TestScanPrefersLastPort(t *testing.T) {
	root := t.TempDir()
	msmDir := filepath.Join(root, "cs2-multiserver-new")
	cfgDir := filepath.Join(root, "msm.d", "cs2", "cfg")
	// 来源实例(手改过一次端口,末尾留强覆盖)
	mustWrite(t, filepath.Join(cfgDir, "inst-main", "server.conf"), "__PORT__=\"27015\"\nPORT=\"27015\"\n")
	// 从 main 克隆出来的新实例:默认配置 + 来源整体覆盖 + 末尾再追加新端口
	mustWrite(t, filepath.Join(cfgDir, "inst-arena1", "server.conf"),
		"__PORT__=\"27015\"\nSM_SWIFTLYS2=0\n\n# 手动分配端口避免实例间冲突(2026-08-11)\nPORT=\"27015\"\n"+
			"\n# Auto-assigned by cs2-server create/clone to avoid colliding with other instances\nPORT=\"27119\"\n")
	// gotv.conf 同样"后赋值生效":末尾的字面值必须覆盖前面的
	mustWrite(t, filepath.Join(cfgDir, "inst-arena1", "gotv.conf"), "TV_PORT=\"27215\"\nTV_PORT=\"27219\"\n")
	if err := os.MkdirAll(msmDir, 0o755); err != nil {
		t.Fatal(err)
	}

	items, err := Scan(msmDir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Instance{}
	for _, it := range items {
		byName[it.Name] = it
	}
	got := byName["arena1"]
	if got.Port != 27119 {
		t.Fatalf("回读端口必须取最后一条 PORT=(期望 27119,得到 %d —— 说明读到了来源实例的端口): %+v", got.Port, got)
	}
	if got.GotvPort != 27219 {
		t.Fatalf("回读 GOTV 必须取最后一条 TV_PORT=(期望 27219,得到 %d): %+v", got.GotvPort, got)
	}
	if byName["main"].Port != 27015 {
		t.Fatalf("来源实例端口不应受影响: %+v", byName["main"])
	}
}

// 已有编号保留、新实例追加 —— 重扫不重排(cs status 的 #号必须稳定)。
func TestMergeIdxStable(t *testing.T) {
	previous := []Instance{{Idx: 2, Name: "main"}, {Idx: 7, Name: "match1"}}
	scanned := []Instance{{Name: "match1"}, {Name: "main"}, {Name: "arena1"}}
	merged := MergeIdx(scanned, previous, 0)
	got := map[string]int{}
	for _, it := range merged {
		got[it.Name] = it.Idx
	}
	if got["main"] != 2 || got["match1"] != 7 || got["arena1"] != 8 {
		t.Fatalf("编号合并错误: %+v", got)
	}
}

// 编号高水位:删除后编号不回收(floorMax 来自 File.MaxIdx)。
func TestMergeIdxNoReuse(t *testing.T) {
	scanned := []Instance{{Name: "main"}, {Name: "match1"}}
	// match1 曾拿过 #7(现已从注册表删除)→ 新实例必须从 8 起,不得复用 7
	merged := MergeIdx(scanned, []Instance{{Idx: 2, Name: "main"}}, 7)
	got := map[string]int{}
	for _, it := range merged {
		got[it.Name] = it.Idx
	}
	if got["main"] != 2 || got["match1"] != 8 {
		t.Fatalf("编号不回收规则被破坏: %+v", got)
	}
}

// 注册表文件:Append 取号/Remove 不回收/落盘读回一致(原子写)。
func TestFileAppendRemoveAndPersist(t *testing.T) {
	var f File
	f, a := Append(f, "main", 27015, 27115)
	f, b := Append(f, "match1", 27016, 27116)
	if a.Idx != 1 || b.Idx != 2 {
		t.Fatalf("编号分配错误: %+v %+v", a, b)
	}
	// 同名再 Append → 只更新端口,编号不变
	f, again := Append(f, "main", 27025, 27125)
	if again.Idx != 1 || len(f.Items) != 2 {
		t.Fatalf("同名 Append 应保留编号: %+v %+v", again, f.Items)
	}
	f = Remove(f, "main")
	if len(f.Items) != 1 || f.Items[0].Name != "match1" || f.MaxIdx != 2 {
		t.Fatalf("Remove 后状态错误: %+v", f)
	}
	// 删除后新实例取 #3(不回收到 #1)
	f, c := Append(f, "arena1", 27017, 27117)
	if c.Idx != 3 {
		t.Fatalf("删除后的编号不得回收: %+v", c)
	}

	path := filepath.Join(t.TempDir(), "registry.json")
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	back := Load(path)
	if back.MaxIdx != 3 || len(back.Items) != 2 {
		t.Fatalf("落盘读回不一致: %+v", back)
	}
	if back.Items[1].Name != "arena1" || back.Items[1].Port != 27017 {
		t.Fatalf("落盘条目内容错误: %+v", back.Items)
	}
	// 文件不存在 / 内容损坏都按空表(不阻断启动)
	if empty := Load(filepath.Join(t.TempDir(), "nope.json")); len(empty.Items) != 0 || empty.MaxIdx != 0 {
		t.Fatalf("缺失文件应为空表: %+v", empty)
	}
	badPath := filepath.Join(t.TempDir(), "bad.json")
	mustWrite(t, badPath, "{not json")
	if bad := Load(badPath); len(bad.Items) != 0 {
		t.Fatalf("损坏文件应为空表: %+v", bad)
	}
}

func TestResolveByNameAndIdx(t *testing.T) {
	items := []Instance{{Idx: 1, Name: "main"}, {Idx: 2, Name: "match1"}}
	if it, err := Resolve(items, "2"); err != nil || it.Name != "match1" {
		t.Fatalf("按编号解析失败: %+v %v", it, err)
	}
	if it, err := Resolve(items, "main"); err != nil || it.Idx != 1 {
		t.Fatalf("按名称解析失败: %+v %v", it, err)
	}
	if _, err := Resolve(items, "nope"); err == nil {
		t.Fatal("未知实例名应报错")
	}
	if _, err := Resolve(items, "9"); err == nil {
		t.Fatal("未知编号应报错")
	}
}

func TestPortConflict(t *testing.T) {
	ok := []Instance{{Idx: 1, Name: "main", Port: 27015, GotvPort: 27115}, {Idx: 2, Name: "m1", Port: 27016, GotvPort: 27116}}
	if c := PortConflict(ok); len(c) != 0 {
		t.Fatalf("不该报冲突: %v", c)
	}
	// 实例 A 的 GOTV 撞实例 B 的游戏端口
	bad := []Instance{{Idx: 1, Name: "main", Port: 27015, GotvPort: 27016}, {Idx: 2, Name: "m1", Port: 27016, GotvPort: 27116}}
	if c := PortConflict(bad); len(c) == 0 {
		t.Fatal("端口冲突应被检出")
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 墓碑:cs del 删掉的名字进 Removed(守护上报 action:'removed');同名重建时清除。
func TestFileTombstones(t *testing.T) {
	var f File
	f, _ = Append(f, "main", 27015, 27115)
	f, _ = Append(f, "match1", 27016, 27116)
	f = Remove(f, "match1")
	if len(f.Removed) != 1 || f.Removed[0] != "match1" || f.MaxIdx != 2 {
		t.Fatalf("Remove 应记墓碑且编号不回收: %+v", f)
	}
	// 重复删除不重复记
	f = Remove(f, "match1")
	if len(f.Removed) != 1 {
		t.Fatalf("重复 Remove 不应重复记墓碑: %+v", f.Removed)
	}
	// 同名重建 → 墓碑清除,且拿新编号(#3,不复用 #2)
	f, again := Append(f, "match1", 27016, 27116)
	if again.Idx != 3 || len(f.Removed) != 0 {
		t.Fatalf("同名重建应清墓碑并取新编号: %+v %+v", again, f.Removed)
	}
	// 落盘读回:墓碑随文件持久化
	path := filepath.Join(t.TempDir(), "registry.json")
	f = Remove(f, "main")
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	if back := Load(path); len(back.Removed) != 1 || back.Removed[0] != "main" {
		t.Fatalf("墓碑应随文件持久化: %+v", back)
	}
}
