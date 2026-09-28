// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// Package msm 只做三件事:以 argv 方式执行 msm(无 shell)、解析实例状态、按 msm_dir 推导路径模板。
//
// 桥不重写 msm:游戏安装/更新/校验与 tmux 监督仍是 msm 的职责(决策 1.2)。
package msm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrBusy:只读查询拿不到锁时的哨兵(不排队 —— 否则 500ms 一次的 tail 会与真正的控制命令互相拖慢)。
var ErrBusy = errors.New("msm busy(实例正被其他命令占用),请重试")

// TimeoutError:msm 子进程超时。
type TimeoutError struct{ Seconds int }

func (e *TimeoutError) Error() string { return fmt.Sprintf("msm timeout after %ds", e.Seconds) }

// Lock 是全局单锁:所有实例的 msm 调用串行化(跨实例,与 v1 一致)。
// 显式导出是为了自检能确定性地构造"锁忙"场景。
type Lock struct{ mu sync.Mutex }

func (l *Lock) Acquire()         { l.mu.Lock() }
func (l *Lock) Release()         { l.mu.Unlock() }
func (l *Lock) TryAcquire() bool { return l.mu.TryLock() }

// Result 是一次 msm 调用的输出。ok 恒由 Returncode 决定。
type Result struct {
	Stdout     string
	Stderr     string
	Returncode int
}

// Runner 执行 msm 命令并缓存实例状态。
type Runner struct {
	Cfg  func() RunnerConfig
	Lock *Lock

	cacheMu sync.Mutex
	cache   map[string]cacheEntry
}

// RunnerConfig 是 Runner 需要的配置切片(由 config.Store 提供,避免包间循环依赖)。
type RunnerConfig struct {
	MSM         string
	MSMDir      string
	TimeoutS    int
	StateCacheS float64
}

type cacheEntry struct {
	state string
	at    time.Time
}

func NewRunner(cfg func() RunnerConfig) *Runner {
	return &Runner{Cfg: cfg, Lock: &Lock{}, cache: map[string]cacheEntry{}}
}

// Run 执行 `msm @<instance> <op> [extra...]`,cwd=msm_dir,env=父环境 + 覆盖。
// wait=false:拿不到锁立即返回 ErrBusy(只读查询路径)。
func (r *Runner) Run(instance, op string, extra []string, env map[string]string, wait bool) (*Result, error) {
	cfg := r.Cfg()

	if wait {
		r.Lock.Acquire()
	} else if !r.Lock.TryAcquire() {
		return nil, ErrBusy
	}
	defer r.Lock.Release()

	args := append([]string{"@" + instance, op}, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutS)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, cfg.MSM, args...)
	cmd.Dir = cfg.MSMDir
	if cmd.Dir == "" {
		cmd.Dir = "."
	}
	// 父环境 + 覆盖:ARENA_STUB_DIR / STUB_BOOT_S 等必须传到 msm 子进程
	if len(env) > 0 {
		merged := os.Environ()
		for k, v := range env {
			merged = append(merged, k+"="+v)
		}
		cmd.Env = merged
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, &TimeoutError{Seconds: cfg.TimeoutS}
	}
	res := &Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Returncode = ee.ExitCode() // 非 0 退出是正常结果,不是 Go 层的错误
			return res, nil
		}
		return nil, err // 起不来(可执行不存在/权限):交给上层收口成 ok:false
	}
	return res, nil
}

// StreamOpts 是 RunStream 的选项。
type StreamOpts struct {
	// Env 覆盖环境变量(父环境 + 覆盖)。
	Env map[string]string
	// TimeoutS 单步超时(<=0 用配置超时;msm update/validate 给更长的值)。
	TimeoutS int
	// Stdin 需要喂给 msm 的输入(msm update 是交互式:游戏本体 Y、SwiftlyS2 n)。
	Stdin string
	// OnLine 逐行回调(可为 nil)。
	OnLine func(string)
}

