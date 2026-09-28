// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// arena.go 实现 `cs install --arena-patches`:主机侧补丁(M6 / README.md 清单)的
// **幂等重做**。
//
// CS2 更新可能还原人机配置与游戏资源路径；本命令修复配置和共享链接。
// 插件由用户自行安装，本命令不分发、安装或覆盖插件 DLL。
//
// 两条纪律:
//  1. 幂等 —— 先判"在位"(sha256 / 标记 / 值),在位就跳过;重复执行不产生任何写入与 .bak;
//  2. 可回退 —— 任何改写先把原文件备份为同名 `.bak-<stamp>`(与既有发布脚本同款命名)。
package install

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"arena/agent/internal/host"
)

// DefaultBotInstance 是增强人机专用实例(人机 cfg 链 / gamemode 配额 / gameinfo 只做它一份)。
const DefaultBotInstance = "match3"

// neutralArenaBotsCfg 与后端非人机场次写入的内容一致(lib/instances.js),
// 让 cfg 链的 `exec arena_bots.cfg` 不报错、且不留上场人机指令。
const neutralArenaBotsCfg = "// arena: no bots (non-bot match)\n"

// ArenaPatchOptions 是补丁重做的入参(全部可注入,便于测试)。
type ArenaPatchOptions struct {
	MSMDir      string   // msm 根(可含 ~)
	Instances   []string // 实例名清单(来自注册表扫描)
	BotInstance string   // 增强人机实例;空 = 自动判定(见 pickBotInstance)
	Home        string
	DryRun      bool
	Log         func(format string, args ...any)
}

// ArenaPatchItem 是一条补丁项的结果。
// Action: ok=本来就位 / fixed=本次修复(dry-run 时为"将修复")/ warn=需人工处理 / skip=环境不适用。
type ArenaPatchItem struct {
	Item   string `json:"item"`
	Where  string `json:"where"`
	Action string `json:"action"`
	Note   string `json:"note,omitempty"`
}

// Changed 报告本次是否真的改动了什么(排除 dry-run 与 warn/skip)。
func Changed(items []ArenaPatchItem) int {
	n := 0
	for _, it := range items {
		if it.Action == "fixed" {
			n++
		}
	}
	return n
}

type patchCtx struct {
	o      ArenaPatchOptions
	msmDir string
	home   string
	stamp  string
	items  []ArenaPatchItem
	errs   []string
}

// ApplyArenaPatches 应用主机侧补丁并返回逐项结果。
// 单项失败不中断其余项(结果里记 warn,末尾汇总为 error)。
func ApplyArenaPatches(o ArenaPatchOptions) ([]ArenaPatchItem, error) {
	home := o.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	c := &patchCtx{
		o:      o,
		msmDir: expandHome(o.MSMDir, home),
		home:   home,
		stamp:  time.Now().Format("20060102150405"),
	}
	if c.o.Log == nil {
		c.o.Log = func(string, ...any) {}
	}

	bot := o.BotInstance
	if bot == "" {
		bot = pickBotInstance(c.msmDir, o.Instances)
	}
	if bot != "" {
		c.stepBotCfgChain(bot)
		c.stepGamemodeQuota(bot)
		c.stepGameInfo(bot)
		c.stepArenaBotsFile(bot)
	}

	c.stepLogsSymlinks()
	c.stepWorkshopShare()

	if len(c.errs) > 0 {
		return c.items, fmt.Errorf("%d 项未完成: %s", len(c.errs), strings.Join(c.errs, "; "))
	}
	return c.items, nil
}

// ---- 3) 人机 cfg 链(match3)---------------------------------------------------

// botCfgBlock 生成追加块:与 2026-08-28 固化时写入主机的内容**逐字节一致**
// (warmup.cfg 带 bot-improver 注释,live 两份带 `sv_cheats 1`;my_bot_*.cfg 只在
// 该实例确实装了 bot mod 时才 exec,免得在纯比赛实例上报 cfg 不存在)。
//
// leadingBlank 决定块前是否加一个空行作分隔:stock 的 live_override/live_wingman
// 本身就是"一个空行"(1 字节),再加前导换行会多出一个空行(主机环境核对 2026-09-22)。
func botCfgBlock(file string, hasMyBot, leadingBlank bool) string {
	var b strings.Builder
	if leadingBlank {
		b.WriteString("\n")
	}
	if file == "warmup.cfg" {
		if hasMyBot {
			b.WriteString("// bot-improver: restore sv_cheats cvars & bot tuning\n")
		}
	} else {
		b.WriteString("sv_cheats 1\n")
	}
	if hasMyBot {
		b.WriteString("exec my_bot_normal_config.cfg\n")
	}
	b.WriteString("bot_quota_mode normal\n")
	b.WriteString("bot_quota 0\n\n")
	b.WriteString("// arena: bot placement (cfg rewritten per match by backend)\n")
	b.WriteString("exec arena_bots.cfg\n")
	return b.String()
}

