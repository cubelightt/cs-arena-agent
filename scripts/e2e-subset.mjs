// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 cubelightt

// reverse 后端与 Go 桥的端到端检查。
// 运行: node scripts/e2e-subset.mjs [--backend <平台 backend 路径>] [--bin <cs 路径>]
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync, readFileSync, existsSync, mkdirSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const V2 = path.join(__dirname, '..')
const backendArgIdx = process.argv.indexOf('--backend')
const BACKEND = path.resolve(backendArgIdx > 0 ? process.argv[backendArgIdx + 1] : (process.env.CS_ARENA_BACKEND_DIR || path.join(V2, '..', 'cs-arena', 'backend')))
const backendServer = path.join(BACKEND, 'server.js')
const PORT = Number(process.env.E2E_PORT || 8097)
const BASE = `http://127.0.0.1:${PORT}`
const ADMIN = '76561199000000000'

const argIdx = process.argv.indexOf('--bin')
const CS_BIN = argIdx > 0 ? path.resolve(process.argv[argIdx + 1]) : path.join(V2, 'cs')

let pass = 0
let fail = 0
function check(name, cond, extra = '') {
  if (cond) {
    pass++
    console.log(`  PASS  ${name}`)
  } else {
    fail++
    console.log(`  FAIL  ${name} ${extra}`)
  }
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

let cookie = ''
async function req(method, url, body) {
  const res = await fetch(`${BASE}${url}`, {
    method,
    headers: { 'content-type': 'application/json', ...(cookie ? { cookie } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  })
  const text = await res.text()
  const setCookie = res.headers.get('set-cookie')
  if (setCookie) cookie = setCookie.split(';')[0]
  return { status: res.status, data: text ? JSON.parse(text) : null }
}

async function main() {
  if (!existsSync(backendServer)) {
    console.error(`[e2e] 找不到平台后端: ${BACKEND}(请用 --backend 或 CS_ARENA_BACKEND_DIR 指定)` )
    process.exit(1)
  }
  if (!existsSync(CS_BIN)) {
    console.error(`[e2e] 找不到桥二进制: ${CS_BIN}(先跑 agent/v2/scripts/build.sh)`)
    process.exit(1)
  }
  const stub = mkdtempSync(path.join(tmpdir(), 'cs-e2e-'))
  const cfgPath = path.join(stub, 'config.yaml')

  // 造一份**真实形状的 msm 布局**:msm_dir 与其同级的 msm.d/cs2(端口真源/游戏版本/日志)
  // —— registry/host/disk/buildid 这些 M2/M3 代码路径只有在这种布局下才走真分支
  const layoutRoot = path.join(stub, 'host')
  const msmDir = path.join(layoutRoot, 'cs2-multiserver-new')
  const msmD = path.join(layoutRoot, 'msm.d', 'cs2')
  for (const inst of ['main', 'match1', 'match2', 'match3']) {
    const dir = path.join(msmD, 'cfg', `inst-${inst}`)
    mkdirSync(dir, { recursive: true })
    const port = 27015 + ['main', 'match1', 'match2', 'match3'].indexOf(inst)
    writeFileSync(path.join(dir, 'server.conf'), `__PORT__="27015"\nPORT="${port}"\n`)
  }
  mkdirSync(path.join(msmDir), { recursive: true })
  mkdirSync(path.join(msmD, 'base', 'steamapps'), { recursive: true })
  writeFileSync(
    path.join(msmD, 'base', 'steamapps', 'appmanifest_730.acf'),
    'AppState\n{\n\t"appid"\t\t"730"\n\t"SizeOnDisk"\t\t"71099106130"\n\t"buildid"\t\t"25218825"\n}\n',
  )
  mkdirSync(path.join(msmD, 'log', 'inst-main'), { recursive: true })
  writeFileSync(path.join(msmD, 'log', 'inst-main', '260920_io-server.log'), 'boot line\nsecond line\n')
  // v2 最小键集(故意带上一个弃用键,验证"警告并忽略"不影响运行)
  writeFileSync(
    cfgPath,
    [
      'mode: reverse            # 已弃用:只警告、不读取',
      'token: replace-this-before-use',
      `backend_ws: ws://127.0.0.1:${PORT}/api/agent`,
      `msm_dir: ${msmDir}`,
      `msm: ${path.join(BACKEND, 'scripts', 'msm-stub.sh')}`,
      `archive_dir: ${path.join(stub, 'arena-data')}`,
      // CI/开发机磁盘余量远小于生产:建实例/更新的磁盘前置检查阈值调小(生产留默认 15 GiB)
      'min_free_bytes: 1073741824',
      'console_tail_ms: 300',
      'health_push_ms: 500',
    ].join('\n') + '\n',
  )

  const server = spawn('node', ['server.js'], {
    cwd: BACKEND,
    env: {
      ...process.env,
      PORT: String(PORT),
      PUBLIC_BASE_URL: BASE,
      DB_PATH: path.join(stub, 'arena.db'),
      BRIDGE_MODE: 'reverse',
      ARENA_STUB_DIR: stub,
      STUB_BOOT_S: '1',
      AGENT_PING_MS: '300',
      AGENT_STALE_MS: '1500',
      ADMIN_STEAM_IDS: ADMIN,
      STEAM_PROXY: '',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  let serverLog = ''
  server.stdout.on('data', (d) => {
    serverLog += String(d)
    process.stdout.write(`[server] ${d}`)
  })
  server.stderr.on('data', (d) => process.stderr.write(`[server-err] ${d}`))

  let bridge = null
  let bridgeOut = ''
  const spawnBridge = () => {
    bridge = spawn(CS_BIN, ['agent', '--config', cfgPath], {
      cwd: V2,
      // STUB_CLONE_SLEEP_S:让平台建实例任务的 clone 步骤睡 3s —— 跨进程取消用例需要一个
      // 足够长的「任务运行中」窗口才能发起并观察到取消(只影响守护发起的 msm clone)
      env: { ...process.env, ARENA_STUB_DIR: stub, STUB_CLONE_SLEEP_S: '3' },
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    bridge.stdout.on('data', (d) => {
      bridgeOut += String(d)
      process.stdout.write(`[bridge] ${d}`)
    })
    bridge.stderr.on('data', (d) => {
      bridgeOut += String(d)
      process.stdout.write(`[bridge] ${d}`)
    })
    return bridge
  }
  // 已退出的进程也要能等到结果('close' 早于监听注册时,只等事件会永远挂住)
  const waitExit = (proc, ms = 5000) =>
    new Promise((resolve) => {
      if (proc.exitCode !== null) return resolve(proc.exitCode)
      const t = setTimeout(() => resolve('timeout'), ms)
      proc.once('close', (code) => {
        clearTimeout(t)
        resolve(code)
      })
    })

  try {
    let healthy = false
    for (let i = 0; i < 60; i++) {
      try {
        if ((await fetch(`${BASE}/api/health`)).ok) {
          healthy = true
          break
        }
      } catch {}
      await sleep(500)
    }
    check('backend up (reverse)', healthy)
    if (!healthy) throw new Error('backend did not start')

    spawnBridge()

    // 管理员登录(首登 set-password 流,与 smoke 同款)—— 实例接口都要鉴权,先登录再断言
    await req('POST', '/api/auth/login', { steamId: ADMIN, name: 'AdminTest', avatarUrl: '' })
    let r = await req('POST', '/api/auth/set-password', { steamId: ADMIN, password: 'admin123', name: 'AdminTest' })
    check('admin login (set-password)', r.status === 200 && r.data?.user?.isAdmin === true)

    // 等桥握手 + hello_ack 生效(health 推送 500ms 一次)
    let connected = false
    for (let i = 0; i < 40; i++) {
      const g = await req('GET', '/api/game-servers')
      if (g.data?.[0]?.connected === true) {
        connected = true
        break
      }
      await sleep(250)
    }
    check('bridge connected (hello + capability negotiation)', connected)

    // hello_ack 的白名单生效:4 个实例健康由桥推送(实例清单由 DB 种子,白名单由 ack 下发)
    let status = await req('GET', '/api/servers/status')
    const servers = status.data?.[0]?.servers || []
    check(
      'hello_ack whitelist: 4 instances reported STOPPED',
      servers.length === 4 && status.data?.[0]?.summary?.stopped === 4 && servers.every((s) => s.health === 'STOPPED'),
      JSON.stringify(status.data?.[0]?.summary) + ' ' + JSON.stringify(servers),
    )

    // hello_ack 落盘到 state.json(离线降级缓存)
    const statePath = path.join(stub, 'state.json')
    let stateJson = {}
    try {
      stateJson = JSON.parse(readFileSync(statePath, 'utf8'))
    } catch {}
    check(
      'hello_ack persisted to state.json (serverId + 4 instances)',
      stateJson.serverId === 'g1' && (stateJson.instances || []).length === 4,
      JSON.stringify(stateJson),
    )

    // 守护心跳(agent.status.json):CLI 的 cs status/cs doctor 依赖它判"守护在不在、后端连没连"
    const statusPath = path.join(stub, 'agent.status.json')
    let agentStatus = {}
    try {
      agentStatus = JSON.parse(readFileSync(statusPath, 'utf8'))
    } catch {}
    check(
      'agent heartbeat reports connected + instancesSource=backend',
      agentStatus.connected === true && agentStatus.instancesSource === 'backend' && agentStatus.instanceCount === 4,
      JSON.stringify(agentStatus),
    )

    // ---- 主机概况 API + 实例清单上报 ----
    const hostRes = await req('GET', '/api/host/status')
    const g0 = hostRes.data?.groups?.[0] || {}
    const instRows = g0.instances || []
    check(
      'host status: disk + game build + 4 instances with ports',
      hostRes.status === 200 && g0.ok === true && g0.connected === true &&
        g0.disk?.freeBytes > 0 && g0.game?.installed?.build?.length >= 6 &&
        instRows.length === 4 && instRows.every((x) => x.port >= 27015 && x.gotvPort === x.port + 100) &&
        g0.instancesSource === 'backend',
      JSON.stringify({ disk: g0.disk, build: g0.game?.installed?.build, n: instRows.length, src: g0.instancesSource }),
    )
    const oneGroup = await req('GET', '/api/host/status?groupId=g1')
    check('host status: groupId filter', oneGroup.status === 200 && oneGroup.data.groups.length === 1 && oneGroup.data.groups[0].groupId === 'g1')
    const noAuth = await (async () => {
      const saved = cookie
      cookie = ''
      const r2 = await req('GET', '/api/host/status')
      cookie = saved
      return r2
    })()
    check('host status requires admin (401 anonymous)', noAuth.status === 401)
    check(
      'instances_report reconcile received by backend',
      serverLog.includes('instances_report(origin=reconcile)'),
      serverLog.split('\n').filter((l) => l.includes('instances_report')).slice(-2).join(' | '),
    )

    // start → RUNNING
    r = await req('POST', '/api/instances/main/start', {})
    check('start main via bridge', r.status === 200, JSON.stringify(r.data))
    let insts = (await req('GET', '/api/instances')).data || []
    check('main RUNNING after start', insts.find((i) => i.name === 'main')?.health === 'RUNNING', JSON.stringify(insts))

    // stop → STOPPED
    r = await req('POST', '/api/instances/main/stop', {})
    insts = (await req('GET', '/api/instances')).data || []
    check('stop main via bridge', r.status === 200 && insts.find((i) => i.name === 'main')?.health === 'STOPPED', JSON.stringify(insts))

    // restart → RUNNING(PUT /api/instances/:name 之前的 /:name/:op 路由)
    r = await req('POST', '/api/instances/main/restart', {})
    insts = (await req('GET', '/api/instances')).data || []
    check('restart main via bridge', r.status === 200 && insts.find((i) => i.name === 'main')?.health === 'RUNNING', JSON.stringify(insts))

    // argv 形状证据:stub msm 把 start/stop/restart 记进 <stub>/main.log
    const log = existsSync(path.join(stub, 'main.log')) ? readFileSync(path.join(stub, 'main.log'), 'utf8') : ''
    check(
      'msm argv shape (start/stop/restart recorded by stub)',
      log.includes('START') && log.includes('STOP') && log.includes('RESTART'),
      log.split('\n').slice(-4).join(' | '),
    )

    // 非白名单实例:桥必须拒绝(实例白名单唯一执行点在桥侧 + 后端)
    const adminConsole = await req('POST', '/api/instances/nope/start', {})
    check('unknown instance rejected by backend', adminConsole.status === 404 || adminConsole.status === 502, `status=${adminConsole.status}`)

    // ---- M4 任务框架:① 平台触发(202 → 进度/日志推送 → 终态 → 维护自动清除)----
    const up = await req('POST', '/api/host/game-update', { groupId: 'g1', confirm: 'UPDATE' })
    check(
      'platform triggers game_update via v2 bridge (202 + maintenance)',
      up.status === 202 && ['queued', 'running'].includes(up.data.job?.status) && up.data.maintenance?.enabled === true,
      JSON.stringify(up.data).slice(0, 240),
    )
    const jobId = up.data?.job?.id
    // 维护守卫(确定性断言:手动维护同样走这一条判定,任务终态会自动清掉它)
    await req('POST', '/api/host/maintenance', { groupId: 'g1', enabled: true, reason: 'e2e' })
    const blockedStart = await req('POST', '/api/instances/main/start', {})
    check('maintained group blocks instance start (409)', blockedStart.status === 409, JSON.stringify(blockedStart.data))
    // 手动维护只由管理员关闭(任务终态只清"任务自己开的"维护)
    const offMaint = await req('POST', '/api/host/maintenance', { groupId: 'g1', enabled: false })
    check('manual maintenance cleared by admin', offMaint.data?.maintenance?.enabled === false)
    let jstatus = null
    for (let i = 0; i < 80; i++) {
      await sleep(500)
      const cur = await req('GET', `/api/jobs/${jobId}`)
      jstatus = cur.data?.job?.status
      if (['done', 'failed', 'cancelled'].includes(jstatus)) break
    }
    // CI 环境没有真实 msm 布局:msm-stub 对 update 回非 0 → 任务 failed 属预期;
    // 走到**终态**本身就证明「op → 步骤机 → 流式日志 → push → jobs 行 → 维护」全链路通
    check('platform job reaches terminal state', ['done', 'failed', 'cancelled'].includes(jstatus), `status=${jstatus}`)
    const detail = await req('GET', `/api/jobs/${jobId}?lines=200`)
    check(
      'job log streamed from bridge (job_log)',
      Array.isArray(detail.data?.lines) && detail.data.lines.length > 0,
      JSON.stringify(detail.data).slice(0, 200),
    )
    const healthAfterJob = await req('GET', '/api/health')
    check('maintenance auto-cleared on job terminal', !(healthAfterJob.data.maintenance || []).some((x) => x.groupId === 'g1'))

    // ---- M4 任务框架:② CLI 触发(cs update --yes)经 job_report 收敛到平台 --------
    // (update 会先比对 buildid,恰好最新时不会起任务 —— 这里用 plugins sync 验证 CLI→job→job_report 链路)
    const cliExit = await new Promise((resolve) => {
      const proc = spawn(CS_BIN, ['--config', cfgPath, 'plugins', 'sync', '--from', 'main', '--to', 'match1'], {
        cwd: V2,
        env: { ...process.env, ARENA_STUB_DIR: stub },
        stdio: ['ignore', 'pipe', 'pipe'],
      })
      let out = ''
      proc.stdout.on('data', (d) => (out += String(d)))
      proc.stderr.on('data', (d) => (out += String(d)))
      const t = setTimeout(() => proc.kill('SIGTERM'), 60000)
      proc.once('close', (code) => {
        clearTimeout(t)
        resolve({ code, out })
      })
    })
    let cliJob = null
    for (let i = 0; i < 30; i++) {
      const list = await req('GET', '/api/jobs?limit=20')
      cliJob = (list.data?.jobs || []).find((j) => j.origin === 'cli' && j.kind === 'plugin_sync')
      if (cliJob && ['done', 'failed', 'cancelled'].includes(cliJob.status)) break
      await sleep(500)
    }
    check(
      'cli-originated job converges to platform (job_report)',
      !!cliJob && ['done', 'failed', 'cancelled'].includes(cliJob.status) && cliExit.code !== null,
      JSON.stringify({ cli: cliJob, exit: cliExit.code, out: cliExit.out.slice(-160) }),
    )
    const healthAfterCli = await req('GET', '/api/health')
    check('maintenance cleared after cli job terminal', !(healthAfterCli.data.maintenance || []).some((x) => x.groupId === 'g1'))

    // ---- 建/删实例闭环(面板路径 = job_start → 桥步骤机 → 注册表 + 平台收敛)------
    {
      const created = await req('POST', '/api/instances', { name: 'arena1', gameServerId: 'g1' })
      check(
        'create instance via bridge (201 + instance_create task)',
        created.status === 201 && created.data.task?.kind === 'instance_create' && created.data.instance?.provisionState === 'creating',
        JSON.stringify(created.data).slice(0, 300),
      )
      let st = null
      let jobErr = null
      let jobLog = ''
      for (let i = 0; i < 40; i++) {
        await sleep(300)
        const cur = await req('GET', `/api/jobs/${created.data.task?.id}?refresh=1`)
        st = cur.data?.job?.status
        jobErr = cur.data?.job?.error || null
        if (Array.isArray(cur.data?.lines)) jobLog = cur.data.lines.slice(-4).join(' | ')
        if (['done', 'failed', 'cancelled'].includes(st)) break
      }
      const arena1 = (await req('GET', '/api/instances')).data.find((x) => x.name === 'arena1')
      check(
        'create job done + instance converged (port/idx from bridge)',
        st === 'done' && arena1?.port === 27019 && arena1?.idx === 5 && arena1?.provisionState === null,
        `st=${st} inst=${JSON.stringify(arena1)} err=${jobErr} log=${jobLog}`,
      )
      const regFile = path.join(stub, 'registry.json')
      const reg = existsSync(regFile) ? JSON.parse(readFileSync(regFile, 'utf8')) : null
      check(
        'bridge registry.json persisted (numbering survives)',
        !!reg && Array.isArray(reg.items) && reg.items.some((i) => i.name === 'arena1' && i.idx === 5 && i.port === 27019),
        JSON.stringify(reg).slice(0, 240),
      )
      check(
        'msm layout created (cfg/inst-arena1 + inst-arena1)',
        existsSync(path.join(msmD, 'cfg', 'inst-arena1', 'server.conf')) && existsSync(path.join(msmD, 'inst-arena1')),
      )
      // 删除(二次确认 = 实例名;桥真删目录 → 平台删行)
      const del = await req('DELETE', '/api/instances/arena1', { confirm: 'arena1' })
      check('delete instance via bridge (ok + instance_delete task)', del.status === 200 && del.data.task?.kind === 'instance_delete', JSON.stringify(del.data).slice(0, 240))
      let dst = null
      for (let i = 0; i < 40; i++) {
        await sleep(300)
        const cur = await req('GET', `/api/jobs/${del.data.task?.id}?refresh=1`)
        dst = cur.data?.job?.status
        if (['done', 'failed', 'cancelled'].includes(dst)) break
      }
      const after = (await req('GET', '/api/instances')).data
      check(
        'delete job done + row removed + dirs gone',
        dst === 'done' && !after.some((x) => x.name === 'arena1') &&
          !existsSync(path.join(msmD, 'inst-arena1')) && !existsSync(path.join(msmD, 'cfg', 'inst-arena1')),
        `st=${dst} rows=${after.map((x) => x.name).join(',')}`,
      )
      const reg2 = existsSync(regFile) ? JSON.parse(readFileSync(regFile, 'utf8')) : null
      check(
        'registry tombstone recorded (no number reuse)',
        !!reg2 && !(reg2.items || []).some((i) => i.name === 'arena1') && (reg2.removed || []).includes('arena1') && reg2.maxIdx === 5,
        JSON.stringify(reg2).slice(0, 240),
      )
    }

    // ---- 建删实例闭环(CLI 路径 = cs new / cs del → job_report 收敛 + 实例名/结果上报)----
    // 覆盖 job_report 帧的 instance/result 字段与 CLI 建删全链路。
    {
      const runCli = (args) =>
        new Promise((resolve) => {
          const proc = spawn(CS_BIN, ['--config', cfgPath, ...args], {
            cwd: V2,
            env: { ...process.env, ARENA_STUB_DIR: stub },
            stdio: ['ignore', 'pipe', 'pipe'],
          })
          let out = ''
          proc.stdout.on('data', (d) => (out += String(d)))
          proc.stderr.on('data', (d) => (out += String(d)))
          const t = setTimeout(() => proc.kill('SIGTERM'), 90000)
          proc.once('close', (code) => {
            clearTimeout(t)
            resolve({ code, out })
          })
        })
      const findCliJob = async (kind, name) => {
        for (let i = 0; i < 40; i++) {
          const list = await req('GET', '/api/jobs?limit=20')
          const j = (list.data?.jobs || []).find((x) => x.origin === 'cli' && x.kind === kind && x.instanceName === name)
          if (j && ['done', 'failed', 'cancelled'].includes(j.status)) return j
          await sleep(300)
        }
        return null
      }
      const mk = await runCli(['new', '--name', 'arena2'])
      const cliCreate = await findCliJob('instance_create', 'arena2')
      check(
        'cs new converges with instance name + result (job_report instance/result)',
        mk.code === 0 && cliCreate?.status === 'done' && Number(cliCreate?.result?.port) > 0 && Number(cliCreate?.result?.idx) === 6,
        JSON.stringify({ exit: mk.code, job: cliCreate, out: mk.out.slice(-200) }),
      )
      const a2 = (await req('GET', '/api/instances')).data.find((x) => x.name === 'arena2')
      check(
        'cs new registers row as 待确认 (bridge_report, not assignable)',
        !!a2 && a2.provisionState === 'unconfirmed' && a2.source === 'bridge_report' && a2.port === Number(cliCreate?.result?.port),
        JSON.stringify(a2),
      )
      const rm = await runCli(['del', 'arena2', '--yes'])
      const cliDel = await findCliJob('instance_delete', 'arena2')
      const gone = !(await req('GET', '/api/instances')).data.some((x) => x.name === 'arena2')
      // 尾随 --yes 必须被识别(否则卡在交互确认 → 非 0 退出)
      check(
        'cs del <名> --yes (trailing flag) deletes + row converged away',
        rm.code === 0 && cliDel?.status === 'done' && Number(cliDel?.result?.freedBytes) >= 0 && gone,
        JSON.stringify({ exit: rm.code, job: cliDel, out: rm.out.slice(-200) }),
      )
    }

    // ---- M6 前置:跨进程取消(取消请求落 <jobs>/<id>.cancel,执行进程轮询消费)----
    // 两个方向都覆盖:平台任务(守护执行)由 CLI 取消;CLI 任务由平台取消。
    {
      const runCli = (args, extraEnv = {}) =>
        new Promise((resolve) => {
          const proc = spawn(CS_BIN, ['--config', cfgPath, ...args], {
            cwd: V2,
            env: { ...process.env, ARENA_STUB_DIR: stub, ...extraEnv },
            stdio: ['ignore', 'pipe', 'pipe'],
          })
          let out = ''
          proc.stdout.on('data', (d) => (out += String(d)))
          proc.stderr.on('data', (d) => (out += String(d)))
          const t = setTimeout(() => proc.kill('SIGTERM'), 90000)
          proc.once('close', (code) => {
            clearTimeout(t)
            resolve({ code, out })
          })
        })
      const findJob = async (pred, want = ['running', 'cancelling'], tries = 30) => {
        for (let i = 0; i < tries; i++) {
          const list = await req('GET', '/api/jobs?limit=20')
          const j = (list.data?.jobs || []).find(pred)
          if (j && want.includes(j.status)) return j
          await sleep(300)
        }
        return null
      }
      const waitTerminal = async (id, tries = 80) => {
        for (let i = 0; i < tries; i++) {
          await sleep(300)
          const cur = await req('GET', `/api/jobs/${id}`)
          const st = cur.data?.job?.status
          if (['done', 'failed', 'cancelled'].includes(st)) return st
        }
        return null
      }

      // ① 平台任务(守护执行)← CLI 取消:守护的实例建任务在 clone 步骤睡 3s,窗口内取消
      const created = await req('POST', '/api/instances', { name: 'arena3', gameServerId: 'g1', cloneFrom: 'main' })
      check(
        'cancel fixture: platform create job started',
        created.status === 201 && created.data.task?.kind === 'instance_create',
        JSON.stringify(created.data).slice(0, 200),
      )
      const pjId = created.data.task?.id
      const pj = await findJob((x) => x.id === pjId, ['running'])
      check('cancel fixture: platform job running', !!pj, `job=${pjId}`)
      const cliCancel = await runCli(['job', 'cancel', String(pjId)])
      check(
        'cs job cancel on platform job accepted (cross-process request)',
        cliCancel.code === 0 && /取消请求已提交/.test(cliCancel.out),
        JSON.stringify({ exit: cliCancel.code, out: cliCancel.out.slice(-200) }),
      )
      const pjFinal = await waitTerminal(pjId)
      const a3Gone = !(await req('GET', '/api/instances')).data.some((x) => x.name === 'arena3')
      check(
        'CLI cancel stops guardian-executed job (cancelled) + reserved row withdrawn',
        pjFinal === 'cancelled' && a3Gone,
        `status=${pjFinal} rowGone=${a3Gone}`,
      )
      // 请求文件必须在终态后清掉(否则 jobs/ 里越积越多;CLI 退出时机还会让"终态后清理"整段丢掉)
      check(
        'cancel request file cleaned after terminal',
        !existsSync(path.join(path.dirname(cfgPath), 'jobs', `${pjId}.cancel`)),
        `leftover=${path.join(path.dirname(cfgPath), 'jobs', `${pjId}.cancel`)}`,
      )

      // ② CLI 任务(CLI 进程执行)← 平台取消:CLI 侧 clone 也睡 3s,平台经守护落请求文件
      const cliCreate = runCli(['new', '--name', 'arena5'], { STUB_CLONE_SLEEP_S: '3' })
      const cj = await findJob((x) => x.origin === 'cli' && x.kind === 'instance_create' && x.instanceName === 'arena5', ['running'])
      check('cancel fixture: CLI job visible as running on platform', !!cj, JSON.stringify(cj))
      const platCancel = cj ? await req('POST', `/api/jobs/${cj.id}/cancel`, {}) : { status: 0 }
      check('platform cancel accepted for CLI job', platCancel.status === 200, JSON.stringify(platCancel.data))
      const cjFinal = cj ? await waitTerminal(cj.id) : null
      const cliRes = await cliCreate
      const a5Gone = !(await req('GET', '/api/instances')).data.some((x) => x.name === 'arena5')
      check(
        'platform cancel stops CLI-executed job (cancelled) + no phantom row',
        cjFinal === 'cancelled' && cliRes.code === 1 && a5Gone,
        `status=${cjFinal} exit=${cliRes.code} rowGone=${a5Gone} out=${cliRes.out.slice(-200)}`,
      )
      check(
        'cancel request file cleaned after CLI-executed job terminal',
        !cj || !existsSync(path.join(path.dirname(cfgPath), 'jobs', `${cj.id}.cancel`)),
        `leftover=${cj ? path.join(path.dirname(cfgPath), 'jobs', `${cj.id}.cancel`) : '(无任务)'}`,
      )
    }

    // 断线重连:SIGTERM 桥 → connected=false;再起 → connected=true(退避 1s 起步)
    bridge.kill('SIGTERM')
    const exitCode = await waitExit(bridge)
    check('bridge exits 0 on SIGTERM', exitCode === 0, `exit=${exitCode} out=${bridgeOut.slice(-300)}`)
    let down = false
    for (let i = 0; i < 20; i++) {
      const g = await req('GET', '/api/game-servers')
      if (g.data?.[0]?.connected === false) {
        down = true
        break
      }
      await sleep(200)
    }
    check('backend sees disconnect', down)
    spawnBridge()
    let back = false
    for (let i = 0; i < 40; i++) {
      const g = await req('GET', '/api/game-servers')
      if (g.data?.[0]?.connected === true) {
        back = true
        break
      }
      await sleep(250)
    }
    check('bridge reconnects', back)

    console.log(`\n[e2e] RESULT: ${pass} passed, ${fail} failed`)
    if (fail > 0) process.exitCode = 1
  } finally {
    if (bridge) bridge.kill('SIGTERM')
    server.kill('SIGTERM')
    await sleep(300)
    rmSync(stub, { recursive: true, force: true })
  }
}

main().catch((e) => {
  console.error('[e2e] error:', e)
  process.exitCode = 1
})