// RunStream 供 job 框架使用:与 Run 相同的 argv/cwd/env 规则,但
//
//   - 接受外部 ctx(任务取消):取消时 kill **整个进程组**(steamcmd/tmux 子进程一起,
//     强制取消整个进程组，而不是只杀直接子进程;
//   - 子进程放进独立进程组(Setpgid)—— 只对本方法生效,普通 op 路径的行为不变;
//   - stdout/stderr 逐行回调 OnLine(供任务日志流式落盘 + push);
//   - instance 为空表示主机级 msm 命令(如 update/validate),argv 不带 @实例。
func (r *Runner) RunStream(ctx context.Context, instance, op string, extra []string, o StreamOpts) (*Result, error) {
	cfg := r.Cfg()
	timeoutS := o.TimeoutS
	if timeoutS <= 0 {
		timeoutS = cfg.TimeoutS
	}

	r.Lock.Acquire()
	defer r.Lock.Release()

	args := extra
	if instance != "" {
		args = append([]string{"@" + instance, op}, extra...)
	} else {
		args = append([]string{op}, extra...)
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutS)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, cfg.MSM, args...)
	cmd.Dir = cfg.MSMDir
	if cmd.Dir == "" {
		cmd.Dir = "."
	}
	if len(o.Env) > 0 {
		merged := os.Environ()
		for k, v := range o.Env {
			merged = append(merged, k+"="+v)
		}
		cmd.Env = merged
	}
	if o.Stdin != "" {
		cmd.Stdin = strings.NewReader(o.Stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// 负号 = 整个进程组(steamcmd 会再 fork)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	sink := &lineSink{emit: o.OnLine, maxKeep: 256 * 1024}
	cmd.Stdout = sink
	cmd.Stderr = sink

	err := cmd.Run()
	sink.Flush()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, &TimeoutError{Seconds: timeoutS}
	}
	res := &Result{Stdout: sink.Stdout(), Stderr: sink.Stderr()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Returncode = ee.ExitCode()
			return res, nil
		}
		if ctx.Err() != nil || cctx.Err() != nil {
			// 被取消(Job 取消路径):由调用方判定 cancelled
			res.Returncode = -1
			return res, ctx.Err()
		}
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			// msm start/update 可能拉起继承 stdout/stderr 的后台进程。主命令已成功退出时,
			// WaitDelay 只表示输出管道仍被占用,不能把已完成的更新误判为失败。
			if o.OnLine != nil {
				o.OnLine("[cs] msm 退出码 0;输出管道未在 5 秒内关闭,已停止等待后续输出")
			}
			return res, nil
		}
		return nil, err
	}
	return res, nil
}

// lineSink 是按行切分的输出收集器(exec 会从 stdout/stderr 两个 goroutine 并发写)。
// 保留前 maxKeep 字节供 Result 使用,超出部分只流式回调、不囤内存(msm update 的输出可达数 MB)。
type lineSink struct {
	mu      sync.Mutex
	buf     string
	out     strings.Builder
	outLen  int
	errLen  int
	phase   int // 0=stdout 1=stderr(仅用于分桶,不精确)
	emit    func(string)
	maxKeep int
}

func (w *lineSink) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf += string(p)
	for {
		i := strings.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(w.buf[:i], "\r")
		w.buf = w.buf[i+1:]
		w.keep(line)
		if w.emit != nil {
			w.emit(line)
		}
	}
	return len(p), nil
}

// keep 把一行计入结果文本(受 maxKeep 限制)。
func (w *lineSink) keep(line string) {
	if w.outLen >= w.maxKeep {
		return
	}
	n, _ := w.out.WriteString(line + "\n")
	w.outLen += n
}

// Flush 收尾:半行(结尾没有 \n 的残留)也作为一行回调(与"只推进到完整行"的日志增量语义不同 ——
// 这里是一次性命令的输出,丢半行会丢信息)。
func (w *lineSink) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf != "" {
		line := strings.TrimRight(w.buf, "\r")
		w.buf = ""
		w.keep(line)
		if w.emit != nil {
			w.emit(line)
		}
	}
}

func (w *lineSink) Stdout() string { return w.out.String() }
func (w *lineSink) Stderr() string { return "" }

