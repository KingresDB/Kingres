// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// One command: npm run e2e
// Builds the production panel, builds kingresd for Linux with that panel embedded in its place (a Go
// overlay replaces the placeholder files of internal/webui/dist in the build only; nothing in the
// control plane is changed), starts it in WSL2 on a private data directory, drives the panel that the
// server itself serves (with the server's own headers) in a headless browser through the real flow,
// prints a report, and tears everything down. Exit code 0 only if every step passed.
//
// Options (environment): KINGRESD_BIN (use this Linux binary instead of building),
// E2E_KEEP_INSTALL=1 (keep ~/kw-web/install for the next run), E2E_KEEP_RUNNING=1 (do not stop).
import { spawnSync } from 'node:child_process'
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { ADMIN, runFlow } from './flow.mjs'
import { sleep } from './browser.mjs'

const here = dirname(fileURLToPath(import.meta.url))
const web = resolve(here, '../..')
const work = join(web, '.e2e')
const API_PORT = process.env.E2E_API_PORT ?? '58201'
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
        WSLENV: 'E2E_ADMIN_PASSWORD:E2E_ADMIN_USER:E2E_API_PORT:E2E_PG_PORT:E2E_PG_INSTALL',
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

function buildPanel() {
  console.log('Building the production panel ...')
  const viteBin = join(web, 'node_modules/vite/bin/vite.js')
  const r = spawnSync(process.execPath, [viteBin, 'build'], { cwd: web, encoding: 'utf8' })
  if (r.status !== 0) throw new Error(`vite build failed: ${r.stdout}${r.stderr}`)
}

function listFiles(dir) {
  return readdirSync(dir).flatMap((name) => {
    const p = join(dir, name)
    return statSync(p).isDirectory() ? listFiles(p) : [p]
  })
}

/** A Go build overlay that puts the built panel in place of the placeholder in internal/webui/dist. */
function writeOverlay(controlPlane) {
  const target = join(controlPlane, 'internal/webui/dist')
  const replace = {}
  for (const f of readdirSync(target)) replace[join(target, f)] = '' // the placeholder files
  for (const f of listFiles(join(web, 'dist')))
    replace[join(target, relative(join(web, 'dist'), f))] = f
  const file = join(work, 'overlay.json')
  writeFileSync(file, JSON.stringify({ Replace: replace }))
  return file
}

function buildBinary() {
  if (process.env.KINGRESD_BIN) return process.env.KINGRESD_BIN
  mkdirSync(work, { recursive: true })
  const out = join(work, 'kingresd')
  const controlPlane = resolve(web, '../control-plane')
  console.log('Building kingresd for Linux (with the built panel embedded) ...')
  const r = spawnSync(
    'go',
    ['build', '-overlay', writeOverlay(controlPlane), '-o', out, './cmd/kingresd'],
    {
      cwd: controlPlane,
      env: { ...process.env, GOOS: 'linux', GOARCH: 'amd64', CGO_ENABLED: '0' },
      encoding: 'utf8',
    },
  )
  if (r.status !== 0) throw new Error(`go build failed: ${r.stderr}`)
  return out
}

/** The standalone verifier for the ledger proofs (standard library Go, built for this machine). */
function buildVerifier() {
  const out = join(work, process.platform === 'win32' ? 'kingres-verify.exe' : 'kingres-verify')
  const r = spawnSync('go', ['build', '-o', out, './cmd/kingres-verify'], {
    cwd: resolve(web, '../verifier'),
    encoding: 'utf8',
  })
  if (r.status !== 0) throw new Error(`go build of kingres-verify failed: ${r.stderr}`)
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

let cleaned = false
function cleanup() {
  if (cleaned || process.env.E2E_KEEP_RUNNING) return
  cleaned = true
  try {
    wsl(['stop', ...(process.env.E2E_KEEP_INSTALL ? [] : ['--remove-install'])], { quiet: true })
    console.log('Backend stopped and removed.')
  } catch (e) {
    console.error(String(e))
  }
  for (const f of [
    'kingresd',
    'overlay.json',
    'kingres-verify',
    'kingres-verify.exe',
    'proof-kept.json',
  ])
    rmSync(join(work, f), { force: true })
  rmSync(join(work, 'downloads'), { recursive: true, force: true })
}
process.on('SIGINT', () => {
  cleanup()
  process.exit(130)
})

async function main() {
  buildPanel()
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
  if (!process.env.E2E_DATA_DIR) process.env.E2E_DATA_DIR = wsl(['datadir'], { quiet: true }).trim()
  await waitFor(`http://127.0.0.1:${API_PORT}/api/v1/healthz`, 'kingresd')

  const baseUrl = `http://127.0.0.1:${API_PORT}`
  await waitFor(baseUrl + '/', 'the panel (served by kingresd)')

  const downloads = join(work, 'downloads')
  rmSync(downloads, { recursive: true, force: true })
  mkdirSync(downloads, { recursive: true })
  console.log('Running the browser flow ...')
  const report = await runFlow({
    baseUrl,
    setupToken: () => wsl(['token'], { quiet: true }).trim(),
    psql: (db, sql) => wsl(['psql', db, sql], { quiet: true }),
    restartApi: () => wsl(['restart'], { quiet: true }),
    serverLog: () => wsl(['log'], { quiet: true }),
    verifier: buildVerifier(),
    downloads,
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
  console.log('\n== Content-Security-Policy violations')
  console.log(report.log.csp.length ? report.log.csp.join('\n') : ' none')
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
  return failed.length === 0 && report.log.pageErrors.length === 0 && report.log.csp.length === 0
    ? 0
    : 1
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