// needsLeadingBlank 判断追加前是否需要补一个空行:文件为空、本身就是空行、
// 或已经以空行结尾时都不补(否则会多出空行)。
func needsLeadingBlank(content []byte) bool {
	s := string(content)
	return s != "" && s != "\n" && !strings.HasSuffix(s, "\n\n")
}

func (c *patchCtx) stepBotCfgChain(bot string) {
	inst := host.InstanceDir(c.msmDir, bot)
	hasMyBot := fileExists(filepath.Join(inst, "game", "csgo", "cfg", "my_bot_normal_config.cfg"))
	for _, name := range []string{"warmup.cfg", "live_override.cfg", "live_wingman_override.cfg"} {
		p := filepath.Join(inst, "game", "csgo", "cfg", "MatchZy", name)
		raw, err := os.ReadFile(p)
		if err != nil {
			c.add("人机 cfg 链", "@"+bot+"/"+name, "skip", "文件不存在(MatchZy 未装?)")
			continue
		}
		if bytes.Contains(raw, []byte("exec arena_bots.cfg")) {
			c.add("人机 cfg 链", "@"+bot+"/"+name, "ok", "已挂载 exec arena_bots.cfg")
			continue
		}
		body := append(append([]byte{}, raw...), []byte(botCfgBlock(name, hasMyBot, needsLeadingBlank(raw)))...)
		if err := c.backupAndWrite(p, body); err != nil {
			c.fail("人机 cfg 链 @"+bot+"/"+name, err)
			continue
		}
		c.add("人机 cfg 链", "@"+bot+"/"+name, "fixed", "末尾追加 bot 就位块(bot_quota 0 + exec arena_bots.cfg)")
	}
}

// ---- 4) gamemode 配额归零(match3)--------------------------------------------

var (
	reBotQuota     = regexp.MustCompile(`(?i)^(\s*)bot_quota(\s+)(\S+)(\s*)$`)
	reBotQuotaMode = regexp.MustCompile(`(?i)^(\s*)bot_quota_mode(\s+)(\S+)(\s*)$`)
)

// zeroBotQuota 把 `bot_quota` 置 0、`bot_quota_mode` 置 normal(保留缩进/行尾);
// 两行都缺时补在文件末尾。changed=false 表示本来就对(幂等判定点)。
func zeroBotQuota(raw []byte) (out []byte, changed bool, note string) {
	lines := splitLinesKeepEOL(string(raw))
	seenQuota, seenMode := false, false
	fixed := 0
	for i, ln := range lines {
		body, eol := trimEOL(ln)
		if m := reBotQuota.FindStringSubmatch(body); m != nil {
			seenQuota = true
			if m[3] != "0" {
				lines[i] = m[1] + "bot_quota" + m[2] + "0" + m[4] + eol
				fixed++
			}
			continue
		}
		if m := reBotQuotaMode.FindStringSubmatch(body); m != nil {
			seenMode = true
			if !strings.EqualFold(m[3], "normal") {
				lines[i] = m[1] + "bot_quota_mode" + m[2] + "normal" + m[4] + eol
				fixed++
			}
		}
	}
	tail := ""
	if !seenQuota {
		tail += "bot_quota 0\n"
		fixed++
	}
	if !seenMode {
		tail += "bot_quota_mode normal\n"
		fixed++
	}
	if fixed == 0 {
		return raw, false, "bot_quota 0 / bot_quota_mode normal 已在位"
	}
	res := strings.Join(lines, "")
	if tail != "" && !strings.HasSuffix(res, "\n") && res != "" {
		res += "\n"
	}
	return []byte(res + tail), true, fmt.Sprintf("引擎侧配额已归零(%d 处)", fixed)
}

