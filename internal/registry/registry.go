// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package registry 维护"实例编号注册表":实例名 ↔ 编号 ↔ 端口。
//
// 端口与实例清单的**真源是主机 msm 布局**(`cfg/inst-<name>/server.conf` 的 `PORT`,
// GOTV = PORT + 100),不解析 msm 的 ANSI 文本输出。
// 编号(idx)是桥侧概念:缺失时按名称序补,已有则保留(重扫不重排,避免 `cs status` 里的 #号跳动);
// 已分配的编号落 `<config_dir>/registry.json`(M5:`cs new`/`cs del` 维护,**删除不回收**)。
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"arena/agent/internal/fsx"
)

// Instance 是注册表里的一行。
type Instance struct {
	Idx      int    `json:"idx"`
	Name     string `json:"name"`
	Port     int    `json:"port"`
	GotvPort int    `json:"gotvPort"`
}

var (
	confPORTRe   = regexp.MustCompile(`(?m)^\s*PORT="?([0-9]{2,5})"?\s*$`)
	confTVPORTRe = regexp.MustCompile(`(?m)^\s*TV_PORT="?([0-9]{2,5})"?\s*$`)
	confNameRe   = regexp.MustCompile(`^inst-([\w.-]{1,32})$`)
)

// lastNumber 返回 raw 中**最后**一条匹配的数值(无匹配或不可解析 → 0)。
//
// 为什么取最后一条而不是第一条:server.conf/gotv.conf 是 **bash 源文件**(后赋值覆盖先赋值),
// msm 的 `App::assignInstancePort` 正是靠这一点把"强 `PORT=` 覆盖"**追加在文件末尾**;
// 而 clone 的落地顺序是"create(追加一次)→ 用来源配置整体覆盖(带回来源的 PORT=)→ 再追加一次"
// —— 所以真机 clone 出来的 server.conf 里有**两条** PORT=,真端口是最后那条。
// 取第一条会读到**来源实例**的端口(cs new 会把新实例登记成来源的端口 → 与来源撞端口)。
func lastNumber(re *regexp.Regexp, raw []byte) int {
	ms := re.FindAllSubmatch(raw, -1)
	if len(ms) == 0 {
		return 0
	}
	p, err := strconv.Atoi(string(ms[len(ms)-1][1]))
	if err != nil {
		return 0
	}
	return p
}

// CFGDir 返回 msm 布局下的实例配置目录(<msm_dir>/../msm.d/cs2/cfg)。
func CFGDir(msmDir string) string {
	return filepath.Clean(filepath.Join(msmDir, "..", "msm.d", "cs2", "cfg"))
}

// Scan 扫描 cfg/inst-*/ 得到实例列表(按名称排序;端口读不到 → 0 并保留条目)。
func Scan(msmDir string) ([]Instance, error) {
	root := CFGDir(msmDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("读取实例配置目录失败: %s(%v)", root, err)
	}
	var out []Instance
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m := confNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		name := m[1]
		inst := Instance{Name: name}
		dir := filepath.Join(root, e.Name())
		if raw, err := os.ReadFile(filepath.Join(dir, "server.conf")); err == nil {
			if p := lastNumber(confPORTRe, raw); p > 0 {
				inst.Port = p
			}
		}
		inst.GotvPort = inst.Port + 100 // msm 默认 gotv.conf:TV_PORT=$(( PORT + 100 ))
		if raw, err := os.ReadFile(filepath.Join(dir, "gotv.conf")); err == nil {
			if p := lastNumber(confTVPORTRe, raw); p > 0 { // 显式写字面值时以后者为准
				inst.GotvPort = p
			}
		}
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// MergeIdx 把扫描结果与已有注册表合并:**已有实例保留原编号**,新实例按 max(idx)+1 追加。
// floorMax = 曾分配过的最大编号(File.MaxIdx;编号作废不回收 —— 传 0 表示未记录,
// 此时退化为仅按 previous 推断,与 M2 行为一致)。
func MergeIdx(scanned, previous []Instance, floorMax int) []Instance {
	prev := map[string]int{}
	maxIdx := floorMax
	for _, p := range previous {
		if p.Name == "" || p.Idx <= 0 {
			continue
		}
		prev[p.Name] = p.Idx
		if p.Idx > maxIdx {
			maxIdx = p.Idx
		}
	}
	out := make([]Instance, 0, len(scanned))
	for _, s := range scanned {
		if idx, ok := prev[s.Name]; ok {
			s.Idx = idx
		} else {
			maxIdx++
			s.Idx = maxIdx
		}
		out = append(out, s)
	}
	return out
}

// File 是 <config_dir>/registry.json 的落盘形状:编号高水位 + 已编号实例。
//
// 为什么不放 state.json:守护在 hello_ack / config_sync 时会**整份重写** state.json,
// 而 `cs new`/`cs del` 是另一个进程 —— 写进 state.json 的编号会被守护的下一次保存覆盖。
// 独立文件 + 原子写,两端都"现读现并",不存在互相覆盖(与 M4 把待上报状态放 jobs/ 同理)。
type File struct {
	// MaxIdx 是曾分配过的最大编号(删除不回收;新实例从 MaxIdx+1 起)
	MaxIdx int        `json:"maxIdx"`
	Items  []Instance `json:"items"`
	// Removed 是"本机已删除"的实例名(墓碑;`cs del` 写入,同名重建时由 Append 清除)。
	// 用途:守护在 reconcile 上报里带 action:'removed',平台据此收敛掉 DB 行 ——
	// 与 M3 的"缺报不自动删行"不冲突:那是主机没上报(可能只是桥没连上),
	// 这里是主机侧**显式执行过删除**的记录。
	Removed []string `json:"removed,omitempty"`
}

// PathFor 返回注册表文件路径(与 config.yaml 同目录,便于整目录备份/迁移)。
func PathFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "registry.json")
}

