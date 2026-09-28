// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package install 实现 `cs install`:新服务器引导与"补丁在位"体检。
//
// 分工:
//
//	L0 root 依赖      → 只**体检**(apt 命令交给运维;`cs install --check` 列出缺什么)
//	L1 Steam 会话     → 只体检(steamcmd 全量下载才需要;局域网复制不需要)
//	L2 msm 投放       → `cs install` 解包内嵌的 vendored msm(见 internal/install/msm)
//	L3 游戏文件       → 只体检(appmanifest/buildid/base game dir;导入见 README.md 的主机配置)
//	L4 主机侧补丁     → **体检**(cfg 链 / 工坊 symlink / 日志外移；插件由用户自行安装)
//	L5 桥自身         → `cs install` 生成 config.yaml + 安装 systemd user unit
//
// 一切写操作都幂等且可 dry-run;覆盖已存在的 msm 目录需要 --force(覆盖前自动备份)。
package install

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"arena/agent/internal/config"
	"arena/agent/internal/host"
)

// MSMFS 是内嵌的 vendored msm(来源/许可/本地改动见 msm/MODIFICATIONS.md)。
//
//go:embed all:msm
var MSMFS embed.FS

// unitTemplate 是 systemd user unit 模板(与仓库 deploy/cs-agent.service 同源;
// selftest 有"两处一致"断言防止漂移)。
//
//go:embed assets/cs-agent.service
var unitTemplate string

// DefaultMSMDir 是新机的默认 msm 落点。
const DefaultMSMDir = "~/cs2-multiserver-new"

// Options 是体检与投放共用的环境(全部可注入,便于测试)。
type Options struct {
	MSMDir     string // msm 目标目录(可含 ~)
	MSMExe     string // = <MSMDir>/cs2-server
	ConfigPath string // config.yaml 路径
	UnitPath   string // systemd user unit 落点(默认 ~/.config/systemd/user/cs-agent.service)
	Instances  []string
	Home       string
}

// CheckItem 是一项体检结果。
type CheckItem struct {
	Layer    string `json:"layer"`
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Critical bool   `json:"critical"` // false = 提示项(缺了不阻塞,例如 steamcmd 会话)
	Note     string `json:"note,omitempty"`
}

// criticalOK 报告所有阻塞项是否全过。
func CriticalOK(items []CheckItem) bool {
	for _, it := range items {
		if it.Critical && !it.OK {
			return false
		}
	}
	return true
}