func (c *patchCtx) stepGamemodeQuota(bot string) {
	inst := host.InstanceDir(c.msmDir, bot)
	for _, name := range []string{"gamemode_competitive.cfg", "gamemode_competitive_offline.cfg"} {
		p := filepath.Join(inst, "game", "csgo", "cfg", name)
		raw, err := os.ReadFile(p)
		if err != nil {
			c.add("gamemode 配额", "@"+bot+"/"+name, "skip", "文件不存在")
			continue
		}
		out, changed, note := zeroBotQuota(raw)
		if !changed {
			c.add("gamemode 配额", "@"+bot+"/"+name, "ok", note)
			continue
		}
		if err := c.backupAndWrite(p, out); err != nil {
			c.fail("gamemode 配额 @"+bot+"/"+name, err)
			continue
		}
		c.add("gamemode 配额", "@"+bot+"/"+name, "fixed", note)
	}
}

// ---- 5) gameinfo.gi 的名字库搜索路径(match3)----------------------------------

// insertBotprofilePath 在 `Game_LowViolence` 行后插入 overrides 搜索路径
// 。缩进与行尾沿用被插入行。
func insertBotprofilePath(raw []byte) (out []byte, changed bool, note string) {
	s := string(raw)
	if strings.Contains(s, "overrides/botprofile.vpk") {
		return raw, false, "搜索路径已在位"
	}
	re := regexp.MustCompile(`(?m)^([ \t]*)Game_LowViolence[^\r\n]*(\r?\n|$)`)
	m := re.FindStringSubmatchIndex(s)
	if m == nil {
		return raw, false, "未找到 Game_LowViolence 行(文件形态异常,需人工核对)"
	}
	eol := "\n"
	if strings.Contains(s[m[0]:m[1]], "\r\n") {
		eol = "\r\n"
	}
	indent := s[m[2]:m[3]]
	insert := indent + "Game\tcsgo/overrides/botprofile.vpk" + eol
	return []byte(s[:m[1]] + insert + s[m[1]:]), true, "已插入 overrides/botprofile.vpk 搜索路径"
}

func (c *patchCtx) stepGameInfo(bot string) {
	p := filepath.Join(host.InstanceDir(c.msmDir, bot), "game", "csgo", "gameinfo.gi")
	raw, err := os.ReadFile(p)
	if err != nil {
		c.add("gameinfo.gi", "@"+bot, "skip", "文件不存在")
		return
	}
	out, changed, note := insertBotprofilePath(raw)
	if !changed {
		action := "ok"
		if strings.Contains(note, "需人工") {
			action = "warn"
		}
		c.add("gameinfo.gi", "@"+bot, action, note)
		return
	}
	if err := c.backupAndWrite(p, out); err != nil {
		c.fail("gameinfo.gi @"+bot, err)
		return
	}
	c.add("gameinfo.gi", "@"+bot, "fixed", note)
}

// ---- 6) arena_bots.cfg 占位(match3)------------------------------------------

func (c *patchCtx) stepArenaBotsFile(bot string) {
	p := filepath.Join(host.InstanceDir(c.msmDir, bot), "game", "csgo", "cfg", "arena_bots.cfg")
	if !dirExists(filepath.Dir(p)) {
		c.add("arena_bots.cfg", "@"+bot, "skip", "cfg 目录不存在")
		return
	}
	if fileExists(p) {
		c.add("arena_bots.cfg", "@"+bot, "ok", "存在(每场由后端覆写)")
		return
	}
	if err := c.writeFile(p, []byte(neutralArenaBotsCfg)); err != nil {
		c.fail("arena_bots.cfg @"+bot, err)
		return
	}
	c.add("arena_bots.cfg", "@"+bot, "fixed", "缺失 → 写中性占位(cfg 链的 exec 不再报错)")
}

// ---- 7) 日志外移(各实例 game/csgo/logs 与 CSS logs)-------------------------