// Load 读注册表文件;文件不存在或损坏都按空表处理(不阻断启动,编号会重排)。
func Load(path string) File {
	var f File
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return File{}
	}
	return f
}

// Save 原子落盘(tmp+fsync+rename,与 config.yaml/state.json 同款)。
func Save(path string, f File) error {
	if f.Items == nil {
		f.Items = []Instance{}
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	_, err = fsx.AtomicWriteText(path, string(raw)+"\n")
	return err
}

// Append 给新实例分配编号(已分配最大值 +1)并返回更新后的注册表;同名已存在则只更新端口、保留编号。
func Append(f File, name string, port, gotvPort int) (File, Instance) {
	f, inst, _ := AppendAt(f, name, port, gotvPort, 0)
	return f, inst
}

// AppendAt 同 Append,但可指定编号(wantIdx=0 表示自动取号)。指定编号已被占用 → 报错
// (供 `cs new --idx N` 用;编号作废不回收的规则不因显式指定而放宽:必须 > 已分配最大值)。
func AppendAt(f File, name string, port, gotvPort, wantIdx int) (File, Instance, error) {
	for i := range f.Items {
		if f.Items[i].Name == name {
			f.Items[i].Port, f.Items[i].GotvPort = port, gotvPort
			if f.Items[i].Idx > f.MaxIdx {
				f.MaxIdx = f.Items[i].Idx
			}
			if wantIdx != 0 && wantIdx != f.Items[i].Idx {
				return f, f.Items[i], fmt.Errorf("实例 %s 已占用编号 #%d,不能改为 #%d", name, f.Items[i].Idx, wantIdx)
			}
			return f, f.Items[i], nil
		}
		if wantIdx != 0 && f.Items[i].Idx == wantIdx {
			return f, Instance{}, fmt.Errorf("编号 #%d 已被实例 %s 占用", wantIdx, f.Items[i].Name)
		}
	}
	idx := f.MaxIdx + 1
	if wantIdx != 0 {
		if wantIdx <= f.MaxIdx {
			return f, Instance{}, fmt.Errorf("编号 #%d 已作废不回收(当前已分配最大编号 %d);请用更大的编号或不指定", wantIdx, f.MaxIdx)
		}
		idx = wantIdx
	}
	if idx > f.MaxIdx {
		f.MaxIdx = idx
	}
	inst := Instance{Idx: idx, Name: name, Port: port, GotvPort: gotvPort}
	f.Removed = removeName(f.Removed, name) // 同名重建 → 清墓碑(否则下次上报又收敛一次"已删除")
	f.Items = append(f.Items, inst)
	return f, inst, nil
}

// removeName 从名字列表里删掉一个(保序)。
func removeName(names []string, name string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != name {
			out = append(out, n)
		}
	}
	return out
}

// Sync 把"扫描结果 ∪ 文件已有编号"合并后的**完整编号表**写回文件内容:
// 首次落盘时把既有实例一并记上(否则文件里只有新实例,既有实例的编号会随名称序漂移);
// 同时清掉主机上已不存在的条目(编号高水位保留 —— 作废不回收)。
func Sync(f File, scanned []Instance) File {
	merged := MergeIdx(scanned, f.Items, f.MaxIdx)
	maxIdx := f.MaxIdx
	for _, it := range merged {
		if it.Idx > maxIdx {
			maxIdx = it.Idx
		}
	}
	f.Items = merged
	f.MaxIdx = maxIdx
	return f
}

// Remove 删除条目;**编号作废不回收**(MaxIdx 不回退,后续新实例继续往上取号)。
func Remove(f File, name string) File {
	out := make([]Instance, 0, len(f.Items))
	for _, it := range f.Items {
		if it.Name != name {
			out = append(out, it)
		}
	}
	f.Items = out
	// 记墓碑:守护下次 reconcile 上报带 action:'removed',平台据此删掉 DB 行
	if name != "" {
		f.Removed = removeName(f.Removed, name)
		f.Removed = append(f.Removed, name)
	}
	return f
}

// Resolve 按"编号或名称"解析实例;arg 为空时返回 nil(调用方自行决定语义,如 all)。
func Resolve(items []Instance, arg string) (*Instance, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil, fmt.Errorf("需要实例编号或名称")
	}
	if n, err := strconv.Atoi(arg); err == nil {
		for i := range items {
			if items[i].Idx == n {
				return &items[i], nil
			}
		}
		return nil, fmt.Errorf("没有编号为 #%d 的实例", n)
	}
	for i := range items {
		if items[i].Name == arg {
			return &items[i], nil
		}
	}
	return nil, fmt.Errorf("没有名为 %s 的实例", arg)
}

// Names 返回全部实例名(注册表顺序)。
func Names(items []Instance) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

// PortConflict 检查端口/GOTV 是否在注册表内重复(启动前自检用)。
func PortConflict(items []Instance) []string {
	seen := map[int]string{}
	var out []string
	for _, it := range items {
		for _, p := range []int{it.Port, it.GotvPort} {
			if p == 0 {
				continue
			}
			if owner, ok := seen[p]; ok && owner != it.Name {
				out = append(out, fmt.Sprintf("端口 %d 被 %s 与 %s 同时使用", p, owner, it.Name))
				continue
			}
			seen[p] = it.Name
		}
	}
	return out
}
