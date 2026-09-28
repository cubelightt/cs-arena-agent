// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package host

import (
	"os"
	"path/filepath"
	"testing"
)

// buildid 从 appmanifest 的 Valve KV 文本里取(真机格式见 Valve key/value manifest format)。
func TestReadBuildID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "appmanifest_730.acf")
	body := "AppState\n{\n\t\"appid\"\t\t\"730\"\n\t\"SizeOnDisk\"\t\t\"71099106130\"\n\t\"buildid\"\t\t\"25218825\"\n}\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBuildID(p)
	if err != nil || got != "25218825" {
		t.Fatalf("buildid 解析失败: %q %v", got, err)
	}
	if _, err := ReadBuildID(filepath.Join(dir, "missing.acf")); err == nil {
		t.Fatal("缺文件应报错")
	}
}

// 路径推导与 v1 的默认模板同值(msm_dir/../msm.d/cs2/...)——迁移期两边必须一致。
func TestLayoutPaths(t *testing.T) {
	msmDir := "/home/testuser/cs2-multiserver-new"
	if got := BaseDir(msmDir); got != "/home/testuser/msm.d/cs2/base" {
		t.Fatalf("base 目录: %s", got)
	}
	if got := ManifestPath(msmDir); got != "/home/testuser/msm.d/cs2/base/steamapps/appmanifest_730.acf" {
		t.Fatalf("appmanifest: %s", got)
	}
	if got := TmuxSocket(msmDir, "main"); got != "/home/testuser/msm.d/cs2/inst-main/msm.d/tmp/server.tmux-socket" {
		t.Fatalf("tmux socket: %s", got)
	}
	if got := InstanceLogDir(msmDir, "match3"); got != "/home/testuser/msm.d/cs2/log/inst-match3" {
		t.Fatalf("日志目录: %s", got)
	}
}

func TestDirSizeAndTopEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "big"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "small"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big", "a.bin"), make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "small", "b.bin"), make([]byte, 16), 0o644); err != nil {
		t.Fatal(err)
	}
	size, count := DirSize(dir)
	if size != 2064 || count != 2 {
		t.Fatalf("DirSize = %d / %d", size, count)
	}
	top := TopEntries(dir, 1)
	if len(top) != 1 || filepath.Base(top[0].Path) != "big" {
		t.Fatalf("TopEntries = %+v", top)
	}
	if HumanBytes(15<<30) != "15.0 G" {
		t.Fatalf("HumanBytes = %s", HumanBytes(15<<30))
	}
}