// ParseState 把 msm status 的输出映射成状态串(大小写不敏感,只解析 stdout)。
//
// 与 v1 的**唯一有意差异**(2026-09-20 用户批准修复):判断顺序把 "not running" 提到 RUNNING 之前,
// 修掉 v1「停机实例被误判成 RUNNING」的缺陷;生产 msm 打印的是 STOPPED,故迁移期两版实际输出一致。
func ParseState(text string) string {
	up := strings.ToUpper(text)
	switch {
	case strings.Contains(up, "BOOTING"):
		return "BOOTING"
	// "not running" 必须排在 RUNNING 之前 —— v1 排在后面,导致停机实例被判成 RUNNING
	// (2026-09-20 用户批准修复;唯一有意差异,见 README.md)
	case strings.Contains(up, "NOT RUNNING"):
		return "STOPPED"
	case strings.Contains(up, "RUNNING"):
		return "RUNNING"
	case strings.Contains(up, "STOPPED"):
		return "STOPPED"
	default:
		return "UNKNOWN"
	}
}

// NoteState 写状态缓存:ERROR 不写、其余(含 UNKNOWN)写。
func (r *Runner) NoteState(instance, state string) {
	if state == "" || state == "ERROR" {
		return
	}
	r.cacheMu.Lock()
	r.cache[instance] = cacheEntry{state: state, at: time.Now()}
	r.cacheMu.Unlock()
}

func (r *Runner) cached(instance string, ignoreTTL bool) (string, bool) {
	r.cacheMu.Lock()
	e, ok := r.cache[instance]
	r.cacheMu.Unlock()
	if !ok {
		return "", false
	}
	if ignoreTTL || time.Since(e.at) <= time.Duration(r.Cfg().StateCacheS*float64(time.Second)) {
		return e.state, true
	}
	return "", false
}

// Status 查询实例状态。
//   - useCache=true(health / 控制台 tail / 订阅):TTL 内缓存优先;未命中且锁忙 → 回退过期缓存 → "ERROR";
//   - useCache=false(显式 status op):始终真查,异常一律 "ERROR",成功写缓存。
func (r *Runner) Status(instance string, useCache bool) string {
	if useCache {
		if st, ok := r.cached(instance, false); ok {
			return st
		}
	}
	res, err := r.Run(instance, "status", nil, nil, !useCache)
	if err != nil {
		if useCache {
			if st, ok := r.cached(instance, true); ok {
				return st
			}
		}
		return "ERROR"
	}
	st := ParseState(res.Stdout)
	r.NoteState(instance, st)
	return st
}

var (
	envKeyRe   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)
	envValueRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,32}$`)
)

// MaxEnvKeys 启动项覆盖的键数上限。
const MaxEnvKeys = 8

// SanitizeEnv 校验 start/restart 的启动项覆盖环境变量。
//
// 值会被 msm 写进生成的 server-start.sh(启动命令行),因此严禁空格/引号/分号/`$` 等 shell 元字符。
// 返回 (env, nil) 或 (nil, 错误文案) —— 文案与 v1 逐字一致(后端 smoke 断言过)。
func SanitizeEnv(env map[string]any) (map[string]string, string) {
	if env == nil {
		return map[string]string{}, ""
	}
	if len(env) > MaxEnvKeys {
		return nil, fmt.Sprintf("env must be an object with at most %d keys", MaxEnvKeys)
	}
	out := map[string]string{}
	for k, v := range env {
		if !envKeyRe.MatchString(k) {
			return nil, "bad env key: " + k
		}
		sv := ScalarString(v)
		if !envValueRe.MatchString(sv) {
			return nil, fmt.Sprintf("bad env value for %s (allowed: A-Za-z0-9_.- up to 32 chars)", k)
		}
		out[k] = sv
	}
	return out, ""
}

// ScalarString 复刻 Python 的 str(v):JSON 数值 16 → "16"(不是 "16.000000")。
// 供 env 校验与 keepMatchIds/matchId 这类"任意标量转字符串"的场景共用。
func ScalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return fmt.Sprint(v)
	}
}