// Check 逐层体检(只读,不落任何文件)。
func Check(o Options) []CheckItem {
	var items []CheckItem
	add := func(layer, name string, critical, ok bool, note string) {
		items = append(items, CheckItem{Layer: layer, Name: name, OK: ok, Critical: critical, Note: note})
	}

	// ---- L0 root 依赖----
	cmds := []string{"tmux", "jq", "inotifywait", "wget", "tar", "curl", "unzip", "patchelf"}
	var missing []string
	for _, c := range cmds {
		if _, err := exec.LookPath(c); err != nil {
			missing = append(missing, c)
		}
	}
	add("L0", "依赖命令("+strings.Join(cmds, "/")+")", true, len(missing) == 0,
		missingNote(missing, "apt install -y jq unzip inotify-tools patchelf tmux wget tar curl ca-certificates"))
	// 32 位库只有 Debian 系能查;非 Debian → 提示"无法判定",不阻塞
	if _, err := exec.LookPath("dpkg-query"); err != nil {
		add("L0", "32 位运行库(lib32gcc-s1/lib32stdc++6)", false, true, "无 dpkg-query,跳过(非 Debian 系请自行确认 steamcmd 能跑)")
	} else {
		var missLib []string
		for _, pkg := range []string{"lib32gcc-s1", "lib32stdc++6"} {
			if !dpkgInstalled(pkg) {
				missLib = append(missLib, pkg)
			}
		}
		add("L0", "32 位运行库(lib32gcc-s1/lib32stdc++6)", true, len(missLib) == 0,
			missingNote(missLib, "apt install -y lib32gcc-s1 lib32stdc++6"))
	}

	// ---- L1 Steam 会话(只有 steamcmd 全量下载需要)----
	steam := filepath.Join(o.Home, "Steam")
	if fi, err := os.Stat(steam); err == nil && fi.IsDir() {
		add("L1", "Steam 会话(~/Steam)", false, true, "已存在(steamcmd 可复用登录)")
	} else {
		add("L1", "Steam 会话(~/Steam)", false, false,
			"未发现:仅 steamcmd 全量下载需要(局域网复制游戏文件不需要);需要时用拥有 CS2 的账号跑一次 steamcmd")
	}

	// ---- L2 msm 投放 ----
	msmDir := expandHome(o.MSMDir, o.Home)
	if fi, err := os.Stat(msmDir); err == nil && fi.IsDir() {
		add("L2", "msm 目录("+o.MSMDir+")", true, true, "")
	} else {
		add("L2", "msm 目录("+o.MSMDir+")", true, false, "不存在 —— `cs install` 会从内嵌的 vendored msm 投放")
	}
	if fi, err := os.Stat(o.MSMExe); err == nil && !fi.IsDir() {
		add("L2", "msm 可执行("+o.MSMExe+")", true, true, "")
	} else {
		add("L2", "msm 可执行("+o.MSMExe+")", true, false, "缺失或不可执行")
	}
	// 本地补丁在位(CS2 更新不会动 msm,但手工换上游件会丢;见 msm/MODIFICATIONS.md)
	if ok, note := VerifyLocalPatches(msmDir); ok {
		add("L2", "msm 本地补丁(PORT+100 / assignInstancePort / 实例级 clone)", true, true, "")
	} else {
		add("L2", "msm 本地补丁(PORT+100 / assignInstancePort / 实例级 clone)", true, false, note)
	}

	// ---- L3 游戏文件 ----
	manifest := host.ManifestPath(msmDir)
	if build, err := host.ReadBuildID(manifest); err == nil {
		add("L3", "游戏文件(appmanifest buildid)", true, true, fmt.Sprintf("build %s", build))
	} else {
		add("L3", "游戏文件(appmanifest buildid)", true, false,
			fmt.Sprintf("%s 不可读 —— 请使用 SteamCMD 安装或导入已有 CS2 服务端文件", manifest))
	}
	baseGame := filepath.Join(host.BaseDir(msmDir), "game", "csgo")
	if fi, err := os.Stat(baseGame); err == nil && fi.IsDir() {
		add("L3", "base 游戏目录(game/csgo)", true, true, "")
	} else {
		add("L3", "base 游戏目录(game/csgo)", true, false, baseGame+" 不存在")
	}

	// ---- L4 主机侧补丁(有实例时才查;逐实例清点,别只看 base)----
	if len(o.Instances) > 0 {
		// 人机 cfg 链:平台每场覆写 arena_bots.cfg,而**主机侧**的 warmup/live_override 必须 exec 它。
		// 只有增强人机专用实例需要,故非阻塞;CS2 更新会还原 stock cfg → 这条就是复测点(README.md)。
		var withChain []string
		for _, inst := range o.Instances {
			for _, f := range []string{"warmup.cfg", "live_override.cfg"} {
				p := filepath.Join(host.InstanceDir(msmDir, inst), "game", "csgo", "cfg", "MatchZy", f)
				if raw, err := os.ReadFile(p); err == nil && strings.Contains(string(raw), "arena_bots.cfg") {
					withChain = append(withChain, inst)
					break
				}
			}
		}
		note := "在位实例:" + strings.Join(withChain, ", ")
		if len(withChain) == 0 {
			note = "无实例链接 arena_bots.cfg —— 请为增强人机实例配置 warmup/live_override 的 exec 链"
		}
		add("L4", "人机 cfg 链(warmup/live_override → exec arena_bots.cfg)", false, len(withChain) > 0, note)
		// 日志外移与工坊图共享(实例侧 symlink;仅提示,失败才带说明)
		inst := o.Instances[0]
		instDir := host.InstanceDir(msmDir, inst)
		logs := filepath.Join(instDir, "game", "csgo", "logs")
		logsNote := ""
		if !isSymlink(logs) {
			logsNote = fmt.Sprintf("@%s 的 logs 不是符号链接:日志会保存在实例目录里", inst)
		}
		add("L4", "日志外移(实例 game/csgo/logs 符号链接)", false, isSymlink(logs), logsNote)
		// 工坊图共享:整个组只允许**一个**实目录(共享源,通常是 main),其余实例必须是符号链接。
		// 逐个实例查(不看 o.Instances[0]:main 就是那个实目录,不该被当成缺符号链接)。
		var owners []string
		linked := 0
		for _, in := range o.Instances {
			sa := filepath.Join(host.InstanceDir(msmDir, in), "game", "bin", "linuxsteamrt64", "steamapps")
			if _, err := os.Stat(sa); err != nil {
				continue // 该实例还没建起来
			}
			if isSymlink(sa) {
				linked++
			} else {
				owners = append(owners, in)
			}
		}
		saNote := fmt.Sprintf("共享源:@%s;其余 %d 个实例为符号链接", strings.Join(owners, ","), linked)
		if len(owners) > 1 {
			saNote = "多个实例各自是实目录(工坊图会重复占用): " + strings.Join(owners, ", ") + " —— 只应留一个共享源"
		}
		add("L4", "工坊图共享(组内单一实目录 + 其余符号链接)", false, len(owners) <= 1, saNote)
	}

	// ---- L5 桥自身 ----
	if _, err := os.Stat(o.ConfigPath); err == nil {
		if cfg, _, err := config.Load(o.ConfigPath); err != nil {
			add("L5", "config.yaml 可解析", true, false, err.Error())
		} else {
			note := o.ConfigPath
			if strings.Contains(cfg.Token, "<") || strings.Contains(cfg.BackendWS, "<") {
				note = "还是占位符:用 `cs config set token <平台 game_servers.bridge_token>` 填真实值(backend_ws 同理)"
			}
			add("L5", "config.yaml 可解析", true, true, note)
		}
	} else {
		add("L5", "config.yaml 可解析", true, false, "不存在 —— `cs install` 会生成(需要 token 与 backend_ws)")
	}
	if _, err := os.Stat(o.UnitPath); err == nil {
		add("L5", "systemd user unit 已安装", false, true, o.UnitPath)
	} else {
		add("L5", "systemd user unit 已安装", false, false, "未安装 —— `cs install` 会写入 "+o.UnitPath)
	}
	statusPath := filepath.Join(filepath.Dir(o.ConfigPath), "agent.status.json")
	if st, err := os.Stat(statusPath); err == nil && time.Since(st.ModTime()) < 60*time.Second {
		add("L5", "守护心跳(agent.status.json)", false, true, "新鲜(守护在跑)")
	} else {
		add("L5", "守护心跳(agent.status.json)", false, false, "无/陈旧:守护未运行(装完 unit 后 `systemctl --user start cs-agent`)")
	}
	return items
}

