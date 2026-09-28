// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package fsx 是文件系统小工具:原子写与按字节偏移的增量读取。
//
// 这里的语义都是踩坑修出来的,不要"顺手简化":
//   - 原子写:tmp + fsync + rename,实例侧读取方(MatchZy/引擎)不会看到半截文件;
//   - 增量读:只推进到最后一个完整行,末尾半行既不返回也不推进(否则行首永久丢失)。
package fsx

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// AtomicWriteText 原子写文本(UTF-8),返回写入字节数。
// tmp 名沿用 v1 的 `<目标>.tmp-<pid>`,便于排障时一眼看出是哪个进程留下的残留。
func AtomicWriteText(full string, data string) (int, error) {
	tmp := fmt.Sprintf("%s.tmp-%d", full, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	if _, err := f.WriteString(data); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, full); err != nil {
		return 0, err
	}
	return len(data), nil
}

// ReadNewLines 从字节偏移读取**完整行**,返回 (行, 下次偏移)。
//
// 末尾没有换行的半行(引擎正在写)既不返回、offset 也不推进过它 —— 下一轮从原处续读。
func ReadNewLines(path string, offset int64) (lines []string, next int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return nil, offset, err
		}
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, err
	}
	return splitCompleteLines(chunk, offset)
}

func splitCompleteLines(chunk []byte, offset int64) ([]string, int64, error) {
	end := -1
	for i := len(chunk) - 1; i >= 0; i-- {
		if chunk[i] == '\n' {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, offset, nil // 没有完整行:半行留给下一轮
	}
	complete := chunk[:end+1]
	return SplitLines(string(complete)), offset + int64(end) + 1, nil
}

// Python 的 str.splitlines() 行结束符集合 —— 引擎日志里出现过 \r 与 \x0b,
// 只按 \n 切会把两行粘成一行。
var lineBreaks = map[rune]bool{
	'\n': true, '\r': true, '\v': true, '\f': true,
	'\x1c': true, '\x1d': true, '\x1e': true, '\u0085': true,
	'\u2028': true, '\u2029': true,
}

// SplitLines 等价于 Python 的 splitlines():按行结束符切分,不含行尾符。
func SplitLines(s string) []string {
	var out []string
	var cur strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if lineBreaks[r] {
			if r == '\r' && i+1 < len(rs) && rs[i+1] == '\n' {
				i++ // \r\n 算一个
			}
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ReadTail 读日志尾 n 行。**行内含换行符**(与 v1 的 readlines()[−n:] 一致:
// 末行无换行也返回),因为后端/前端按原样回显。
func ReadTail(path string, lines int) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return TailLines(string(raw), lines), nil
}

// TailLines 把原始文本按 v1 readlines() 的语义切成"含行尾符"的行,取末尾 n 行。
// 文本模式下的 universal newlines:\r\n 与 \r 都归一为 \n。
func TailLines(text string, n int) []string {
	norm := strings.ReplaceAll(text, "\r\n", "\n")
	norm = strings.ReplaceAll(norm, "\r", "\n")
	if norm == "" {
		return nil
	}
	parts := strings.SplitAfter(norm, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1] // SplitAfter 的尾部空串不是一行
	}
	if n > 0 && len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	return parts // 行尾符原样保留(与 v1 的 readlines 一致)
}

// Itoa 便于日志/回包拼接 int。
func Itoa(n int) string { return strconv.Itoa(n) }
