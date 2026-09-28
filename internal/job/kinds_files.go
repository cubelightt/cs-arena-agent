// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package job

import (
	"archive/zip"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"arena/agent/internal/host"
)

// 本文件是"文件类"任务:plugin_sync / plugin_deploy / demo_collect / host_cleanup。
// 共同纪律:① 默认只读(dry-run)除非显式要求;② 破坏性操作先备份到 ~/backup;
// ③ 删除类操作要求二次确认串(host_cleanup 的 confirm="CLEAN")。

// ---- plugin_sync(纯拷贝同步,替代已失效的 sync-plugin-main.sh;主机无 rsync)------

func pluginSyncPlan(spec Spec, m *Manager) (Plan, error) {
	from := strParam(spec.Params, "from")
	if from == "" {
		from = "main"
	}
	targets := strSliceParam(spec.Params, "targets")
	if len(targets) == 0 {
		return Plan{}, fmt.Errorf("缺少 targets(要同步到的目标,例如 [\"base\",\"match1\"])")
	}
	doDelete := boolParam(spec.Params, "delete", false)
	dryRun := boolParam(spec.Params, "dryRun", false)
	// 走 msm 全局锁:同步期间不与其他 msm 命令并发(避免拷到一半实例在跑)
	_ = m

	steps := []Step{
		{
			Name: fmt.Sprintf("同步插件树 %s → %s", from, strings.Join(targets, ",")),
			Run: func(c *Ctx) error {
				srcDir, err := addonsDirOf(c, from)
				if err != nil {
					return err
				}
				if fi, err := os.Stat(srcDir); err != nil || !fi.IsDir() {
					return fmt.Errorf("来源插件目录不存在: %s", srcDir)
				}
				c.Logf("来源 %s", srcDir)
				totalFiles, totalBytes, deleted := 0, int64(0), 0
				perTarget := make([]map[string]any, 0, len(targets))
				for _, t := range targets {
					if t == from {
						c.Logf("跳过 %s(与来源相同)", t)
						continue
					}
					dstDir, err := addonsDirOf(c, t)
					if err != nil {
						return err
					}
					files, bytes, del, err := syncTree(srcDir, dstDir, doDelete, dryRun, from, t, c.Logf)
					if err != nil {
						return fmt.Errorf("同步到 %s 失败: %w", t, err)
					}
					c.Logf("%s: 拷贝 %d 个文件 / %s%s", t, files, host.HumanBytes(bytes), map[bool]string{true: "(dry-run)", false: ""}[dryRun])
					totalFiles += files
					totalBytes += bytes
					deleted += del
					perTarget = append(perTarget, map[string]any{"name": t, "files": files, "bytes": bytes, "deleted": del})
				}
				c.SetResult("targets", perTarget)
				c.SetResult("files", totalFiles)
				c.SetResult("bytes", totalBytes)
				c.SetResult("dryRun", dryRun)
				if !dryRun && totalFiles > 0 {
					c.Logf("提示:插件文件已变更,相关实例需 restart 才生效(cs restart <实例>)")
				}
				return nil
			},
		},
	}
	return Plan{Steps: steps}, nil
}

// addonsDirOf 解析目标名 → addons 目录(base = 共享安装;<name> = 实例)。
func addonsDirOf(c *Ctx, name string) (string, error) {
	if name == "base" {
		return filepath.Join(host.BaseDir(c.Cfg.MSMDir), "game", "csgo", "addons"), nil
	}
	gameDir, tried := c.Paths.FindGameDir(name)
	if gameDir == "" {
		return "", fmt.Errorf("实例 %s 的 csgo 目录未命中(tried %v)", name, tried)
	}
	return filepath.Join(gameDir, "addons"), nil
}