// ApplyOptions 是投放参数。
type ApplyOptions struct {
	Options
	MSMFrom   string // 外部 msm 源目录(默认用内嵌的 vendored msm)
	Force     bool   // 目标已存在时覆盖(覆盖前备份)
	WriteCfg  bool   // 生成 config.yaml(缺省 true;文件已存在则跳过)
	WriteUnit bool   // 安装 systemd unit(缺省 true)
	Token     string
	BackendWS string
	DryRun    bool
	Log       func(format string, args ...any)
}

// Apply 执行 L2(msm 投放)+ L5(config.yaml / unit);返回执行的步骤说明。
func Apply(o ApplyOptions) ([]string, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var done []string

	// ---- L2:msm 投放 ----
	steps, err := DeployMSM(DeployOptions{
		Target: expandHome(o.MSMDir, o.Home), From: o.MSMFrom, Force: o.Force, DryRun: o.DryRun, Log: logf,
	})
	done = append(done, steps...)
	if err != nil {
		return done, err
	}

	// ---- L5a:config.yaml ----
	if o.WriteCfg {
		if _, err := os.Stat(o.ConfigPath); err == nil {
			logf("[skip] config.yaml 已存在(%s);改 key/value 用 `cs config set <键> <值>`", o.ConfigPath)
		} else if o.DryRun {
			logf("[dry-run] 将生成 config.yaml:%s(含 token/backend_ws)", o.ConfigPath)
			done = append(done, "config.yaml(计划)")
		} else {
			if err := WriteConfig(o.ConfigPath, ConfigSpec{
				Token: o.Token, BackendWS: o.BackendWS, MSMDir: o.MSMDir,
			}); err != nil {
				return done, fmt.Errorf("生成 config.yaml 失败: %w", err)
			}
			logf("[ok] config.yaml 已生成:%s(权限 0600;token 请与平台 game_servers.bridge_token 一致)", o.ConfigPath)
			done = append(done, "config.yaml")
		}
	}

	// ---- L5b:systemd user unit ----
	if o.WriteUnit {
		if o.DryRun {
			logf("[dry-run] 将写入 unit:%s", o.UnitPath)
			done = append(done, "cs-agent.service(计划)")
		} else {
			backup, err := WriteUnitFile(o.UnitPath, o.ConfigPath)
			if err != nil {
				return done, fmt.Errorf("写入 unit 失败: %w", err)
			}
			note := ""
			if backup != "" {
				note = "(旧 unit 已备份 " + backup + ")"
			}
			logf("[ok] systemd user unit 已写入:%s%s", o.UnitPath, note)
			done = append(done, "cs-agent.service")
		}
	}
	return done, nil
}

