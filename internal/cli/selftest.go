// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"arena/agent/internal/config"
	"arena/agent/internal/fsx"
	"arena/agent/internal/host"
	"arena/agent/internal/install"
	"arena/agent/internal/job"
	"arena/agent/internal/msm"
	"arena/agent/internal/ops"
	"arena/agent/internal/protocol"
	"arena/agent/internal/registry"
)

// selftest 执行离线自检，不连接后端或修改真实实例目录。
func runSelftest(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "输出单行 JSON(smoke 断言用)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	results := selfcheck()
	if *asJSON {
		raw, _ := json.Marshal(results)
		fmt.Fprintln(stdout, string(raw))
	} else {
		for _, k := range sortedKeys(results) {
			mark := "true "
			if !results[k] {
				mark = "FALSE"
			}
			fmt.Fprintf(stdout, "  [%s] %s\n", mark, k)
		}
	}
	for _, v := range results {
		if !v {
			return ExitFailure
		}
	}
	return ExitOK
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ { // 项数很少,插入排序足够且不引入依赖
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func selfcheck() map[string]bool {
	out := map[string]bool{}
	tmp, err := os.MkdirTemp("", "cs-selftest-")
	if err != nil {
		return map[string]bool{"tmpdir": false}
	}
	defer os.RemoveAll(tmp)

	// ---- 配置:必填校验 / 未知键警告 / mode 已弃用 ----
	writeFile := func(name, body string) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return ""
		}
		return p
	}
	{
		// 缺 backend_ws → 必须报错(这是唯一会拒绝启动的情况)
		p := writeFile("missing.yaml", "token: t\nmsm_dir: /tmp\n")
		_, _, err := config.Load(p)
		out["config_required_keys_rejected"] = err != nil && strings.Contains(err.Error(), "backend_ws")
	}
	{
		p := writeFile("unknown.yaml", "token: t\nbackend_ws: ws://x/api/agent\nmsm_dir: /tmp\nfoo: 1\n")
		_, warns, err := config.Load(p)
		joined := strings.Join(warns, " | ")
		out["config_unknown_key_warns"] = err == nil && strings.Contains(joined, "未知键") && strings.Contains(joined, "foo")
	}
	{
		// mode 已弃用:任何取值都只警告、不改变行为(reverse 是唯一模式)
		p := writeFile("mode.yaml", "mode: http\ntoken: t\nbackend_ws: ws://x/api/agent\nmsm_dir: /tmp\n")
		cfg, warns, err := config.Load(p)
		joined := strings.Join(warns, " | ")
		out["config_mode_deprecated_ignored"] = err == nil && strings.Contains(joined, "mode") &&
			cfg != nil && cfg.MSM == filepath.Join("/tmp", "cs2-server")
	}

	// ---- msm 层:ok 语义 / 状态解析 / 锁忙 ----
	mkRunner := func(bin string) *msm.Runner {
		return msm.NewRunner(func() msm.RunnerConfig {
			return msm.RunnerConfig{MSM: bin, MSMDir: tmp, TimeoutS: 5, StateCacheS: 2}
		})
	}
	mkOps := func(r *msm.Runner) *ops.Ops {
		return &ops.Ops{Runner: r, Paths: msm.NewPaths(tmp), Names: func() []string { return []string{"main"} }}
	}

	rTrue, rFalse := mkRunner("/bin/true"), mkRunner("/bin/false")
	oTrue, oFalse := mkOps(rTrue), mkOps(rFalse)

	// 白名单:非清单实例一律拒绝(且不得产生副作用)
	out["allowlist_send"] = oTrue.Handle("send", "other", map[string]any{"cmd": "x"})["ok"] == false
	out["allowlist_status"] = oTrue.Handle("status", "other", nil)["ok"] == false
	{
		res := oTrue.Handle("matchfile", "other", map[string]any{"filename": "p.cfg", "subdir": "cfg", "text": "x"})
		out["allowlist_matchfile"] = res["ok"] == false && !exists(filepath.Join(tmp, "cfg", "p.cfg"))
	}

	// ok 恒由 returncode 决定(v1 曾因缺省 true 把失败吞成成功)
	out["ok_true_on_rc0"] = oTrue.Handle("send", "main", map[string]any{"cmd": "x"})["ok"] == true
	out["ok_false_on_rc1"] = oFalse.Handle("send", "main", map[string]any{"cmd": "x"})["ok"] == false

	// 命令异常收口成回包,不冒泡(不 panic、不断通道)
	{
		res := mkOps(mkRunner(filepath.Join(tmp, "no-such-msm"))).Handle("status", "main", nil)
		errStr, _ := res["error"].(string)
		out["dispatch_isolates_error"] = res["ok"] == false && errStr != ""
	}

	// 显式 status 回包必须带 state(后端不再解析 stdout 猜状态)
	{
		fake := writeFile("msm-running.sh", "#!/bin/sh\necho RUNNING\n")
		_ = os.Chmod(fake, 0o755)
		res := mkOps(mkRunner(fake)).Handle("status", "main", nil)
		out["status_has_state"] = res["ok"] == true && res["state"] == "RUNNING"
	}

	// 只读查询锁忙即回退(不排队),忙碌回包是失败态
	{
		r := mkRunner("/bin/true")
		r.Lock.Acquire()
		_, err := r.Run("main", "status", nil, nil, false)
		out["readop_skips_when_busy"] = err == msm.ErrBusy
		r.Lock.Release()
		busy := ops.ReplyBusy(map[string]any{"instance": "main"})
		out["busy_reply_is_error"] = busy["ok"] == false && strings.Contains(fmt.Sprint(busy["error"]), "msm busy")
	}

	// 增量日志:末尾半行既不返回也不推进 offset(否则行首永久丢失)
	{
		logPath := filepath.Join(tmp, "main.log")
		_ = os.WriteFile(logPath, []byte("PARTIAL-HEAD"), 0o644)
		lines1, off1, err1 := fsx.ReadNewLines(logPath, 0)
		_ = os.WriteFile(logPath, []byte("PARTIAL-HEAD-TAIL\n"), 0o644)
		lines2, _, err2 := fsx.ReadNewLines(logPath, off1)
		out["partial_line_preserved"] = err1 == nil && err2 == nil &&
			len(lines1) == 0 && off1 == 0 && len(lines2) == 1 && lines2[0] == "PARTIAL-HEAD-TAIL"
	}

	// 原子写:内容正确且不留 .tmp-<pid> 残留
	{
		target := filepath.Join(tmp, "atomic.json")
		n, err := fsx.AtomicWriteText(target, "中文内容")
		back, _ := os.ReadFile(target)
		leftovers, _ := filepath.Glob(target + ".tmp-*")
		out["atomic_write_clean"] = err == nil && n == len("中文内容") &&
			string(back) == "中文内容" && len(leftovers) == 0
	}

	// 未知 op 必须明确回 unknown op(不能静默成功)
	{
		res := oTrue.Handle("nope", "", nil)
		out["unknown_op_rejected"] = res["ok"] == false && res["error"] == "unknown op: nope"
	}

	// health op:按白名单全量返回实例状态映射
	{
		res := oTrue.Handle("health", "", nil)
		inst, _ := res["instances"].(map[string]string)
		out["health_op_lists_instances"] = res["ok"] == true && len(inst) == 1 && inst["main"] != ""
	}

	// 上行帧字段名大小写敏感:改错会被后端静默丢弃
	{
		raw, _ := json.Marshal(protocol.HealthPush{Type: protocol.TypePush, Kind: protocol.KindHealth, Instances: map[string]string{"main": "RUNNING"}})
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		ok := hasKeys(m, "type", "kind", "instances") && len(m) == 3

		raw2, _ := json.Marshal(protocol.ConsolePush{Type: protocol.TypePush, Kind: protocol.KindConsole, Instance: "main", Path: "/x.log", Offset: 7, Lines: []string{"a\n"}})
		var m2 map[string]any
		_ = json.Unmarshal(raw2, &m2)
		ok = ok && hasKeys(m2, "type", "kind", "instance", "path", "offset", "lines")
		out["frame_field_names"] = ok
	}

	// hello 必须声明能力(v2 靠它换 hello_ack;不声明则后端按旧桥对待)
	{
		raw, _ := json.Marshal(protocol.Hello{Type: protocol.TypeHello, Token: "t", Capabilities: []string{protocol.CapConfigSync}})
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		caps, _ := m["capabilities"].([]any)
		out["hello_declares_capabilities"] = len(caps) == 1 && caps[0] == protocol.CapConfigSync
	}

	// ---- 文件类 op(matchfile/log/mapfile/workshop/matchcleanup)----
	// stub 语义:ARENA_STUB_DIR 决定 csgo 目录、maps/ 与 workshop/content/730 的落点
	stubDir := filepath.Join(tmp, "stub")
	_ = os.MkdirAll(filepath.Join(stubDir, "maps"), 0o755)
	_ = os.MkdirAll(filepath.Join(stubDir, "workshop", "content", "730", "3344448932"), 0o755)
	_ = os.WriteFile(filepath.Join(stubDir, "workshop", "content", "730", "3344448932", "a.vpk"), bytes.Repeat([]byte("x"), 1234), 0o644)
	_ = os.WriteFile(filepath.Join(stubDir, "maps", "aim_gryn.vpk"), []byte("mapfile"), 0o644)
	// msm.NewPaths 在构造时读该环境变量 → 必须在建 Ops 之前设置
	_ = os.Setenv("ARENA_STUB_DIR", stubDir)

	mkStubOps := func() *ops.Ops {
		r := mkRunner("/bin/true")
		o := &ops.Ops{
			Runner:          r,
			Paths:           msm.NewPaths(tmp),
			Names:           func() []string { return []string{"main"} },
			ArchiveOverride: filepath.Join(tmp, "arena-data"),
		}
		o.Hub = ops.NewConsoleHub(o)
		return o
	}
	so := mkStubOps()

	// matchfile:校验顺序与失败文案
	out["matchfile_bad_filename"] = so.Handle("matchfile", "main", map[string]any{"filename": "../x", "text": "y"})["error"] == "bad filename"
	out["matchfile_bad_subdir"] = so.Handle("matchfile", "main", map[string]any{"filename": "a.cfg", "subdir": "a/b", "text": "y"})["error"] == "subdir invalid"
	out["matchfile_text_not_string"] = so.Handle("matchfile", "main", map[string]any{"filename": "a.cfg", "text": 12})["error"] == "text must be a string"
	out["matchfile_json_required"] = so.Handle("matchfile", "main", map[string]any{"filename": "a.cfg"})["error"] == "json must be an object"
	out["matchfile_too_large"] = so.Handle("matchfile", "main", map[string]any{"filename": "a.cfg", "text": strings.Repeat("x", 1024*1024+1)})["error"] == "payload too large"
	{
		// 中文原样落盘 + subdir 自动建目录 + 不留 tmp
		res := so.Handle("matchfile", "main", map[string]any{"filename": "arena_bots.cfg", "subdir": "cfg", "text": "bot_quota 0\n中文\n"})
		p, _ := res["path"].(string)
		back, _ := os.ReadFile(p)
		leftovers, _ := filepath.Glob(p + ".tmp-*")
		out["matchfile_writes_atomically"] = res["ok"] == true && string(back) == "bot_quota 0\n中文\n" &&
			res["bytes"] == len("bot_quota 0\n中文\n") && len(leftovers) == 0 && strings.HasSuffix(p, filepath.Join("cfg", "arena_bots.cfg"))
	}

	// mapfile_status:本地自维护社区图预检
	{
		okRes := so.Handle("mapfile_status", "main", map[string]any{"filename": "aim_gryn.vpk"})
		missRes := so.Handle("mapfile_status", "main", map[string]any{"filename": "nope.vpk"})
		badRes := so.Handle("mapfile_status", "main", map[string]any{"filename": "../x"})
		out["mapfile_status_presence"] = okRes["ok"] == true && okRes["present"] == true && okRes["size"] == int64(7) &&
			missRes["present"] == false && badRes["error"] == "bad filename"
	}

	// workshop_status:共享目录盘点(数字目录 + 递归字节数)
	{
		res := so.Handle("workshop_status", "main", nil)
		items, _ := res["items"].(map[string]any)
		entry, _ := items["3344448932"].(map[string]any)
		out["workshop_items_sizes"] = res["ok"] == true && entry != nil && entry["size"] == int64(1234)
	}

	// matchcleanup:单场次删 4 类 + 归档比赛 JSON + 录像不动
	{
		gdir := stubDir
		_ = os.MkdirAll(filepath.Join(gdir, "MatchZyDataBackup"), 0o755)
		_ = os.MkdirAll(filepath.Join(gdir, "MatchZyPlayerNames"), 0o755)
		_ = os.MkdirAll(filepath.Join(gdir, "MatchZy"), 0o755)
		files := []string{
			filepath.Join(gdir, "MatchZyDataBackup", "matchzy_7_0_round00.json"),
			filepath.Join(gdir, "matchzy_7_0_round00.txt"),
			filepath.Join(gdir, "MatchZyPlayerNames", "Match_7.ini"),
			filepath.Join(gdir, "backup_round03.txt"),
			filepath.Join(gdir, "matchzy_load_7.json"),
			filepath.Join(gdir, "MatchZy", "match_7.dem"),
		}
		for _, f := range files {
			_ = os.WriteFile(f, []byte("x"), 0o644)
		}
		res := so.Handle("matchcleanup", "main", map[string]any{"matchId": "7"})
		demKept := exists(filepath.Join(gdir, "MatchZy", "match_7.dem"))
		archived := exists(filepath.Join(tmp, "arena-data", "matchjson", "main", "matchzy_load_7.json"))
		deleted, _ := res["deleted"].([]string)
		moved, _ := res["moved"].([]string)
		out["matchcleanup_single_match"] = res["ok"] == true && len(deleted) == 4 && len(moved) == 1 && demKept && archived &&
			res["archive_dir"] == filepath.Join(tmp, "arena-data")
		// matchId 非法
		out["matchcleanup_bad_matchid"] = so.Handle("matchcleanup", "main", map[string]any{"matchId": "abc"})["error"] == "matchId must be digits"
		// 全量整理保护进行中的场次
		_ = os.WriteFile(filepath.Join(gdir, "MatchZyDataBackup", "matchzy_8_0_round00.json"), []byte("x"), 0o644)
		_ = os.WriteFile(filepath.Join(gdir, "MatchZyDataBackup", "matchzy_9_0_round00.json"), []byte("x"), 0o644)
		_ = os.WriteFile(filepath.Join(gdir, "matchzy_load_8.json"), []byte("x"), 0o644)
		_ = os.WriteFile(filepath.Join(gdir, "matchzy_load_9.json"), []byte("x"), 0o644)
		sweep := so.Handle("matchcleanup", "main", map[string]any{"all": true, "keepMatchIds": []any{"8"}})
		out["matchcleanup_keep_ids"] = sweep["ok"] == true &&
			exists(filepath.Join(gdir, "MatchZyDataBackup", "matchzy_8_0_round00.json")) &&
			!exists(filepath.Join(gdir, "MatchZyDataBackup", "matchzy_9_0_round00.json")) &&
			exists(filepath.Join(gdir, "matchzy_load_8.json")) &&
			!exists(filepath.Join(gdir, "matchzy_load_9.json"))
	}

	// console:三条校验文案(下发前拦下)
	{
		o := mkStubOps()
		reqOnly := o.Handle("console", "main", map[string]any{})["error"] == "command required"
		multi := o.Handle("console", "main", map[string]any{"command": "a\nb"})["error"] == "command must be single line"
		long := o.Handle("console", "main", map[string]any{"command": strings.Repeat("a", 2001)})["error"] == "command too long (max 2000)"
		blank := o.Handle("console", "main", map[string]any{"command": "   "})["error"] == "command required"
		out["console_validation"] = reqOnly && multi && long && blank
	}

	// log:尾段行数语义(`lines or 100` + 夹取 1..2000)
	{
		logPath := filepath.Join(stubDir, "main.log")
		_ = os.WriteFile(logPath, []byte("l1\nl2\nl3\n"), 0o644)
		o := mkStubOps()
		all := o.Handle("log", "main", map[string]any{"lines": 0})  // 数值 0 → 100(取全部 3 行)
		one := o.Handle("log", "main", map[string]any{"lines": -5}) // 负数 → 夹取 1
		linesAll, _ := all["lines"].([]string)
		linesOne, _ := one["lines"].([]string)
		out["log_lines_semantics"] = all["ok"] == true && len(linesAll) == 3 && linesOne != nil && len(linesOne) == 1 && linesOne[0] == "l3\n"
	}

	// probe:URL 校验(仅 http(s));不发起真实网络请求
	out["probe_url_validation"] = so.Handle("probe", "", map[string]any{"url": "ftp://x"})["error"] == "url must start with http(s)://"

	// parse_state:已修复 v1 的 "not running" 误判(有意差异 #1)
	out["parse_state_not_running_fixed"] = msm.ParseState("server not running") == "STOPPED" &&
		msm.ParseState("Server is RUNNING") == "RUNNING"

	// ---- 注册表 / 主机信息 / 配置改写 ----
	// 端口真源 = cfg/inst-*/server.conf 的 PORT=;GOTV = PORT+100;编号重扫不重排
	{
		m2 := filepath.Join(tmp, "m2")
		msmDir := filepath.Join(m2, "cs2-multiserver-new")
		_ = os.MkdirAll(msmDir, 0o755)
		for name, port := range map[string]string{"main": "27015", "match1": "27016"} {
			dir := filepath.Join(m2, "msm.d", "cs2", "cfg", "inst-"+name)
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(filepath.Join(dir, "server.conf"), []byte("__PORT__=\"27015\"\nPORT=\""+port+"\"\n"), 0o644)
		}
		scanned, err := registry.Scan(msmDir)
		ok := err == nil && len(scanned) == 2
		ports := map[string]int{}
		for _, it := range scanned {
			ports[it.Name] = it.Port
		}
		ok = ok && ports["main"] == 27015 && ports["match1"] == 27016
		merged := registry.MergeIdx([]registry.Instance{{Name: "main"}, {Name: "match1"}}, []registry.Instance{{Idx: 3, Name: "match1"}}, 0)
		for _, it := range merged {
			if it.Name == "match1" && it.Idx != 3 {
				ok = false // 已有编号必须保留
			}
			if it.Name == "main" && it.Idx == 3 {
				ok = false // 新实例编号不能与已有冲突
			}
		}
		out["registry_scan_and_idx"] = ok
	}

	// appmanifest 的 buildid 解析(真机为 Valve KV 文本)
	{
		p := filepath.Join(tmp, "appmanifest_730.acf")
		_ = os.WriteFile(p, []byte("AppState\n{\n\t\"appid\"\t\t\"730\"\n\t\"buildid\"\t\t\"25218825\"\n}\n"), 0o644)
		got, err := host.ReadBuildID(p)
		out["buildid_parse"] = err == nil && got == "25218825"
	}

	// cs config set:保留注释与顺序、写前备份、写完能通过 config.Load 校验
	{
		p := filepath.Join(tmp, "cfg-write.yaml")
		body := "# 注释必须保留\ntoken: t\nbackend_ws: ws://x/api/agent\nmsm_dir: /tmp\n"
		_ = os.WriteFile(p, []byte(body), 0o600)
		backup, err := setConfigValue(p, "console_tail_ms", "400")
		after, _ := os.ReadFile(p)
		bak, _ := os.ReadFile(backup)
		out["config_set_preserves_comments"] = err == nil &&
			strings.Contains(string(after), "# 注释必须保留") &&
			strings.Contains(string(after), "console_tail_ms: 400") &&
			strings.HasSuffix(string(after), "\n") &&
			string(bak) == body
	}

	// 弃用键(port/send_prefixes/instances…)只警告、不影响加载
	{
		p := filepath.Join(tmp, "deprecated.yaml")
		_ = os.WriteFile(p, []byte("token: t\nbackend_ws: ws://x/api/agent\nmsm_dir: /tmp\nport: 3001\nsend_prefixes: [a]\ninstances: [main]\n"), 0o644)
		cfg, warns, err := config.Load(p)
		joined := strings.Join(warns, " | ")
		out["config_deprecated_keys_ignored"] = err == nil && cfg != nil && strings.Contains(joined, "已弃用键") &&
			strings.Contains(joined, "port") && strings.Contains(joined, "instances")
	}

	// ---- job 框架----------------------------------------------------------
	// 引擎语义(按组独占/两种取消/日志半行)由 go test 的 internal/job 覆盖;
	// 这里验证**二进制侧**可离线验证的部分:op 注册与回包形状、任务生命周期与日志落盘、
	// 危险操作二次确认、job_report 收敛上报、重启后中断任务不谎报 running。
	{
		jdir := filepath.Join(tmp, "jobs")
		jcfg := config.Defaults()
		jcfg.MSMDir = tmp
		jcfg.ArchiveDir = filepath.Join(tmp, "arena-data")
		var mu sync.Mutex
		var frames []any
		mgr := job.New(job.Options{
			Dir:     jdir,
			Cfg:     func() *config.Config { return jcfg },
			Names:   func() []string { return []string{"main"} },
			GroupID: func() string { return "g1" },
			Push: func(v any) error {
				mu.Lock()
				frames = append(frames, v)
				mu.Unlock()
				return nil
			},
		})

		// job op 已注册
		jo := &ops.Ops{
			Runner: mkRunner("/bin/true"),
			Paths:  msm.NewPaths(tmp),
			Names:  func() []string { return []string{"main"} },
			Jobs:   mgr,
		}
		st := jo.Handle("job_status", "", map[string]any{"jobId": float64(999)})
		_, missingJobs := st["error"].(string)
		stAll := jo.Handle("start_all", "", nil)
		out["job_ops_registered"] = st["ok"] == false && missingJobs && strings.Contains(fmt.Sprint(st["error"]), "任务不存在") &&
			stAll["ok"] == true && stAll["results"] != nil

		// 危险操作二次确认:game_update 必须 confirm 精确等于 UPDATE
		_, errNoConfirm := mgr.Start(job.Spec{Kind: job.KindGameUpdate})
		_, errBadCase := mgr.Start(job.Spec{Kind: job.KindGameUpdate, Confirm: "update"})
		out["job_confirm_required"] = errNoConfirm != nil && errBadCase != nil

		// 建/删实例 —— 二次确认(confirm 必须精确等于实例名)、唯一实例拒删、非法名拒建
		_, errDelNoConfirm := mgr.Start(job.Spec{Kind: job.KindInstanceDelete, Params: map[string]any{"name": "main"}})
		_, errDelWrongConfirm := mgr.Start(job.Spec{Kind: job.KindInstanceDelete, Params: map[string]any{"name": "ghost", "confirm": "main"}})
		_, errDelLast := mgr.Start(job.Spec{Kind: job.KindInstanceDelete, Params: map[string]any{"name": "main", "confirm": "main"}})
		_, errCreateBadName := mgr.Start(job.Spec{Kind: job.KindInstanceCreate, Params: map[string]any{"name": "bad name!"}})
		out["provision_guards"] = errDelNoConfirm != nil && errDelWrongConfirm != nil && errDelLast != nil && errCreateBadName != nil

		// 编号注册表落盘 —— 取号(不回收)/ 删除 / 原子写 + 读回一致
		regFile := filepath.Join(tmp, "registry.json")
		rf, firstInst := registry.Append(registry.File{}, "main", 27015, 27115)
		rf, secondInst := registry.Append(rf, "arena1", 27019, 27219)
		rf = registry.Remove(rf, "main")
		rf, thirdInst := registry.Append(rf, "arena2", 27020, 27220)
		saveRegErr := registry.Save(regFile, rf)
		back := registry.Load(regFile)
		out["registry_persist"] = saveRegErr == nil && firstInst.Idx == 1 && secondInst.Idx == 2 && thirdInst.Idx == 3 &&
			len(back.Items) == 2 && back.MaxIdx == 3 && back.Items[1].Name == "arena2"

		// 任务生命周期:起 → 跑完 → 日志落盘 + 索引落盘 + ActiveGroups 清空
		var jid int64
		started := false
		if j, err := mgr.Start(job.Spec{Kind: job.KindHostCleanup, Params: map[string]any{
			"patterns": []any{"backup:old"}, "dryRun": true,
		}}); err == nil {
			jid = j.ID
			started = true
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if cur := mgr.Get(jid); cur != nil && cur.Status == protocol.JobDone {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		cur := mgr.Get(jid)
		out["job_runs_and_logs"] = started && cur != nil && cur.Status == protocol.JobDone &&
			cur.Progress == 100 && mgr.LogSize(jid) > 0 && len(mgr.ActiveGroups()) == 0 &&
			exists(filepath.Join(jdir, "index.json"))

		// job_report 收敛:终态任务上报一次(帧形状对齐 job_report),已上报不重复
		mu.Lock()
		frames = nil
		mu.Unlock()
		sent := mgr.ReportPending()
		mu.Lock()
		rows := make([]protocol.JobReport, 0, len(frames))
		for _, f := range frames {
			if r, ok := f.(protocol.JobReport); ok {
				rows = append(rows, r)
			}
		}
		mu.Unlock()
		again := mgr.ReportPending()
		shapeOK := len(rows) == 1 && rows[0].Type == protocol.TypeJobReport &&
			rows[0].Job.JobID == jid && rows[0].Job.Kind == job.KindHostCleanup &&
			rows[0].Job.GroupID == "g1" && rows[0].Job.Origin == job.OriginCLI && rows[0].Job.Status == protocol.JobDone
		out["job_report_converges"] = started && sent == 1 && shapeOK && again == 0

		// bridge:old(桥自身目录冗余件清理,2026-09-22):备份每类只留最新一份、jobs/*.log 保留最新 20 份、
		// __pycache__ 删;index.json / *.cancel / *.lock 绝不动。这里在临时 <config_dir> 上做真跑一遍
		// (细节与边界见 internal/job 的 TestHostCleanupBridgeOld*)
		{
			now := time.Now()
			mk := func(name string, mod time.Time) string {
				p := filepath.Join(tmp, name)
				_ = os.WriteFile(p, []byte("x"), 0o644)
				_ = os.Chtimes(p, mod, mod)
				return p
			}
			bakOld := mk("cs.bak-20260921140834", now.Add(-2*time.Hour))
			bakNew := mk("cs.bak-20260922112837", now.Add(-time.Hour))
			pyOld := mk("bridge.py.bak-20260918160405", now.Add(-2*time.Hour))
			pyNew := mk("bridge.py.bak-20260920204822", now.Add(-time.Hour))
			pycache := filepath.Join(tmp, "__pycache__")
			_ = os.MkdirAll(pycache, 0o755)
			var jid2 int64
			started2 := false
			if j, err := mgr.Start(job.Spec{Kind: job.KindHostCleanup, Params: map[string]any{
				"patterns": []any{"bridge:old"}, "dryRun": false, "confirm": "CLEAN",
			}}); err == nil {
				jid2 = j.ID
				started2 = true
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if cur := mgr.Get(jid2); cur != nil && cur.Status == protocol.JobDone {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			cur2 := mgr.Get(jid2)
			out["cleanup_bridge_old"] = started2 && cur2 != nil && cur2.Status == protocol.JobDone &&
				!exists(bakOld) && !exists(pyOld) && !exists(pycache) &&
				exists(bakNew) && exists(pyNew) && exists(filepath.Join(jdir, "index.json"))
		}

		// 任务日志增量:半行既不返回、offset 也不推进(与 log op 同款语义)
		half := filepath.Join(jdir, "half.log")
		_ = os.WriteFile(half, []byte("完整行\n半行"), 0o644)
		lines, next, errHalf := fsx.ReadNewLines(half, 0)
		out["job_log_half_line"] = errHalf == nil && len(lines) == 1 && next < int64(len("完整行\n半行"))

		// 守护重启:上次留下的 running 任务标记 failed(不谎报"还在跑")
		raw, _ := json.Marshal(map[string]any{"nextId": 42, "jobs": []map[string]any{
			{"id": 41, "kind": job.KindGameUpdate, "groupId": "g1", "status": protocol.JobRunning, "startedAt": 1},
		}})
		_ = os.WriteFile(filepath.Join(jdir, "index.json"), raw, 0o644)
		m2 := job.New(job.Options{Dir: jdir, Cfg: func() *config.Config { return jcfg }})
		interrupted := m2.Get(41)
		out["job_restart_marks_failed"] = interrupted != nil && interrupted.Status == protocol.JobFailed &&
			strings.Contains(interrupted.Error, "重启")
	}

	// ---- install(新服务器引导:M5 收口)------------------------------------------
	// 内嵌的 vendored msm 投放、补丁在位判定、config.yaml 生成、unit 渲染都在临时目录离线验证;
	// 真机引导流程请按 README.md 准备主机依赖与游戏文件。
	{
		idir := filepath.Join(tmp, "install")
		msmDir := filepath.Join(idir, "cs2-multiserver-new")
		deployed := false
		if _, err := install.DeployMSM(install.DeployOptions{Target: msmDir}); err == nil {
			fi, err := os.Stat(filepath.Join(msmDir, "msm"))
			link, lerr := os.Lstat(filepath.Join(msmDir, "cs2-server"))
			deployed = err == nil && fi.Mode().Perm()&0o100 != 0 && lerr == nil && link.Mode()&os.ModeSymlink != 0 &&
				exists(filepath.Join(msmDir, "cs2", "app", "functions", "instance.sh"))
		}
		out["install_deploy_embedded"] = deployed

		// 本地补丁在位判定:在位 → true;把 gotv.conf 还原成上游 PORT+5 → 如实报缺失(随后还原)
		okPatch, _ := install.VerifyLocalPatches(msmDir)
		gotv := filepath.Join(msmDir, "cs2", "app", "cfg", "gotv.conf")
		if raw, err := os.ReadFile(gotv); err == nil {
			_ = os.WriteFile(gotv, []byte(strings.Replace(string(raw), "PORT + 100", "PORT + 5", 1)), 0o644)
			badPatch, note := install.VerifyLocalPatches(msmDir)
			out["install_local_patch_check"] = okPatch && !badPatch && strings.Contains(note, "GOTV")
			_ = os.WriteFile(gotv, raw, 0o644) // 还原:后面的体检项依赖"补丁在位"
		}

		// 幂等:二次投放(无 --force)不动目标目录
		marker := filepath.Join(msmDir, "SELFTEST-MARKER")
		_ = os.WriteFile(marker, []byte("x"), 0o644)
		_, errAgain := install.DeployMSM(install.DeployOptions{Target: msmDir})
		out["install_deploy_idempotent"] = errAgain == nil && exists(marker)

		// config.yaml 生成 + unit 渲染(ExecStart 指向本二进制与配置路径,不再含 %h 默认值)
		icfg := filepath.Join(idir, "config.yaml")
		iunit := filepath.Join(idir, "systemd", "cs-agent.service")
		cfgOK := install.WriteConfig(icfg, install.ConfigSpec{Token: "tok", BackendWS: "ws://x/api/agent", MSMDir: msmDir}) == nil
		if _, _, err := config.Load(icfg); err != nil {
			cfgOK = false
		}
		unitOK := false
		if _, err := install.WriteUnitFile(iunit, icfg); err == nil {
			if raw, err := os.ReadFile(iunit); err == nil {
				exe, _ := os.Executable()
				absCfg, _ := filepath.Abs(icfg)
				// ExecStart 必须指向本二进制与配置路径;模板里的注释行(如 StandardOutput 示例)不算
				noDefault := true
				for _, ln := range strings.Split(string(raw), "\n") {
					if !strings.HasPrefix(strings.TrimSpace(ln), "#") && strings.Contains(ln, "%h/data/bridge/cs") {
						noDefault = false
					}
				}
				unitOK = strings.Contains(string(raw), "ExecStart="+exe+" agent --config "+absCfg) && noDefault
			}
		}
		out["install_config_generated"] = cfgOK
		out["install_unit_rendered"] = unitOK

		// 体检分层:L2(msm)应全 ✓,config 项应 ✓(并给出路径)
		items := install.Check(install.Options{
			MSMDir: msmDir, MSMExe: filepath.Join(msmDir, "cs2-server"),
			ConfigPath: icfg, UnitPath: iunit, Home: idir,
		})
		l2ok, cfgNote := true, ""
		for _, it := range items {
			if it.Layer == "L2" && !it.OK {
				l2ok = false
			}
			if it.Name == "config.yaml 可解析" {
				cfgNote = it.Note
			}
		}
		out["install_check_layers"] = l2ok && strings.Contains(cfgNote, icfg)
	}

	// ---- install --arena-patches(主机侧补丁 L4 的幂等重做)-------------------------
	// 夹具:base + match1 + match3 保留用户插件，验证配置修复、一次执行补齐、
	// 二次执行零改动、以及**作用域**(只有增强人机实例吃 cfg/gameinfo 补丁)。
	{
		adir := filepath.Join(tmp, "arena-patches")
		amsm := filepath.Join(adir, "cs2-multiserver-new")
		ahome := filepath.Join(adir, "home")
		gameFor := func(site string) string {
			return filepath.Join(amsm, "..", "msm.d", "cs2", site, "game", "csgo")
		}
		writeAt := func(p, body string) {
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.WriteFile(p, []byte(body), 0o644)
		}
		setupSite := func(site string) {
			g := gameFor(site)
			writeAt(filepath.Join(g, "addons", "counterstrikesharp", "plugins", "MatchZy", "MatchZy.dll"), "official dll")
			ad := filepath.Join(g, "addons", "counterstrikesharp", "plugins", "ArenaDuel")
			writeAt(filepath.Join(ad, "ArenaDuel.dll"), "old")
			writeAt(filepath.Join(ad, "ArenaDuel.deps.json"), "old")
			writeAt(filepath.Join(g, "gameinfo.gi"), "\t\t\tGame_LowViolence\tcsgo_lv\n\t\t\tGame\tcsgo\n")
			writeAt(filepath.Join(g, "cfg", "gamemode_competitive.cfg"), "bot_quota\t1\nbot_quota_mode\tcompetitive\n")
			writeAt(filepath.Join(g, "cfg", "gamemode_competitive_offline.cfg"), "bot_quota\t10\nbot_quota_mode\tfill\n")
			for _, f := range []string{"warmup.cfg", "live_override.cfg", "live_wingman_override.cfg"} {
				writeAt(filepath.Join(g, "cfg", "MatchZy", f), "mp_maxrounds 24\n")
			}
		}
		for _, site := range []string{"base", "inst-match1", "inst-match3"} {
			setupSite(site)
		}
		writeAt(filepath.Join(gameFor("inst-match3"), "cfg", "my_bot_normal_config.cfg"), "sv_cheats 1\n")

		insts := []string{"match1", "match3"}
		items, err := install.ApplyArenaPatches(install.ArenaPatchOptions{
			MSMDir: amsm, Instances: insts, Home: ahome,
		})
		rawDLL, _ := os.ReadFile(filepath.Join(gameFor("base"), "addons", "counterstrikesharp", "plugins", "MatchZy", "MatchZy.dll"))
		rawWarm, _ := os.ReadFile(filepath.Join(gameFor("inst-match3"), "cfg", "MatchZy", "warmup.cfg"))
		rawInfo3, _ := os.ReadFile(filepath.Join(gameFor("inst-match3"), "gameinfo.gi"))
		rawInfo1, _ := os.ReadFile(filepath.Join(gameFor("inst-match1"), "gameinfo.gi"))
		out["install_arena_patches_repair"] = err == nil && install.Changed(items) > 0 &&
			string(rawDLL) == "official dll" &&
			strings.Contains(string(rawWarm), "exec arena_bots.cfg") &&
			strings.Contains(string(rawInfo3), "overrides/botprofile.vpk") &&
			!strings.Contains(string(rawInfo1), "botprofile")

		items2, err2 := install.ApplyArenaPatches(install.ArenaPatchOptions{
			MSMDir: amsm, Instances: insts, Home: ahome,
		})
		out["install_arena_patches_idempotent"] = err2 == nil && install.Changed(items2) == 0
	}

	return out
}

func hasKeys(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