// syncTree 把 src 拷到 dst(同名同大小跳过);delete 时删 dst 中 src 没有的文件;
// dryRun 只统计不落盘。返回 (拷贝文件数, 字节数, 删除数)。
//
// 符号链接**按链接本身复制**(不跟随):主机把 CSSharp 日志外移成
// `addons/counterstrikesharp/logs -> ~/arena-data/cssharp-logs/<实例>`,跟随它会把目录当文件拷
// (copy_file_range: is a directory)。目标里"作为独立路径段"的 instFrom 会改写成 instTo 并建好
// 目标目录 —— 否则新实例与来源实例共用一份日志(instFrom/instTo 为空或相同则原样保留)。
func syncTree(src, dst string, del, dryRun bool, instFrom, instTo string, logf func(string, ...any)) (int, int64, int, error) {
	srcFiles := map[string]os.FileInfo{}
	srcLinks := map[string]string{}
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t, err := os.Readlink(path)
			if err != nil {
				if logf != nil {
					logf("读取符号链接失败(跳过) %s: %v", path, err)
				}
				return nil
			}
			srcLinks[rel] = t
			return nil
		}
		srcFiles[rel] = info
		return nil
	})
	if err != nil {
		return 0, 0, 0, err
	}

	files, bytes := 0, int64(0)
	rels := make([]string, 0, len(srcFiles))
	for rel := range srcFiles {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		info := srcFiles[rel]
		target := filepath.Join(dst, rel)
		if fi, err := os.Stat(target); err == nil && fi.Size() == info.Size() && !fi.IsDir() {
			continue // 同大小视为已同步(纯拷贝同步,不做哈希)
		}
		files++
		bytes += info.Size()
		if dryRun {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return files, bytes, 0, err
		}
		if err := copyFilePerm(filepath.Join(src, rel), target, info.Mode()); err != nil {
			return files, bytes, 0, err
		}
	}

	linkRels := make([]string, 0, len(srcLinks))
	for rel := range srcLinks {
		linkRels = append(linkRels, rel)
	}
	sort.Strings(linkRels)
	for _, rel := range linkRels {
		want := retargetLink(srcLinks[rel], instFrom, instTo)
		target := filepath.Join(dst, rel)
		if cur, err := os.Readlink(target); err == nil && cur == want {
			continue // 已是指向期望目标的链接
		}
		files++
		if dryRun {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return files, bytes, 0, err
		}
		if want != srcLinks[rel] {
			if filepath.IsAbs(want) {
				// 重指向的目标目录(外移的日志目录等)要先建好,否则新实例的链接是断的
				if err := os.MkdirAll(want, 0o755); err != nil {
					return files, bytes, 0, err
				}
			}
			if logf != nil {
				logf("符号链接 %s 重指向新实例:%s → %s", rel, srcLinks[rel], want)
			}
		}
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			if err := os.RemoveAll(target); err != nil { // 目标可能是目录(旧行为跟随链接留下的)
				return files, bytes, 0, err
			}
		}
		if err := os.Symlink(want, target); err != nil {
			return files, bytes, 0, err
		}
	}

	deleted := 0
	if del {
		_ = filepath.Walk(dst, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return err
			}
			rel, err := filepath.Rel(dst, path)
			if err != nil {
				return nil
			}
			if _, ok := srcFiles[rel]; ok {
				return nil
			}
			if _, ok := srcLinks[rel]; ok {
				return nil // 源里的链接(含重指向后的)保留
			}
			deleted++
			if dryRun {
				return nil
			}
			if err := os.Remove(path); err != nil && logf != nil {
				logf("删除失败 %s: %v", path, err)
			}
			return nil
		})
	}
	return files, bytes, deleted, nil
}

// retargetLink 把链接目标里"作为独立路径段"的 instFrom 换成 instTo
// (只替换完整路径段,避免 main → domain 这类子串误伤);无需改写时原样返回。
func retargetLink(target, instFrom, instTo string) string {
	if instFrom == "" || instTo == "" || instFrom == instTo {
		return target
	}
	parts := strings.Split(target, "/")
	changed := false
	for i, p := range parts {
		if p == instFrom {
			parts[i] = instTo
			changed = true
		}
	}
	if !changed {
		return target
	}
	return strings.Join(parts, "/")
}