// DeployOptions 是 msm 投放参数。
type DeployOptions struct {
	Target string // 目标目录(展开后的绝对路径)
	From   string // 源目录(空 = 内嵌 vendored msm)
	Force  bool
	DryRun bool
	Log    func(format string, args ...any)
}

// DeployMSM 把 vendored msm 落盘到 Target(幂等:已有内容且未 --force 时跳过)。
//
// 覆盖策略:先整体**改名备份**为 <Target>.bak-<stamp>(同目录 rename,瞬时),再写新树 ——
// 不做"逐文件覆盖",避免旧文件残留造成版本混杂。
func DeployMSM(o DeployOptions) ([]string, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var steps []string
	files, err := msmSourceFiles(o.From)
	if err != nil {
		return nil, err
	}
	exists := dirHasContent(o.Target)
	if exists && !o.Force {
		logf("[skip] msm 目录已存在且非空:%s(要覆盖加 --force,覆盖前会备份)", o.Target)
		steps = append(steps, "msm(已存在,跳过)")
		return steps, nil
	}
	if o.DryRun {
		logf("[dry-run] 将投放 %d 个文件到 %s%s", len(files), o.Target, map[bool]string{true: "(先备份旧目录)"}[exists])
		return append(steps, "msm(计划)"), nil
	}
	if exists {
		backup := fmt.Sprintf("%s.bak-%s", o.Target, time.Now().Format("20060102150405"))
		if _, err := os.Stat(backup); err == nil {
			return steps, fmt.Errorf("备份路径已存在: %s", backup)
		}
		if err := os.Rename(o.Target, backup); err != nil {
			return steps, fmt.Errorf("备份旧 msm 目录失败: %w", err)
		}
		logf("[ok] 旧 msm 目录已备份:%s", backup)
	}
	if err := os.MkdirAll(o.Target, 0o755); err != nil {
		return steps, err
	}
	for _, rel := range files {
		dst := filepath.Join(o.Target, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return steps, err
		}
		raw, err := readMSMSource(o.From, rel)
		if err != nil {
			return steps, err
		}
		mode := os.FileMode(0o644)
		if rel == "msm" {
			mode = 0o755 // 唯一需要可执行位的文件(其余 .sh 都是被 source 的)
		}
		if err := os.WriteFile(dst, raw, mode); err != nil {
			return steps, err
		}
	}
	// cs2-server:上游是 cs2-server -> msm 的符号链接(嵌入件带不了符号链接)
	link := filepath.Join(o.Target, "cs2-server")
	if err := os.Symlink("msm", link); err != nil && !os.IsExist(err) {
		return steps, fmt.Errorf("创建 cs2-server 符号链接失败: %w", err)
	}
	logf("[ok] msm 已投放:%s(%d 个文件,含 cs2-server -> msm)", o.Target, len(files))
	return append(steps, "msm"), nil
}

