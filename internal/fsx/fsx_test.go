// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitLinesMatchesPythonSplitlines(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a\nb\n", []string{"a", "b"}},
		{"a\nb", []string{"a", "b"}},
		{"a\r\nb", []string{"a", "b"}},       // \r\n 算一个结束符
		{"a\rb", []string{"a", "b"}},         // \r 单独也算
		{"a\vb\fc", []string{"a", "b", "c"}}, // 引擎日志里出现过 \v/\f
		{"", nil},
	}
	for _, c := range cases {
		got := SplitLines(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("SplitLines(%q) = %q, want %q", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("SplitLines(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	}
}

// 半行(引擎正在写)既不返回也不推进 offset —— 否则行首会永久丢失。
func TestReadNewLinesKeepsPartialLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "console.log")
	if err := os.WriteFile(p, []byte("PARTIAL-HEAD"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, off, err := ReadNewLines(p, 0)
	if err != nil || len(lines) != 0 || off != 0 {
		t.Fatalf("半行不该返回: lines=%q off=%d err=%v", lines, off, err)
	}
	if err := os.WriteFile(p, []byte("PARTIAL-HEAD-TAIL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, off2, err := ReadNewLines(p, off)
	if err != nil || len(lines) != 1 || lines[0] != "PARTIAL-HEAD-TAIL" || off2 != int64(len("PARTIAL-HEAD-TAIL\n")) {
		t.Fatalf("续读应拿到整行: lines=%q off=%d err=%v", lines, off2, err)
	}
}

func TestAtomicWriteCleanAndComplete(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "arena_bots.cfg")
	n, err := AtomicWriteText(target, "bot_quota 0\n中文\n")
	if err != nil {
		t.Fatal(err)
	}
	back, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != "bot_quota 0\n中文\n" || n != len(string(back)) {
		t.Fatalf("内容不符: %q n=%d", string(back), n)
	}
	leftovers, _ := filepath.Glob(target + ".tmp-*")
	if len(leftovers) != 0 {
		t.Fatalf("残留临时文件: %v", leftovers)
	}
}

// 尾段读的"行内含换行符"语义与 v1 readlines()[−n:] 一致(后端/前端按原样回显)。
func TestTailLinesKeepsLineEndings(t *testing.T) {
	got := TailLines("a\nb\nc", 2)
	if len(got) != 2 || got[0] != "b\n" || got[1] != "c" {
		t.Fatalf("TailLines = %q", got)
	}
}