func (c *patchCtx) stepLogsSymlinks() {
	specs := []struct{ rel, arch string }{
		{filepath.Join("game", "csgo", "logs"), "logs"},
		{filepath.Join("game", "csgo", "addons", "counterstrikesharp", "logs"), "cssharp-logs"},
	}
	for _, inst := range c.o.Instances {
		where := "@" + inst
		instDir := host.InstanceDir(c.msmDir, inst)
		for _, sp := range specs {
			link := filepath.Join(instDir, sp.rel)
			target := filepath.Join(c.home, "arena-data", sp.arch, inst)
			item := "日志外移"
			fi, err := os.Lstat(link)
			switch {
			case os.IsNotExist(err):
				if !dirExists(filepath.Join(instDir, "game", "csgo")) {
					c.add(item, where+"/"+sp.rel, "skip", "实例目录未建起")
					continue
				}
				if err := c.mkdirAll(target); err != nil {
					c.fail(item+" "+where, err)
					continue
				}
				if err := c.symlink(target, link); err != nil {
					c.fail(item+" "+where, err)
					continue
				}
				c.add(item, where+"/"+sp.rel, "fixed", "建外移链接 → "+target)
			case err != nil:
				c.fail(item+" "+where, err)
			case fi.Mode()&os.ModeSymlink != 0:
				if got, _ := os.Readlink(link); got != target {
					c.add(item, where+"/"+sp.rel, "warn", "链接指向 "+got+"(期望 "+target+")")
				} else {
					c.add(item, where+"/"+sp.rel, "ok", "→ "+target)
				}
			case fi.IsDir():
				moved, conflict, err := c.moveDirInto(link, target)
				if err != nil {
					c.fail(item+" "+where, err)
					continue
				}
				if conflict > 0 {
					c.add(item, where+"/"+sp.rel, "warn",
						fmt.Sprintf("目录里有与归档同名项(%d 项已迁,%d 项冲突):请人工合并后改链接", moved, conflict))
					continue
				}
				if err := c.removeDir(link); err != nil {
					c.fail(item+" "+where, err)
					continue
				}
				if err := c.symlink(target, link); err != nil {
					c.fail(item+" "+where, err)
					continue
				}
				c.add(item, where+"/"+sp.rel, "fixed", fmt.Sprintf("原目录 %d 项已迁入归档 → 改建链接", moved))
			default:
				c.add(item, where+"/"+sp.rel, "warn", "是普通文件(非目录/链接),未动")
			}
		}
	}
}

// ---- 8) 工坊图共享(组内单一实目录 + 其余符号链接)----------------------------

func (c *patchCtx) stepWorkshopShare() {
	rel := filepath.Join("game", "bin", "linuxsteamrt64", "steamapps")
	type site struct{ inst, path string }
	var owners []string
	var missing []site
	linked := 0
	for _, inst := range c.o.Instances {
		p := filepath.Join(host.InstanceDir(c.msmDir, inst), rel)
		fi, err := os.Lstat(p)
		switch {
		case os.IsNotExist(err):
			missing = append(missing, site{inst, p})
		case err != nil:
			c.fail("工坊图共享 @"+inst, err)
		case fi.Mode()&os.ModeSymlink != 0:
			linked++
		default:
			owners = append(owners, inst)
		}
	}
	item := "工坊图共享"
	switch {
	case len(owners) == 0:
		c.add(item, "-", "skip", "没有任何实例有实目录(实例未启动过?)")
	case len(owners) > 1:
		c.add(item, "-", "warn", "多个实目录: "+strings.Join(owners, ", ")+" —— 需人工合并(只留一个共享源)")
	default:
		owner := owners[0]
		ownerPath := filepath.Join(host.InstanceDir(c.msmDir, owner), rel)
		for _, m := range missing {
			if err := c.mkdirAll(filepath.Dir(m.path)); err != nil {
				c.fail("工坊图共享 @"+m.inst, err)
				continue
			}
			if err := c.symlink(ownerPath, m.path); err != nil {
				c.fail("工坊图共享 @"+m.inst, err)
				continue
			}
			c.add(item, "@"+m.inst, "fixed", "建共享链接 → @"+owner)
		}
		c.add(item, "-", "ok", fmt.Sprintf("共享源:@%s;其余 %d 个为符号链接", owner, linked))
	}
}

// ---- 站点与文件工具 ----------------------------------------------------------

type patchSite struct {
	where string
	dir   string
}

// sites 返回 base + 各实例(与 `cs install --check` 的 L4 口径一致)。
func (c *patchCtx) sites() []patchSite {
	out := []patchSite{{"base", host.BaseDir(c.msmDir)}}
	for _, inst := range c.o.Instances {
		out = append(out, patchSite{"@" + inst, host.InstanceDir(c.msmDir, inst)})
	}
	return out
}

func (c *patchCtx) add(item, where, action, note string) {
	if c.o.DryRun && action == "fixed" {
		note = strings.TrimSpace(note + "(dry-run,未执行)")
	}
	c.items = append(c.items, ArenaPatchItem{Item: item, Where: where, Action: action, Note: note})
	c.o.Log("[%s] %s %s%s", action, item, where, map[bool]string{true: " —— " + note, false: ""}[note != ""])
}