// msmSourceFiles 列出源文件(内嵌或外部目录),相对路径、已排序、跳过 patches/ 与说明文件。
func msmSourceFiles(from string) ([]string, error) {
	if from != "" {
		var out []string
		err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(from, p)
			if rel == "." || skipMSMPath(rel) {
				if d.IsDir() && rel != "." {
					return fs.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() {
				out = append(out, filepath.ToSlash(rel))
			} else if d.Type()&os.ModeSymlink != 0 {
				return nil // 源里的符号链接不复制(安装期重建 cs2-server)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(out)
		return out, nil
	}
	var out []string
	err := fs.WalkDir(MSMFS, "msm", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "msm"), "/")
		if rel == "" || skipMSMPath(rel) {
			if d.IsDir() && rel != "" {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// skipMSMPath 是"不投放"的路径(我们的补丁件与改动说明留在仓库,不进主机目录)。
func skipMSMPath(rel string) bool {
	rel = filepath.ToSlash(rel)
	return rel == "cs2-server" || rel == "MODIFICATIONS.md" || strings.HasPrefix(rel, "patches/")
}

func readMSMSource(from, rel string) ([]byte, error) {
	if from != "" {
		return os.ReadFile(filepath.Join(from, filepath.FromSlash(rel)))
	}
	return MSMFS.ReadFile("msm/" + rel)
}

// ConfigSpec 是生成 config.yaml 的入参(其余键用默认值与注释说明)。
type ConfigSpec struct {
	Token     string
	BackendWS string
	MSMDir    string
}

// WriteConfig 生成最小键集的 config.yaml(权限 0600:含 token),并**回读校验**能解析。
func WriteConfig(path string, spec ConfigSpec) error {
	if spec.Token == "" {
		spec.Token = "<平台 game_servers.bridge_token>"
	}
	if spec.BackendWS == "" {
		spec.BackendWS = "ws://<平台地址>:8080/api/agent"
	}
	if spec.MSMDir == "" {
		spec.MSMDir = DefaultMSMDir
	}
	body := fmt.Sprintf(`# CS2 Arena 桥 v2 配置(最小键集;改值用 `+"`cs config set <键> <值>`"+`,改完自动 SIGHUP 热重载)
# 说明与全部可选键见 README.md;旧 config.json 的键一律不读(只警告忽略)。

# ---- 必填 ----
token: %s
backend_ws: %s
msm_dir: %s

# ---- 可选(不写即用默认)----
# demo_dir: ~/arena-data/demos        # 录像归集落点
# archive_dir: ~/arena-data           # 比赛 JSON/日志归档根
# min_free_bytes: 16106127360         # 建实例/更新的磁盘余量门槛(默认 15 GiB)
# console_tail_ms: 1000
# health_push_ms: 5000
`, spec.Token, spec.BackendWS, spec.MSMDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}
	// 回读校验:必填键缺失/格式错必须当场发现(否则守护起不来)
	if _, _, err := config.Load(path); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("生成的 config.yaml 校验失败(已撤回): %w", err)
	}
	return nil
}

// WriteUnitFile 写 systemd user unit(ExecStart 指向**本二进制所在路径**与配置路径)。
func WriteUnitFile(unitPath, configPath string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	absCfg, err := filepath.Abs(configPath)
	if err != nil {
		return "", err
	}
	body := strings.Replace(unitTemplate,
		"ExecStart=%h/data/bridge/cs agent --config %h/data/bridge/config.yaml",
		fmt.Sprintf("ExecStart=%s agent --config %s", exe, absCfg), 1)
	// 幂等:内容一致就不动(避免每次 install 都产生 .bak 垃圾)
	if old, err := os.ReadFile(unitPath); err == nil && string(old) == body {
		return "", nil
	}
	backup := ""
	if _, err := os.Stat(unitPath); err == nil {
		backup = unitPath + ".bak-" + time.Now().Format("20060102150405")
		if err := os.Rename(unitPath, backup); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(unitPath, []byte(body), 0o644); err != nil {
		return "", err
	}
	return backup, nil
}

// UnitTemplate 暴露模板文本(selftest 用它断言与 deploy/cs-agent.service 一致)。
func UnitTemplate() string { return unitTemplate }

// expandHome 展开 ~/ 前缀。
func expandHome(p, home string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
}

func dirHasContent(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

func isSymlink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func missingNote(missing []string, hint string) string {
	if len(missing) == 0 {
		return ""
	}
	return "缺失: " + strings.Join(missing, ", ") + " —— " + hint
}

func dpkgInstalled(pkg string) bool {
	out, err := exec.Command("dpkg-query", "-W", "-f=${Status}", pkg).Output()
	return err == nil && strings.Contains(string(out), "install ok installed")
}

// VerifyLocalPatches 检查 vendored msm 的三处关键本地改动是否在位(见 msm/MODIFICATIONS.md)。
// `cs install --check` 与 selftest 都用它:CS2 更新不会动 msm,但手工换上游件会把这些补丁丢掉。
func VerifyLocalPatches(msmDir string) (bool, string) {
	checks := []struct {
		rel  string
		want string
		name string
	}{
		{"cs2/app/cfg/gotv.conf", "PORT + 100", "GOTV=端口+100"},
		{"cs2/app/functions/instance.sh", "App::assignInstancePort", "自动分配端口"},
		{"program/Core/BaseInstallation/functions.sh", "$INSTANCE", "实例级 clone 注册"},
	}
	var bad []string
	for _, c := range checks {
		raw, err := os.ReadFile(filepath.Join(msmDir, filepath.FromSlash(c.rel)))
		if err != nil || !strings.Contains(string(raw), c.want) {
			bad = append(bad, c.name)
		}
	}
	if len(bad) > 0 {
		return false, "缺失/被上游件覆盖: " + strings.Join(bad, ", ") +
			"(重新投放: cs install --force;或 patch -p1 < internal/install/msm/patches/arena-local.patch)"
	}
	return true, ""
}