func copyFilePerm(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyTree 递归拷贝目录(备份用),返回文件数。
func copyTree(src, dst string) (int, error) {
	n := 0
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := copyFilePerm(path, target, info.Mode()); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}

// ---- plugin_deploy(从 zip 部署到 base + 指定实例;旧版自动备份)------------------

func pluginDeployPlan(spec Spec, m *Manager) (Plan, error) {
	name := strParam(spec.Params, "name")
	if name == "" {
		return Plan{}, fmt.Errorf("缺少 name(插件名,用于备份目录命名)")
	}
	zipPath := strParam(spec.Params, "path")
	zipB64 := strParam(spec.Params, "zipB64")
	if zipPath == "" && zipB64 == "" {
		return Plan{}, fmt.Errorf("需要 path(主机上的 zip 路径)或 zipB64")
	}
	targets := strSliceParam(spec.Params, "targets")
	if len(targets) == 0 {
		targets = []string{"base"}
	}
	doBackup := boolParam(spec.Params, "backup", true)
	dryRun := boolParam(spec.Params, "dryRun", false)

	steps := []Step{
		{
			Name: "解包并部署插件",
			Run: func(c *Ctx) error {
				zpath := zipPath
				if zpath == "" {
					raw, err := base64.StdEncoding.DecodeString(zipB64)
					if err != nil {
						return fmt.Errorf("zipB64 解码失败: %w", err)
					}
					zpath = filepath.Join(m.opt.Dir, fmt.Sprintf("upload-%d-%s.zip", time.Now().Unix(), name))
					if err := os.WriteFile(zpath, raw, 0o644); err != nil {
						return fmt.Errorf("写入上传包失败: %w", err)
					}
					defer os.Remove(zpath)
				}
				zr, err := zip.OpenReader(zpath)
				if err != nil {
					return fmt.Errorf("打开 zip 失败: %w", err)
				}
				defer zr.Close()

				entries := make([]*zip.File, 0, len(zr.File))
				for _, f := range zr.File {
					if f.FileInfo().IsDir() {
						continue
					}
					rel := filepath.Clean(f.Name)
					if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
						return fmt.Errorf("zip 内含非法路径: %s", f.Name)
					}
					if !strings.HasPrefix(rel, "addons/") && !strings.HasPrefix(rel, "cfg/") {
						return fmt.Errorf("zip 条目必须在 addons/ 或 cfg/ 下: %s", f.Name)
					}
					entries = append(entries, f)
				}
				if len(entries) == 0 {
					return fmt.Errorf("zip 内没有可部署的文件(addons/ 或 cfg/ 下为空)")
				}
				c.Logf("待部署 %d 个文件 → %s%s", len(entries), strings.Join(targets, ","),
					map[bool]string{true: "(dry-run)", false: ""}[dryRun])

				stamp := time.Now().Format("20060102-150405")
				backupDir := ""
				for _, t := range targets {
					gameDir, err := gameDirOf(c, t)
					if err != nil {
						return err
					}
					if doBackup && !dryRun {
						backupDir = filepath.Join(backupRoot(c.Cfg), name+"-"+stamp)
						for _, f := range entries {
							rel := filepath.Clean(f.Name)
							cur := filepath.Join(gameDir, rel)
							if _, err := os.Stat(cur); err != nil {
								continue
							}
							dst := filepath.Join(backupDir, t, rel)
							if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
								return err
							}
							if err := copyFilePerm(cur, dst, 0o644); err != nil {
								return err
							}
						}
					}
					for _, f := range entries {
						rel := filepath.Clean(f.Name)
						dst := filepath.Join(gameDir, rel)
						if dryRun {
							continue
						}
						if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
							return err
						}
						if err := extractZipFile(f, dst); err != nil {
							return fmt.Errorf("写出 %s 失败: %w", dst, err)
						}
					}
					c.Logf("%s: 部署到 %s", t, gameDir)
				}
				if backupDir != "" {
					c.SetResult("backupDir", backupDir)
					c.Logf("旧版已备份:%s", backupDir)
				}
				c.SetResult("name", name)
				c.SetResult("files", len(entries))
				c.SetResult("targets", targets)
				if !dryRun {
					c.Logf("提示:插件已部署,相关实例需 restart 才生效")
				}
				return nil
			},
		},
	}
	return Plan{Steps: steps}, nil
}

// gameDirOf 解析目标名 → csgo 目录(base = 共享安装)。
func gameDirOf(c *Ctx, name string) (string, error) {
	if name == "base" {
		return filepath.Join(host.BaseDir(c.Cfg.MSMDir), "game", "csgo"), nil
	}
	gameDir, tried := c.Paths.FindGameDir(name)
	if gameDir == "" {
		return "", fmt.Errorf("实例 %s 的 csgo 目录未命中(tried %v)", name, tried)
	}
	return gameDir, nil
}

