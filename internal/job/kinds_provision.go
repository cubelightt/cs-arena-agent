// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package job

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"arena/agent/internal/fsx"
	"arena/agent/internal/host"
	"arena/agent/internal/registry"
)

// 本文件实现建/删实例任务。面板与 `cs new`/`cs del` 共用状态机、进度、日志与取消逻辑。
//
// 纪律(与其它 kind 一致):破坏性动作先二次确认(cs del 的 confirm 必须精确等于实例名);
// 失败回滚只删"本次任务自己创建的"半成品;任何删除都必须落在 msm 数据根之内(removeTreeGuarded)。

// 实例名规则(与后端 PL/py 校验一致:1~32 位字母/数字/下划线/点/连字符)。
var instanceNameRe = regexp.MustCompile(`^[\w.-]{1,32}$`)

// ---- instance_create(建实例)------------------------------------------------

func instanceCreatePlan(spec Spec, instances []string, m *Manager) (Plan, error) {
	name := strings.TrimSpace(strParam(spec.Params, "name"))
	if !instanceNameRe.MatchString(name) {
		return Plan{}, errors.New("实例名非法(允许 1~32 位字母/数字/下划线/点/连字符)")
	}
	for _, n := range instances {
		if n == name {
			return Plan{}, fmt.Errorf("实例 %s 已存在(白名单/注册表内)", name)
		}
	}
	cloneFrom := strings.TrimSpace(strParam(spec.Params, "cloneFrom"))
	if cloneFrom == "" {
		cloneFrom = defaultCloneSource(instances)
	}
	if cloneFrom == "" {
		return Plan{}, errors.New("缺少 cloneFrom:本机没有可用作模板的实例(先建一个或用 cloneFrom 指定)")
	}
	if cloneFrom == name {
		return Plan{}, errors.New("cloneFrom 不能与实例名相同")
	}
	if !instanceNameRe.MatchString(cloneFrom) {
		return Plan{}, errors.New("cloneFrom 名字非法")
	}
	wantPort := intParam(spec.Params, "port", 0)
	syncPlugins := boolParam(spec.Params, "syncPlugins", true)
	linkMaps := boolParam(spec.Params, "linkMaps", true)

	// 本次任务创建出来的目录(回滚用;只删这里,绝不动别人)
	created := false

	steps := []Step{
		{Name: "前置检查(磁盘/msm/来源/端口)", Run: func(c *Ctx) error {
			total, free, _, err := host.DiskUsage(c.Cfg.MSMDir)
			if err != nil {
				return fmt.Errorf("读取磁盘余量失败: %w", err)
			}
			if c.Cfg.MinFreeBytes > 0 && free < c.Cfg.MinFreeBytes {
				return fmt.Errorf("磁盘余量不足: 剩 %s < 阈值 %s(见 config.yaml 的 min_free_bytes)",
					host.HumanBytes(free), host.HumanBytes(c.Cfg.MinFreeBytes))
			}
			c.Logf("磁盘:总 %s / 剩 %s", host.HumanBytes(total), host.HumanBytes(free))
			if fi, err := os.Stat(c.Cfg.MSM); err != nil || fi.IsDir() {
				return fmt.Errorf("msm 可执行不可用: %s", c.Cfg.MSM)
			}
			srcGame, tried := c.Paths.FindGameDir(cloneFrom)
			if srcGame == "" {
				return fmt.Errorf("来源实例 %s 的 csgo 目录未命中(tried %v)", cloneFrom, tried)
			}
			c.Logf("来源实例 %s 的 csgo 目录: %s", cloneFrom, srcGame)
			// 先把"既有实例的完整编号表"落盘(此刻新实例还没出现):否则 clone 之后新实例会按
			// 名称序拿到第一个号、把既有实例的 #号挤到后面(重扫不重排的语义被破坏)
			if scanned, err := registry.Scan(c.Cfg.MSMDir); err == nil {
				rf := registry.Sync(registry.Load(m.registryPath()), scanned)
				if err := registry.Save(m.registryPath(), rf); err != nil {
					c.Logf("注册表预落盘失败(忽略,末步会重试): %v", err)
				}
			}
			cfgDir := filepath.Join(registry.CFGDir(c.Cfg.MSMDir), "inst-"+name)
			if fi, err := os.Stat(cfgDir); err == nil && fi.IsDir() {
				return fmt.Errorf("目标实例已存在: %s(先 cs del 或换名字)", cfgDir)
			}
			if instDir := host.InstanceDir(c.Cfg.MSMDir, name); dirExists(instDir) {
				return fmt.Errorf("目标实例目录已存在: %s(先 cs del 或换名字)", instDir)
			}
			if wantPort > 0 {
				rf := registry.Load(m.registryPath())
				conflicts := registry.PortConflict(append(append([]registry.Instance{}, rf.Items...), registry.Instance{
					Name: name, Port: wantPort, GotvPort: wantPort + 100,
				}))
				if len(conflicts) > 0 {
					return fmt.Errorf("端口冲突: %s", strings.Join(conflicts, "; "))
				}
				c.Logf("指定端口 %d(GOTV %d)", wantPort, wantPort+100)
			}
			return nil
		}},
		{Name: "msm clone", Run: func(c *Ctx) error {
			c.SetProgress(20)
			res, err := c.MSM(cloneFrom, "clone", []string{name}, 600)
			if err != nil {
				return fmt.Errorf("msm clone 失败: %w", err)
			}
			if res != nil && res.Returncode != 0 {
				return fmt.Errorf("msm clone 返回非零(%d),详见任务日志", res.Returncode)
			}
			cfgDir := filepath.Join(registry.CFGDir(c.Cfg.MSMDir), "inst-"+name)
			if !dirExists(cfgDir) {
				return fmt.Errorf("clone 后仍未生成实例配置目录: %s(检查 msm 的 clone 子命令是否可用)", cfgDir)
			}
			created = true
			c.Logf("clone 完成:%s → %s", cloneFrom, name)
			return nil
		}},
		{Name: "清理 SwiftlyS2 残留", Run: func(c *Ctx) error {
			c.SetProgress(45)
			return cleanSwiftlyS2(c, name)
		}},
		{Name: "拷贝插件树(纯拷贝,无 rsync)", Run: func(c *Ctx) error {
			c.SetProgress(60)
			if !syncPlugins {
				c.Logf("按参数跳过插件树拷贝(syncPlugins=false)")
				return nil
			}
			src, err := addonsDirOf(c, cloneFrom)
			if err != nil {
				c.Logf("来源插件目录不可用(%v),跳过拷贝", err)
				return nil
			}
			if !dirExists(src) {
				c.Logf("来源插件目录不存在(%s),跳过拷贝", src)
				return nil
			}
			dst, err := addonsDirOf(c, name)
			if err != nil {
				return err
			}
			files, bytes, _, err := syncTree(src, dst, false, false, cloneFrom, name, c.Logf)
			if err != nil {
				return fmt.Errorf("插件树拷贝失败: %w", err)
			}
			c.Logf("插件树:%d 个文件 / %s", files, host.HumanBytes(bytes))
			c.SetResult("pluginFiles", files)
			return nil
		}},
		{Name: "共享 steamapps(工坊图零拷贝)", Run: func(c *Ctx) error {
			c.SetProgress(75)
			if !linkMaps {
				c.Logf("按参数跳过 steamapps 符号链接(linkMaps=false)")
				return nil
			}
			srcGame, _ := c.Paths.FindGameDir(cloneFrom)
			dstGame, _ := c.Paths.FindGameDir(name)
			if srcGame == "" || dstGame == "" {
				c.Logf("来源或目标 csgo 目录缺失,跳过共享链接")
				return nil
			}
			src := sharedSteamappsPath(srcGame)
			dst := sharedSteamappsPath(dstGame)
			if fi, err := os.Lstat(dst); err == nil {
				if fi.Mode()&os.ModeSymlink != 0 || dirExists(dst) {
					c.Logf("目标 steamapps 已存在,跳过链接: %s", dst)
					return nil
				}
			}
			if !dirExists(src) {
				c.Logf("来源共享目录不存在(%s),跳过链接(实例将自带一份)", src)
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(src, dst); err != nil {
				return fmt.Errorf("创建 steamapps 符号链接失败: %w", err)
			}
			c.Logf("steamapps → %s", src)
			return nil
		}},
		{Name: "回读端口 + 写注册表", Run: func(c *Ctx) error {
			c.SetProgress(90)
			items, err := registry.Scan(c.Cfg.MSMDir)
			if err != nil {
				return fmt.Errorf("回读实例清单失败: %w", err)
			}
			me, err := registry.Resolve(items, name)
			if err != nil {
				return fmt.Errorf("回读端口失败(实例目录未就位): %w", err)
			}
			rf := registry.Load(m.registryPath())
			rf, appended, err := registry.AppendAt(rf, me.Name, me.Port, me.GotvPort, intParam(spec.Params, "idx", 0))
			if err != nil {
				return err
			}
			if err := registry.Save(m.registryPath(), rf); err != nil {
				return fmt.Errorf("注册表写入失败: %w", err)
			}
			c.Logf("注册表:#%d %s 端口 %d(GOTV %d)", appended.Idx, appended.Name, appended.Port, appended.GotvPort)
			c.SetResult("idx", appended.Idx)
			c.SetResult("name", appended.Name)
			c.SetResult("port", appended.Port)
			c.SetResult("gotvPort", appended.GotvPort)
			return nil
		}},
		{Name: "完成提示", Run: func(c *Ctx) error {
			c.SetProgress(100)
			c.Logf("提示:实例已创建但尚未启动;启动用 `cs start %d`(平台侧需管理员确认后才会参与自动分配)", c.resultIdx())
			return nil
		}},
	}
	return Plan{Steps: steps, Recover: func(c *Ctx) {
		// 失败/取消:删掉本次创建的半成品(实例目录 + cfg),不留脏数据
		if !created {
			return
		}
		c.Logf("回滚:删除本次创建的半成品 %s", name)
		for _, p := range []string{host.InstanceDir(c.Cfg.MSMDir, name), filepath.Join(registry.CFGDir(c.Cfg.MSMDir), "inst-"+name)} {
			if !pathExists(p) {
				continue
			}
			if freed, err := removeTreeGuarded(c.Cfg.MSMDir, p); err != nil {
				c.Logf("回滚失败(需人工清理):%s: %v", p, err)
			} else {
				c.Logf("已删除 %s(%s)", p, host.HumanBytes(freed))
			}
		}
	}}, nil
}

// ---- instance_delete(删实例)------------------------------------------------

func instanceDeletePlan(spec Spec, instances []string, m *Manager) (Plan, error) {
	name := strings.TrimSpace(strParam(spec.Params, "name"))
	if !instanceNameRe.MatchString(name) {
		return Plan{}, errors.New("实例名非法(允许 1~32 位字母/数字/下划线/点/连字符)")
	}
	confirm := spec.Confirm
	if v, ok := spec.Params["confirm"].(string); ok && v != "" {
		confirm = v
	}
	if confirm != name {
		return Plan{}, fmt.Errorf("删除实例需要二次确认(confirm 必须精确等于实例名 %q)", name)
	}
	// 必须在白名单/注册表内:防拼错名字误删(例如把 inst-main 写成 inst-man)
	if !containsName(instances, name) {
		rf := registry.Load(m.registryPath())
		if !containsName(registry.Names(rf.Items), name) {
			return Plan{}, fmt.Errorf("实例 %s 不在本机注册表/白名单内,拒绝删除", name)
		}
	}
	if len(instances) <= 1 && containsName(instances, name) {
		return Plan{}, errors.New("这是本机唯一实例,拒绝删除(至少保留一个可用实例)")
	}

	paths := []string{}
	steps := []Step{
		{Name: "前置检查 + 停实例", Run: func(c *Ctx) error {
			instDir := host.InstanceDir(c.Cfg.MSMDir, name)
			cfgDir := filepath.Join(registry.CFGDir(c.Cfg.MSMDir), "inst-"+name)
			if !dirExists(instDir) && !dirExists(cfgDir) {
				return fmt.Errorf("实例目录与配置目录都不存在: %s / %s", instDir, cfgDir)
			}
			paths = append(paths, instDir, cfgDir)
			if host.TmuxRunning(c.Cfg.MSMDir, name) {
				c.Logf("实例在运行,先停服(msm stop)")
				if _, err := c.MSM(name, "stop", nil, 120); err != nil {
					c.Logf("停服调用失败(%v),继续删除", err)
				}
			} else {
				c.Logf("实例当前未运行")
			}
			for _, p := range paths {
				size, files := host.DirSize(p)
				c.Logf("将删除 %s(%s / %d 个文件)", p, host.HumanBytes(size), files)
			}
			return nil
		}},
		{Name: "删除实例目录与配置目录", Run: func(c *Ctx) error {
			c.SetProgress(40)
			var freed int64
			removed := make([]string, 0, len(paths))
			for _, p := range paths {
				if !pathExists(p) {
					continue
				}
				n, err := removeTreeGuarded(c.Cfg.MSMDir, p)
				if err != nil {
					return err
				}
				freed += n
				removed = append(removed, p)
				c.Logf("已删除 %s(%s)", p, host.HumanBytes(n))
			}
			c.SetResult("freedBytes", freed)
			c.SetResult("removed", removed)
			c.Logf("释放 %s", host.HumanBytes(freed))
			return nil
		}},
		{Name: "注册表移除条目", Run: func(c *Ctx) error {
			c.SetProgress(85)
			rf := registry.Load(m.registryPath())
			if items, err := registry.Scan(c.Cfg.MSMDir); err == nil {
				rf = registry.Sync(rf, items) // 文件保持完整(条目不散失;高水位不动)
			}
			before := len(rf.Items)
			rf = registry.Remove(rf, name)
			if err := registry.Save(m.registryPath(), rf); err != nil {
				return fmt.Errorf("注册表写入失败: %w", err)
			}
			c.Logf("注册表:%d → %d 条(编号作废不回收)", before, len(rf.Items))
			c.SetResult("name", name)
			return nil
		}},
		{Name: "完成提示", Run: func(c *Ctx) error {
			c.SetProgress(100)
			c.Logf("提示:平台侧按墓碑(action:'removed')收敛删除该行;如需重新建同名实例,直接 cs new --name %s(编号作废不回收)", name)
			return nil
		}},
	}
	return Plan{Steps: steps, Recover: func(c *Ctx) {
		// 删除到一半无法恢复:如实记录(不谎报"已回滚")
		c.Logf("注意:删除流程中断,实例目录可能已被部分删除;核对 %v", paths)
	}}, nil
}

// ---- 共用工具 --------------------------------------------------------------

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// defaultCloneSource 取默认模板实例:优先 main,否则第一个可用实例。
func defaultCloneSource(instances []string) string {
	for _, n := range instances {
		if n == "main" {
			return n
		}
	}
	if len(instances) > 0 {
		return instances[0]
	}
	return ""
}

// sharedSteamappsPath 由实例的 csgo 目录推出共享的 steamapps 目录(工坊图零拷贝的落点):
// `<inst>/game/csgo` → `<inst>/game/bin/linuxsteamrt64/steamapps`(主机现状:main 持有真目录,
// match1/2/3 的该路径是指向 main 的符号链接 —— 2026-09-21 主机环境验证)。
func sharedSteamappsPath(gameDir string) string {
	return filepath.Join(filepath.Dir(gameDir), "bin", "linuxsteamrt64", "steamapps")
}

// resultIdx 读回任务结果里的 idx(完成提示用;缺失回 0)。
func (c *Ctx) resultIdx() int {
	if c.Job == nil || c.Job.Result == nil {
		return 0
	}
	switch v := c.Job.Result["idx"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// cleanSwiftlyS2 清掉 clone 带过来的 SwiftlyS2 残留(我们不用它;留着会让启动流程多一层):
//   - 删 <实例>/game/csgo/addons/swiftlys2;
//   - 去掉 gameinfo.gi 里的 swiftly 行;
//   - server.conf 置 SM_SWIFTLYS2=0(msm 的弱默认,显式关掉)。
//
// 全部 best-effort:路径不存在就跳过(不阻断建实例)。
func cleanSwiftlyS2(c *Ctx, name string) error {
	gameDir, tried := c.Paths.FindGameDir(name)
	if gameDir == "" {
		return fmt.Errorf("实例 %s 的 csgo 目录未命中(tried %v)", name, tried)
	}
	addons := filepath.Join(gameDir, "addons", "swiftlys2")
	if dirExists(addons) {
		freed, err := removeTreeGuarded(c.Cfg.MSMDir, addons)
		if err != nil {
			return err
		}
		c.Logf("已删除 SwiftlyS2 插件目录(%s)", host.HumanBytes(freed))
	} else {
		c.Logf("无 SwiftlyS2 插件目录,跳过")
	}
	// gameinfo.gi:剔除以 swiftly 为关键字的搜索路径行
	gi := filepath.Join(gameDir, "gameinfo.gi")
	if raw, err := os.ReadFile(gi); err == nil {
		kept := make([]string, 0)
		removed := 0
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(strings.ToLower(line), "swiftly") {
				removed++
				continue
			}
			kept = append(kept, line)
		}
		if removed > 0 {
			body := strings.Join(kept, "\n")
			if _, err := atomicWriteFile(gi, body); err != nil {
				c.Logf("gameinfo.gi 改写失败(忽略): %v", err)
			} else {
				c.Logf("gameinfo.gi 移除 %d 行 swiftly 搜索路径", removed)
			}
		} else {
			c.Logf("gameinfo.gi 无 swiftly 行")
		}
	}
	conf := filepath.Join(registry.CFGDir(c.Cfg.MSMDir), "inst-"+name, "server.conf")
	raw, err := os.ReadFile(conf)
	if err == nil {
		body := string(raw)
		if strings.Contains(body, "SM_SWIFTLYS2") {
			body = replaceConfValue(body, "SM_SWIFTLYS2", "0")
		} else {
			body = strings.TrimRight(body, "\n") + "\nSM_SWIFTLYS2=0\n"
		}
		if _, err := atomicWriteFile(conf, body); err != nil {
			c.Logf("server.conf 改写失败(忽略): %v", err)
		} else {
			c.Logf("server.conf 置 SM_SWIFTLYS2=0")
		}
	}
	return nil
}

// removeTreeGuarded 删除目录/文件前先校验它落在 msm 数据根(<msm_dir>/../msm.d/cs2)之内,
// 并返回释放的字节数(符号链接按链接本身处理,不跟随 —— 共享 steamapps 不会被误删)。
func removeTreeGuarded(msmDir, path string) (int64, error) {
	root := filepath.Clean(filepath.Dir(registry.CFGDir(msmDir)))
	p := filepath.Clean(path)
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return 0, fmt.Errorf("拒绝删除 msm 数据根之外的路径: %s(根 %s)", p, root)
	}
	size, _ := host.DirSize(p)
	if err := os.RemoveAll(p); err != nil {
		return 0, fmt.Errorf("删除失败 %s: %w", p, err)
	}
	return size, nil
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// atomicWriteFile 原子写文本(与 config.yaml/state.json 同款 tmp+fsync+rename)。
func atomicWriteFile(path, body string) (int, error) {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return fsx.AtomicWriteText(path, body)
}

// replaceConfValue 按行替换 `KEY=...`(保留其余行与注释;找不到则原样返回)。
func replaceConfValue(body, key, value string) string {
	lines := strings.Split(body, "\n")
	prefix := key + "="
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			lines[i] = prefix + value
		}
	}
	return strings.Join(lines, "\n")
}