func (c *patchCtx) fail(what string, err error) {
	c.errs = append(c.errs, what+": "+err.Error())
	c.items = append(c.items, ArenaPatchItem{Item: what, Where: "", Action: "warn", Note: err.Error()})
	c.o.Log("[warn] %s: %v", what, err)
}

// backupAndWrite 先备份原文件(同名 .bak-<stamp>)再写;dry-run 下只打印。
func (c *patchCtx) backupAndWrite(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if c.o.DryRun {
		c.o.Log("[dry-run] 将写 %s(备份 .bak-%s)", path, c.stamp)
		return nil
	}
	if old, err := os.ReadFile(path); err == nil {
		bak := path + ".bak-" + c.stamp
		if _, err := os.Stat(bak); err == nil {
			bak = fmt.Sprintf("%s-%d", bak, os.Getpid())
		}
		if err := os.WriteFile(bak, old, mode); err != nil {
			return fmt.Errorf("备份 %s 失败: %w", path, err)
		}
	}
	return writeAtomic(path, data, mode)
}

func (c *patchCtx) writeFile(path string, data []byte) error {
	if c.o.DryRun {
		c.o.Log("[dry-run] 将写 %s", path)
		return nil
	}
	return writeAtomic(path, data, 0o644)
}

func (c *patchCtx) mkdirAll(dir string) error {
	if c.o.DryRun {
		c.o.Log("[dry-run] 将建目录 %s", dir)
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

func (c *patchCtx) symlink(target, link string) error {
	if c.o.DryRun {
		c.o.Log("[dry-run] 将建链接 %s → %s", link, target)
		return nil
	}
	if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
		return fmt.Errorf("建链接 %s → %s 失败: %w", link, target, err)
	}
	return nil
}

func (c *patchCtx) removeDir(dir string) error {
	if c.o.DryRun {
		c.o.Log("[dry-run] 将删空目录 %s", dir)
		return nil
	}
	return os.Remove(dir)
}

// moveDirInto 把 src 目录里的条目搬进 dst(同文件系统 rename);返回搬移数与冲突数。
// 冲突(目标已有同名项)不覆盖、留在原地,交由人工处置。
func (c *patchCtx) moveDirInto(src, dst string) (moved, conflict int, err error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0, 0, err
	}
	if err := c.mkdirAll(dst); err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		target := filepath.Join(dst, e.Name())
		if _, err := os.Lstat(target); err == nil {
			conflict++
			continue
		}
		if c.o.DryRun {
			c.o.Log("[dry-run] 将迁 %s → %s", filepath.Join(src, e.Name()), target)
			moved++
			continue
		}
		if err := os.Rename(filepath.Join(src, e.Name()), target); err != nil {
			return moved, conflict, fmt.Errorf("迁 %s 失败: %w", e.Name(), err)
		}
		moved++
	}
	return moved, conflict, nil
}

// writeAtomic 写临时文件再改名(避免半截文件被引擎读到)。
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ---- 小工具 -----------------------------------------------------------------

// pickBotInstance 判定"增强人机实例":先看谁的 MatchZy cfg 已挂 arena_bots.cfg
// (存量环境),否则退回默认 match3(被 CS2 更新还原后标记不再存在的情形)。
func pickBotInstance(msmDir string, instances []string) string {
	for _, inst := range instances {
		dir := filepath.Join(host.InstanceDir(msmDir, inst), "game", "csgo", "cfg", "MatchZy")
		for _, f := range []string{"warmup.cfg", "live_override.cfg", "live_wingman_override.cfg"} {
			if raw, err := os.ReadFile(filepath.Join(dir, f)); err == nil && bytes.Contains(raw, []byte("arena_bots.cfg")) {
				return inst
			}
		}
	}
	for _, inst := range instances {
		if inst == DefaultBotInstance {
			return inst
		}
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// splitLinesKeepEOL 按行切分并保留行尾(与 Python splitlines 不同:这里不吞最后一个分隔符)。
func splitLinesKeepEOL(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.SplitAfter(s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// trimEOL 拆出正文与行尾。
func trimEOL(line string) (body, eol string) {
	if strings.HasSuffix(line, "\r\n") {
		return strings.TrimSuffix(line, "\r\n"), "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return strings.TrimSuffix(line, "\n"), "\n"
	}
	return line, ""
}