func extractZipFile(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- demo_collect(实例 MatchZy/*.dem → <demo_dir>/<实例>/<日期>/)---------------

func demoCollectPlan(spec Spec, m *Manager) (Plan, error) {
	_ = m
	matchID := strParam(spec.Params, "matchId")
	all := boolParam(spec.Params, "all", false)
	dryRun := boolParam(spec.Params, "dryRun", false)
	if matchID == "" && !all {
		return Plan{}, fmt.Errorf("需要 matchId(单场次)或 all=true(全量)")
	}

	steps := []Step{
		{
			Name: "归集对局录像",
			Run: func(c *Ctx) error {
				demoDir := c.Cfg.DemoDir
				if demoDir == "" {
					return fmt.Errorf("未配置 demo_dir")
				}
				moved := make([]map[string]any, 0)
				totalBytes := int64(0)
				skipped := 0
				for _, name := range c.Instances {
					gameDir, _ := c.Paths.FindGameDir(name)
					if gameDir == "" {
						c.Logf("%s: csgo 目录未命中,跳过", name)
						continue
					}
					pattern := filepath.Join(gameDir, "MatchZy", "*.dem")
					files, _ := filepath.Glob(pattern)
					sort.Strings(files)
					for _, f := range files {
						if matchID != "" && !strings.Contains(filepath.Base(f), matchID) {
							continue
						}
						fi, err := os.Stat(f)
						if err != nil {
							continue
						}
						day := fi.ModTime().Format("2006-01-02")
						dest := filepath.Join(demoDir, name, day, filepath.Base(f))
						if _, err := os.Stat(dest); err == nil {
							skipped++
							continue
						}
						if dryRun {
							c.Logf("%s → %s(dry-run)", f, dest)
							continue
						}
						if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
							return err
						}
						if err := os.Rename(f, dest); err != nil {
							// 跨挂载点:退化为拷贝 + 删除
							if err := copyFilePerm(f, dest, fi.Mode()); err != nil {
								return fmt.Errorf("移动 %s 失败: %w", f, err)
							}
							if err := os.Remove(f); err != nil {
								return err
							}
						}
						moved = append(moved, map[string]any{"from": f, "to": dest, "bytes": fi.Size()})
						totalBytes += fi.Size()
					}
				}
				c.SetResult("moved", moved)
				c.SetResult("skipped", skipped)
				c.SetResult("freedFromInstanceBytes", totalBytes)
				c.SetResult("dryRun", dryRun)
				c.Logf("归集 %d 个录像 / %s,跳过 %d 个(已存在)", len(moved), host.HumanBytes(totalBytes), skipped)
				return nil
			},
		},
	}
	return Plan{Steps: steps}, nil
}

// ---- host_cleanup(白名单模式 + dry-run 优先;破坏性操作需 confirm="CLEAN")--------

func hostCleanupPlan(spec Spec, instances []string, m *Manager) (Plan, error) {
	patterns := strSliceParam(spec.Params, "patterns")
	if len(patterns) == 0 {
		return Plan{}, fmt.Errorf("需要 patterns(可选:backup:old / logs:rotate / sniper:stubs / stale-instance-dirs / bridge:old)")
	}
	dryRun := boolParam(spec.Params, "dryRun", true) // 默认只读
	confirm := spec.Confirm
	if v, ok := spec.Params["confirm"].(string); ok && v != "" {
		confirm = v
	}
	if !dryRun && confirm != "CLEAN" {
		return Plan{}, fmt.Errorf(`实际清理需要二次确认(confirm 必须精确等于 "CLEAN";dryRun 默认 true)`)
	}
	maxAgeDays := intParam(spec.Params, "maxAgeDays", 30)

	steps := []Step{
		{
			Name: "清理(白名单模式)",
			Run: func(c *Ctx) error {
				items := make([]map[string]any, 0)
				var total int64
				bridgeDir := filepath.Dir(m.Dir()) // <config_dir>/jobs → <config_dir>
				for _, p := range patterns {
					got, err := cleanupPattern(c, p, maxAgeDays, dryRun, bridgeDir)
					if err != nil {
						c.Logf("模式 %s 失败:%v", p, err)
						continue
					}
					items = append(items, got...)
				}
				for _, it := range items {
					if b, ok := it["bytes"].(int64); ok {
						total += b
					}
				}
				sort.Slice(items, func(i, b int) bool {
					bi, _ := items[i]["bytes"].(int64)
					bb, _ := items[b]["bytes"].(int64)
					return bi > bb
				})
				c.SetResult("items", items)
				c.SetResult("totalBytes", total)
				c.SetResult("dryRun", dryRun)
				for _, it := range items {
					c.Logf("%s %s(%s)", map[bool]string{true: "[dry-run]", false: "[删除]"}[dryRun],
						it["path"], host.HumanBytes(toI64(it["bytes"])))
				}
				c.Logf("合计 %s,%d 项%s", host.HumanBytes(total), len(items), map[bool]string{true: "(dry-run,未删除)", false: ""}[dryRun])
				return nil
			},
		},
	}
	return Plan{Steps: steps}, nil
}

func toI64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	}
	return 0
}

// bridge:old 的常量(限制桥日志与终态任务日志的保留数量)
const (
	bridgeBakKeep      = 1               // cs.bak-* / bridge.py.bak-* 每类保留最新份数
	bridgeLogRotateMin = 8 << 20         // 桥日志超过 8 MiB 才轮转
	bridgeLogKeep      = 3               // 每类日志保留的最新 .gz 份数
	bridgeJobsKeep     = 20              // jobs/ 任务日志保留最新份数
	bridgeJobsSafeAge  = 5 * time.Minute // 5 分钟内还在写的任务日志不删(可能正在跑)
)

// rotateLogFile 把 path 的内容 gzip 到 <path>.<stamp>.gz 后把原文件截断为 0 字节
// (copytruncate 语义:写入方可能仍持有同一个 fd,直接删文件不会释放空间也不影响它继续写)。
func rotateLogFile(path, stamp string) {
	src, err := os.Open(path)
	if err != nil {
		return
	}
	defer src.Close()
	dst, err := os.Create(path + "." + stamp + ".gz")
	if err != nil {
		return
	}
	zw := gzip.NewWriter(dst)
	if _, err := io.Copy(zw, src); err != nil {
		zw.Close()
		dst.Close()
		_ = os.Remove(dst.Name())
		return
	}
	if err := zw.Close(); err != nil {
		dst.Close()
		_ = os.Remove(dst.Name())
		return
	}
	if err := dst.Close(); err != nil {
		return
	}
	_ = os.Truncate(path, 0)
}

// cleanupPattern 展开一个白名单模式,返回待删项(path/bytes/files)。
// bridgeDir = 桥配置目录(<config_dir>,即 jobs/ 的父目录);只有 bridge:old 用得到,其余模式可传空串。
func cleanupPattern(c *Ctx, pattern string, maxAgeDays int, dryRun bool, bridgeDir string) ([]map[string]any, error) {
	cutoff := time.Now().AddDate(0, 0, -maxAgeDays)
	out := make([]map[string]any, 0)
	// addItem:登记一项并用自定义动作执行(dry-run 只登记不动作);add = 整路径删除
	addItem := func(path string, bytes int64, files int, act func()) {
		out = append(out, map[string]any{"pattern": pattern, "path": path, "bytes": bytes, "files": files})
		if !dryRun && act != nil {
			act()
		}
	}
	add := func(path string) {
		bytes, files := host.DirSize(path)
		addItem(path, bytes, files, func() { _ = os.RemoveAll(path) })
	}
	switch pattern {
	case "backup:old":
		// 备份目录:每个根保留最新一份(避免把刚做的备份清掉),其余超过 maxAgeDays 的删除。
		// **两个候选根都扫**:`<archive_dir>/backup`(桥自己归档)与 `~/backup`(历次改动前的
		// 人工备份)—— 只认前者会在主机上"扫了个不存在的目录",静默漏掉真正占空间的那批
		// (主机环境验证 2026-09-21:archive_dir=/home/testuser/arena-data 时 backup:old 报 0 项,而 ~/backup 有 1.7 G)。
		roots := []string{backupRoot(c.Cfg), filepath.Join(homeDir(), "backup")}
		seen := map[string]bool{}
		sawAny := false
		for _, root := range roots {
			if seen[root] {
				continue
			}
			seen[root] = true
			entries, err := os.ReadDir(root)
			if err != nil {
				continue
			}
			sawAny = true
			type ent struct {
				path string
				mod  time.Time
			}
			all := make([]ent, 0, len(entries))
			for _, e := range entries {
				fi, err := e.Info()
				if err != nil {
					continue
				}
				all = append(all, ent{path: filepath.Join(root, e.Name()), mod: fi.ModTime()})
			}
			sort.Slice(all, func(i, b int) bool { return all[i].mod.After(all[b].mod) })
			for i, e := range all {
				if i == 0 || !e.mod.Before(cutoff) {
					continue
				}
				add(e.path)
			}
		}
		if !sawAny {
			return nil, fmt.Errorf("备份目录都不存在: %s", strings.Join(roots, ", "))
		}
		return out, nil
	case "logs:rotate":
		root := filepath.Join(MSMRoot(c.Cfg.MSMDir), "log")
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if info.ModTime().Before(cutoff) {
				add(path)
			}
			return nil
		})
		return out, nil
	case "sniper:stubs":
		root := MSMRoot(c.Cfg.MSMDir)
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "sniper_platform") || strings.HasPrefix(e.Name(), "run-in-sniper") {
				add(filepath.Join(root, e.Name()))
			}
		}
		return out, nil
	case "stale-instance-dirs":
		for _, dir := range host.StaleInstanceDirs(c.Cfg.MSMDir, c.Instances) {
			add(filepath.Join(MSMRoot(c.Cfg.MSMDir), dir))
		}
		return out, nil
	case "bridge:old":
		// 桥自身目录的冗余件(2026-09-22 用户定档「路线 B」;主机实测 151 M 里 ~110 M 是这些):
		//   ① 旧二进制/旧桥备份:cs.bak-* 与 bridge.py.bak-* 每类**只保留最新一份** —— 升级流程每次
		//      cp 出一份从不回收(09-21/22 两天堆了 13 份 ≈ 96 M);备份**不看 maxAgeDays**
		//      (刚升级完一个都删不掉就失去意义),留下的那份可用于回退;
		//   ② 桥日志 bridge.log / cs-agent.log:超过 8 MiB 就 gzip 轮转成 <名>.<stamp>.gz 并**截断原文件**
		//      (copytruncate 语义:守护/cron 可能还在往同一个 fd 写,直接删不释放空间),每类留最新 3 份 .gz;
		//   ③ Python 字节码缓存 __pycache__/(回退旧桥时自动重建);
		//   ④ jobs/ 任务日志:保留最新 20 份 *.log,其余删除(index.json / *.cancel / *.lock 一律不碰,
		//      且跳过 5 分钟内还在写的文件)。
		if bridgeDir == "" {
			return nil, fmt.Errorf("无法确定桥配置目录(内部错误:manager 未初始化)")
		}
		if _, err := os.Stat(bridgeDir); err != nil {
			return nil, fmt.Errorf("桥目录不存在: %s", bridgeDir)
		}
		type fileEnt struct {
			path string
			mod  time.Time
			size int64
		}
		listFiles := func(dir, suffix, prefix string) []fileEnt {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil
			}
			out := make([]fileEnt, 0, len(entries))
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) || !strings.HasPrefix(e.Name(), prefix) {
					continue
				}
				fi, err := e.Info()
				if err != nil {
					continue
				}
				out = append(out, fileEnt{filepath.Join(dir, e.Name()), fi.ModTime(), fi.Size()})
			}
			sort.Slice(out, func(i, j int) bool { return out[i].mod.After(out[j].mod) })
			return out
		}
		// ① 旧二进制 / 旧桥备份
		for _, prefix := range []string{"cs.bak-", "bridge.py.bak-"} {
			for i, f := range listFiles(bridgeDir, "", prefix) {
				if i < bridgeBakKeep || strings.HasSuffix(f.path, ".gz") {
					continue
				}
				path := f.path
				addItem(path, f.size, 1, func() { _ = os.Remove(path) })
			}
		}
		// ② 桥日志轮转(gzip + 截断)+ 旧 .gz 回收
		for _, name := range []string{"bridge.log", "cs-agent.log"} {
			path := filepath.Join(bridgeDir, name)
			if fi, err := os.Stat(path); err == nil && fi.Size() >= bridgeLogRotateMin {
				size := fi.Size()
				stamp := time.Now().Format("20060102-150405")
				addItem(path, size, 1, func() { rotateLogFile(path, stamp) })
			}
			for i, gz := range listFiles(bridgeDir, ".gz", name+".") {
				if i < bridgeLogKeep {
					continue
				}
				p := gz.path
				addItem(p, gz.size, 1, func() { _ = os.Remove(p) })
			}
		}
		// ③ Python 字节码缓存
		if fi, err := os.Stat(filepath.Join(bridgeDir, "__pycache__")); err == nil && fi.IsDir() {
			add(filepath.Join(bridgeDir, "__pycache__"))
		}
		// ④ jobs/ 任务日志(保留最新 N 份;index.json / *.cancel / *.lock 不匹配 *.log)
		for i, f := range listFiles(filepath.Join(bridgeDir, "jobs"), ".log", "") {
			if i < bridgeJobsKeep || time.Since(f.mod) < bridgeJobsSafeAge {
				continue
			}
			p := f.path
			addItem(p, f.size, 1, func() { _ = os.Remove(p) })
		}
		return out, nil

	default:
		return nil, fmt.Errorf("未知模式(白名单:backup:old / logs:rotate / sniper:stubs / stale-instance-dirs / bridge:old)")
	}
}
