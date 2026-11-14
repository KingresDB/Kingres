// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// One command: npm run e2e
// Builds kingresd for Linux, starts it in WSL2 on a private data directory, starts the panel with the
// mock API off (Vite proxy to the backend), drives the panel in a headless browser through the real
// flow, prints a report, and tears everything down. Exit code 0 only if every step passed.
//
// Options (environment): KINGRESD_BIN (use this Linux binary instead of building),
// E2E_KEEP_INSTALL=1 (keep ~/kw-web/install for the next run), E2E_KEEP_RUNNING=1 (do not stop).
import { spawn, spawnSync } from 'node:child_process'
import { copyFileSync, existsSync, mkdirSync, rmSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { ADMIN, runFlow } from './flow.mjs'
import { sleep } from './browser.mjs'

const here = dirname(fileURLToPath(import.meta.url))
const web = resolve(here, '../..')
const work = join(web, '.e2e')
const API_PORT = process.env.E2E_API_PORT ?? '58201'
const PANEL_PORT = process.env.E2E_PANEL_PORT ?? '5188'
const DISTRO = process.env.E2E_WSL_DISTRO ?? 'Ubuntu-24.04'
const WSL_USER = process.env.E2E_WSL_USER ?? 'kingres'

function wsl(args, { quiet = false } = {}) {
  const r = spawnSync(
    'wsl.exe',
    ['-d', DISTRO, '-u', WSL_USER, '--cd', here, '--', 'bash', './backend.sh', ...args],
    {
      encoding: 'utf8',
      env: {
        ...process.env,
        WSLENV: 'E2E_ADMIN_PASSWORD:E2E_ADMIN_USER:E2E_API_PORT:E2E_PG_PORT',
        E2E_ADMIN_PASSWORD: ADMIN.password,
        E2E_ADMIN_USER: ADMIN.user,
      },
    },
  )
  if (!quiet && r.stdout.trim()) console.log(r.stdout.trim())
  if (r.status !== 0)
    throw new Error(
      `backend.sh ${args.join(' ')} failed: ${(r.stderr || r.stdout).trim().slice(0, 600)}`,
    )
  return r.stdout
}

function buildBinary() {
  if (process.env.KINGRESD_BIN) return process.env.KINGRESD_BIN
  mkdirSync(work, { recursive: true })
  const out = join(work, 'kingresd')
  console.log('Building kingresd for Linux ...')
  const r = spawnSync('go', ['build', '-o', out, './cmd/kingresd'], {
    cwd: resolve(web, '../control-plane'),
    env: { ...process.env, GOOS: 'linux', GOARCH: 'amd64', CGO_ENABLED: '0' },
    encoding: 'utf8',
  })
  if (r.status !== 0) throw new Error(`go build failed: ${r.stderr}`)
  return out
}

async function waitFor(url, what, ms = 30000) {
  const end = Date.now() + ms
  while (Date.now() < end) {
    try {
      const res = await fetch(url)
      if (res.ok) return
    } catch {
      // not up yet
    }
    await sleep(300)
  }
  throw new Error(`${what} did not come up at ${url}`)
}

let vite = null
let cleaned = false
function cleanup() {
  if (cleaned || process.env.E2E_KEEP_RUNNING) return
  cleaned = true
  if (vite && vite.pid)
    spawnSync('taskkill', ['/PID', String(vite.pid), '/T', '/F'], { stdio: 'ignore' })
  try {
    wsl(['stop', ...(process.env.E2E_KEEP_INSTALL ? [] : ['--remove-install'])], { quiet: true })
    console.log('Backend stopped and removed.')
  } catch (e) {
    console.error(String(e))
  }
  rmSync(join(work, 'kingresd'), { force: true })
}
process.on('SIGINT', () => {
  cleanup()
  process.exit(130)
})

async function main() {
  const binary = buildBinary()
  // The binary is handed to WSL by a path relative to scripts/e2e (no spaces in the argument).
  const staged = join(here, 'kingresd.tmp')
  copyFileSync(binary, staged)
  try {
    wsl(['prepare', './kingresd.tmp'])
  } finally {
    rmSync(staged, { force: true })
  }
  wsl(['start'])
  await waitFor(`http://127.0.0.1:${API_PORT}/api/v1/healthz`, 'kingresd')

  console.log('Starting the panel (mock API off) ...')
  const viteBin = join(web, 'node_modules/vite/bin/vite.js')
  vite = spawn(
    process.execPath,
    [viteBin, '--mode', 'backend', '--port', PANEL_PORT, '--strictPort'],
    {
      cwd: web,
      env: { ...process.env, KINGRES_API_TARGET: `http://127.0.0.1:${API_PORT}` },
      stdio: 'ignore',
    },
  )
  const baseUrl = `http://localhost:${PANEL_PORT}`
  await waitFor(baseUrl + '/', 'the panel')

  console.log('Running the browser flow ...')
  const report = await runFlow({
    baseUrl,
    setupToken: () => wsl(['token'], { quiet: true }).trim(),
    psql: (db, sql) => wsl(['psql', db, sql], { quiet: true }),
    restartApi: () => wsl(['restart'], { quiet: true }),
    shots: join(work, 'shots'),
  })

  console.log('\n== Notes')
  for (const n of report.notes) console.log(' -', n)
  console.log('\n== Pages (mock off)')
  for (const p of report.pageReport) {
    console.log(
      ` ${p.path.padEnd(18)} h1="${p.h1}" loading=${p.loading} overflow=${p.overflow} danger=${JSON.stringify(p.dangerNotices)} info=${JSON.stringify(p.infoNotices)}`,
    )
  }
  console.log('\n== HTTP errors seen by the browser (method path status code), grouped')
  const groups = new Map()
  for (const r of report.log.responses) {
    const key = `${r.method} ${r.path.replace(/\/\d+/g, '/:n')} ${r.status} ${r.code}`
    groups.set(key, (groups.get(key) ?? 0) + 1)
  }
  for (const [k, n] of [...groups].sort()) console.log(` ${String(n).padStart(3)} x ${k}`)
  if (report.log.pageErrors.length) console.log('\n== Page errors\n', report.log.pageErrors)

  const failed = report.results.filter((r) => !r.ok)
  console.log(`\n${report.results.length - failed.length}/${report.results.length} steps passed`)
  for (const f of failed) console.log(` FAILED: ${f.name}: ${f.error}`)
  if (existsSync(join(work, 'shots'))) console.log(`Screenshots: ${join(work, 'shots')}`)
  return failed.length === 0 && report.log.pageErrors.length === 0 ? 0 : 1
}

let code = 1
try {
  code = await main()
} catch (e) {
  console.error(String(e))
} finally {
  cleanup()
}
process.exit(code)
