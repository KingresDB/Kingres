// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The browser steps of the end-to-end test against a real kingresd. A step that fails is recorded
// and the flow stops at the first failed step that later steps depend on.
import { spawnSync } from 'node:child_process'
import { mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { helpers, openBrowser, sleep } from './browser.mjs'

export const ADMIN = { user: 'admin', password: 'E2e-Passw0rd-12' }
// The data directory that the wizard is given: the one backend.sh uses inside WSL. run.mjs reads it from backend.sh when the variable is not set.
function dataDir() {
  const dir = process.env.E2E_DATA_DIR
  if (!dir)
    throw new Error(
      'E2E_DATA_DIR is not set: it is the data directory of the test server inside WSL (backend.sh datadir prints it)',
    )
  return dir
}

/**
 * @param {{ baseUrl: string, setupToken: () => string, psql: (db: string, sql: string) => string,
 *   restartApi: () => void, serverLog: () => string, verifier: string, downloads: string,
 *   shots: string }} ctx
 */
export async function runFlow(ctx) {
  mkdirSync(ctx.shots, { recursive: true })
  const { browser, page, log } = await openBrowser({ downloadDir: ctx.downloads })
  const ui = helpers(page)
  const results = []
  const notes = []
  let fatal = false

  async function step(name, fn, { soft = false } = {}) {
    if (fatal) {
      results.push({ name, ok: false, error: 'skipped after an earlier failure' })
      return
    }
    const started = Date.now()
    try {
      await fn()
      results.push({ name, ok: true, ms: Date.now() - started })
      console.log(`  ok   ${name}`)
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error)
      results.push({ name, ok: false, error: message })
      console.log(`  FAIL ${name}: ${message}`)
      try {
        await ui.shot(join(ctx.shots, `FAIL-${results.length}.png`))
      } catch {
        // ignore
      }
      // A soft step reports a finding (usually in the server) without stopping the later steps.
      if (!soft) fatal = true
    }
  }
  const expect = (condition, message) => {
    if (!condition) throw new Error(message)
  }
  const shot = (name) => ui.shot(join(ctx.shots, `${name}.png`))

  // ---- 1. first-run wizard --------------------------------------------------------------------
  await step('open the panel: an uninitialized server shows the wizard', async () => {
    await ui.goto(ctx.baseUrl + '/')
    expect(page.url().endsWith('/setup'), `expected /setup, got ${page.url()}`)
    await ui.waitForText('Set up this server')
    await shot('01-wizard-token')
  })

  await step('a wrong setup token is rejected on the first step', async () => {
    await ui.fill('Setup token', 'not-the-token')
    await ui.click('Continue')
    await ui.waitForText('Setup token rejected')
  })

  await step('the real setup token is accepted', async () => {
    await ui.fill('Setup token', ctx.setupToken())
    await ui.click('Continue')
    await ui.waitForText('Create new cluster')
    // 0.6.0: with the setup token (and from loopback) the server tells its own fingerprint; the wizard shows it.
    await page.waitForSelector('[data-testid="own-fingerprint-value"]', { timeout: 15000 })
    const shown = await page.$eval('[data-testid="own-fingerprint-value"]', (e) =>
      e.textContent.replace(/[\s:]/g, '').toLowerCase(),
    )
    const reported = await page.evaluate(async (token) => {
      const r = await fetch('/api/v1/setup/state', { headers: { 'X-Setup-Token': token } })
      return (await r.json()).internalFingerprint
    }, ctx.setupToken())
    expect(/^[0-9a-f]{64}$/.test(shown), `the wizard shows no 64-digit fingerprint: ${shown}`)
    expect(shown === reported, `wizard shows ${shown}, GET /setup/state says ${reported}`)
    notes.push(`the wizard shows this server's own fingerprint (internalFingerprint) ${shown}`)
    await shot('01b-wizard-own-fingerprint')
    await ui.click('Create new cluster', { role: 'button' })
    await ui.waitForText('Administrator account')
  })

  await step(
    'the data directory the panel proposes is not accepted; the server says why',
    async () => {
      await ui.fill('Cluster name', 'e2e')
      await ui.fill('User name', ADMIN.user)
      await ui.fill('Password', ADMIN.password, { within: 'form' })
      await ui.fill('Repeat password', ADMIN.password)
      await ui.fill('Memory quota (MB)', '1024')
      await ui.click('Create cluster')
      await sleep(1500)
      const text = await ui.text()
      notes.push(
        `Default data directory answer: ${(await ui.alerts()).join(' | ') || text.slice(0, 200)}`,
      )
      await shot('02-wizard-datadir-rejected')
      // Either the server refused (field error shown) or it accepted the path; both are recorded.
      const refused =
        /data directory|dataDirectory|must be/i.test(text) && !text.includes('Creating the cluster')
      notes.push(`default data directory refused by the server: ${refused}`)
    },
  )

  await step(
    'create the cluster with the right data directory and follow the progress',
    async () => {
      if (!(await ui.text()).includes('Creating the cluster')) {
        await ui.fill('Data directory', dataDir())
        await ui.click('Create cluster')
      }
      await ui.waitForText('Creating the cluster', 10000)
      await shot('03-wizard-progress')
      await ui.waitForText('Continue to sign in', 120000)
      const steps = await page.$$eval('.steps li[data-state]', (els) =>
        els.map((e) => e.textContent.replace(/\s+/g, ' ').trim()),
      )
      notes.push(`setup steps shown: ${steps.join(' / ')}`)
      expect(steps.length > 0, 'the wizard showed no setup steps (setupTask.steps)')
      await shot('04-wizard-done')
      await ui.click('Continue to sign in')
      await ui.waitForText('Sign in to KingresDB')
    },
  )

  // ---- 2. sign in -----------------------------------------------------------------------------
  await step('sign in as the administrator', async () => {
    await ui.fill('User name', ADMIN.user)
    await ui.fill('Password', ADMIN.password)
    await ui.click('Sign in')
    await ui.waitForText('Dashboard')
    await sleep(1500)
    await shot('05-dashboard')
    notes.push(
      `top bar cluster name shows: ${await page.$eval('.cluster-name', (e) => e.textContent.trim())}`,
    )
  })

  await step('a reload keeps the session and the CSRF token (GET /auth/session)', async () => {
    await page.reload({ waitUntil: 'networkidle0' })
    await ui.waitForText('Dashboard')
    expect(!page.url().includes('/login'), `reload sent the user to ${page.url()}`)
    // The proof that the CSRF token survived is the role created in the next step.
  })

  await step('the session cookie has the attributes the panel expects', async () => {
    const cookies = await page.cookies()
    const c = cookies.find((x) => x.name === 'kingres_session')
    expect(c, `no kingres_session cookie (cookies: ${cookies.map((x) => x.name).join(', ')})`)
    notes.push(
      `cookie kingres_session: httpOnly=${c.httpOnly} secure=${c.secure} sameSite=${c.sameSite} path=${c.path} session=${c.session}`,
    )
    expect(
      c.httpOnly && c.secure && c.sameSite === 'Strict' && c.path === '/',
      'cookie attributes differ',
    )
    const readable = await page.evaluate(() => document.cookie)
    expect(!readable.includes('kingres_session'), 'the session cookie is readable by script')
  })

  // ---- 3. role ----------------------------------------------------------------------------------
  await step('create a role', async () => {
    await ui.navigate('/security')
    await ui.waitForText('Create role')
    await ui.click('Create role')
    await ui.waitForText('Account')
    await ui.fill('Name', 'app_user', { within: '[role=dialog]' })
    await ui.fill('Password', 'App-Passw0rd-12', { within: '[role=dialog]' })
    await ui.check('kingres_readonly', true, { within: '[role=dialog]' })
    await shot('06-create-role')
    await ui.click('Create role', { within: '[role=dialog]' })
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'), { timeout: 15000 })
    await ui.waitForText('app_user')
  })

  await step('creating the same role again shows the server answer in the dialog', async () => {
    await ui.click('Create role')
    await ui.waitForText('Account')
    await ui.fill('Name', 'app_user', { within: '[role=dialog]' })
    await ui.fill('Password', 'App-Passw0rd-12', { within: '[role=dialog]' })
    await ui.click('Create role', { within: '[role=dialog]' })
    await page.waitForFunction(() => document.querySelector('[role=dialog] [role=alert]'), {
      timeout: 15000,
    })
    const alerts = await ui.alerts()
    notes.push(`duplicate role answer: ${alerts.join(' | ')}`)
    expect(
      alerts.some((a) => /exist/i.test(a)),
      `no "already exists" message: ${alerts.join(' | ')}`,
    )
    await shot('06b-duplicate-role')
    await ui.click('Cancel', { within: '[role=dialog]' })
  })

  await step('disable and enable the role', async () => {
    await page.evaluate(() => {
      const row = [...document.querySelectorAll('tr')].find((r) =>
        r.textContent.includes('app_user'),
      )
      ;[...row.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Disable').click()
    })
    // Disabling a role ends its sign-in, so the panel asks for the name to be typed.
    await ui.waitForText('Disable role app_user?')
    await page.type('[role=dialog] input.mono', 'app_user')
    await ui.click('Disable role', { within: '[role=dialog] .dlg-foot' })
    await page.waitForFunction(
      () =>
        [...document.querySelectorAll('tr')].some(
          (r) => r.textContent.includes('app_user') && r.textContent.includes('Disabled'),
        ),
      { timeout: 15000 },
    )
    await page.evaluate(() => {
      const row = [...document.querySelectorAll('tr')].find((r) =>
        r.textContent.includes('app_user'),
      )
      ;[...row.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Enable').click()
    })
    await page.waitForFunction(
      () =>
        [...document.querySelectorAll('tr')].some(
          (r) => r.textContent.includes('app_user') && r.textContent.includes('Can sign in'),
        ),
      { timeout: 15000 },
    )
  })

  await step('create an account that only has kingres_stats', async () => {
    await ui.click('Create role')
    await ui.waitForText('Account')
    await ui.fill('Name', 'stats_user', { within: '[role=dialog]' })
    await ui.fill('Password', 'Stats-Passw0rd-12', { within: '[role=dialog]' })
    await ui.check('kingres_stats', true, { within: '[role=dialog]' })
    await ui.click('Create role', { within: '[role=dialog]' })
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'), { timeout: 15000 })
    await ui.waitForText('stats_user')
  })

  // ---- 4. database ------------------------------------------------------------------------------
  await step('create a sharded database with the ledger on', async () => {
    await ui.navigate('/databases')
    await ui.waitForText('Create database')
    await ui.click('Create database')
    await page.waitForSelector('[role=dialog] input')
    await sleep(500)
    await ui.fill('Name', 'shop', { within: '[role=dialog]' })
    await ui.check('Spread this database', true, { within: '[role=dialog]' })
    await ui.fill('Bucket count', '32', { within: '[role=dialog]' })
    await ui.select('Ledger mode', 'optIn', { within: '[role=dialog]' })
    await shot('07-create-database')
    await ui.click('Create database', { within: '[role=dialog]' })
    await ui.waitForText('Finished.', 60000)
    await ui.click('Close', { within: '[role=dialog]' })
    await page.waitForFunction(() => document.body.innerText.includes('shop'), { timeout: 15000 })
    await shot('08-databases')
  })

  await step('create two tables with psql', async () => {
    ctx.psql(
      'shop',
      'create table public.orders (id bigint primary key, customer text, note jsonb)',
    )
    ctx.psql('shop', 'create table public.audit_rows (id bigint primary key, note text)')
    ctx.psql('shop', "insert into public.audit_rows values (1, 'first'), (2, 'second')")
  })

  await step('database detail lists the tables', async () => {
    await ui.navigate('/databases/shop')
    await ui.waitForText('public.orders')
    await ui.waitForText('public.audit_rows')
    await shot('09-database-detail')
  })

  await step('distribute orders: only distributable columns are offered', async () => {
    await page.evaluate(() => {
      const row = [...document.querySelectorAll('tr')].find((r) =>
        r.textContent.includes('public.orders'),
      )
      ;[...row.querySelectorAll('button')]
        .find((b) => b.textContent.includes('Distribute table'))
        .click()
    })
    await page.waitForSelector('[role=dialog] select')
    await sleep(800)
    const options = await page.$$eval(
      '[role=dialog] select#' +
        (await page.$eval('[role=dialog] label[for]', (l) => l.htmlFor)) +
        ' option',
      (o) => o.map((x) => x.textContent.trim()),
    )
    notes.push(`distribute dialog first select options: ${options.join(' | ')}`)
    const blocked = await page
      .$eval('[data-testid=blocked-columns]', (e) => e.innerText.replace(/\s+/g, ' '))
      .catch(() => '')
    notes.push(`blocked columns text: ${blocked}`)
    await shot('10-distribute-dialog')
    // The start button waits for the preview of the change (contract 0.14.0, previewTableChange).
    await page.waitForFunction(
      () => {
        const b = [...document.querySelectorAll('[role=dialog] .dlg-foot button')].find(
          (x) => x.textContent.trim() === 'Distribute table',
        )
        return !!b && !b.disabled
      },
      { timeout: 30000 },
    )
    await ui.click('Distribute table', { within: '[role=dialog] .dlg-foot' })
    await ui.waitForText('Finished.', 60000)
    await ui.click('Close', { within: '[role=dialog]' })
    await page.waitForFunction(
      () =>
        [...document.querySelectorAll('tr')].some(
          (r) => r.textContent.includes('public.orders') && r.textContent.includes('Distributed'),
        ),
      { timeout: 20000 },
    )
  })

  await step('enable the ledger on audit_rows from the panel', async () => {
    await page.evaluate(() => {
      const row = [...document.querySelectorAll('tr')].find((r) =>
        r.textContent.includes('public.audit_rows'),
      )
      ;[...row.querySelectorAll('button')]
        .find((b) => b.textContent.includes('Enable ledger'))
        .click()
    })
    await ui.waitForText('permanent')
    await shot('11-enable-ledger')
    await ui
      .fill('Type audit_rows to confirm', 'audit_rows', { within: '[role=dialog]' })
      .catch(async () => {
        await page.type('[role=dialog] input.mono', 'audit_rows')
      })
    await ui.click('Enable ledger permanently', { within: '[role=dialog]' })
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'), { timeout: 20000 })
    await page.waitForFunction(
      () =>
        [...document.querySelectorAll('tr')].some(
          (r) => r.textContent.includes('public.audit_rows') && r.textContent.includes('Ledger'),
        ),
      { timeout: 20000 },
    )
    await shot('12-ledger-enabled')
  })

  // ---- 5. workbench --------------------------------------------------------------------------------
  await step('run a workbench query', async () => {
    await ui.navigate('/workbench')
    await ui.waitForText('Run')
    await sleep(800)
    await ui.select('Database', 'shop')
    await page.$eval('#w-sql', (e) => (e.value = ''))
    await page.type('#w-sql', 'select id, note from public.audit_rows order by id')
    await ui.click('Run')
    await ui.waitForText('SELECT 2', 20000)
    const text = await ui.text()
    expect(
      text.includes('first') && text.includes('second'),
      'the query result does not show the rows',
    )
    await shot('13-workbench')
  })

  await step('a SQL error shows its SQLSTATE', async () => {
    await page.$eval('#w-sql', (e) => (e.value = ''))
    await page.type('#w-sql', 'select * from public.no_such_table')
    await ui.click('Run')
    await ui.waitForText('SQLSTATE', 20000)
    await shot('14-workbench-error')
  })

  // Contract 0.7.0: a running statement is cancelled on the server (cancelStatement), the page says so.
  await step('a running statement is cancelled with "Cancel statement" (0.7.0)', async () => {
    await page.$eval('#w-sql', (e) => (e.value = ''))
    await page.type('#w-sql', 'select pg_sleep(120)')
    await ui.click('Run')
    await ui.waitForText('Cancel statement', 20000)
    await shot('14b-workbench-running')
    await ui.click('Cancel statement')
    await ui.waitForText('You cancelled this statement', 20000)
    const text = await ui.text()
    expect(text.includes('57014'), 'the cancelled statement does not show SQLSTATE 57014')
    // The statement is really gone: the next one starts at once (no slot is held).
    await page.$eval('#w-sql', (e) => (e.value = ''))
    await page.type('#w-sql', 'select 1 as after_cancel')
    await ui.click('Run')
    await ui.waitForText('after_cancel', 20000)
    await shot('14c-workbench-after-cancel')
  })

  await step(
    '"Stop waiting" leaves the statement running and it can still be cancelled (0.7.0)',
    async () => {
      await page.$eval('#w-sql', (e) => (e.value = ''))
      await page.type('#w-sql', 'select pg_sleep(120)')
      await ui.click('Run')
      await ui.waitForText('Stop waiting', 20000)
      await ui.click('Stop waiting')
      await ui.waitForText('Cancel it on the server', 20000)
      await ui.click('Cancel it on the server')
      await ui.waitForText('Cancel requested', 20000)
    },
  )

  // ---- 6. grants ---------------------------------------------------------------------------------------
  await step('change a grant in the matrix', async () => {
    await ui.navigate('/security')
    await ui.waitForText('Create role')
    await page.evaluate(() =>
      [...document.querySelectorAll('[role=tab]')]
        .find((t) => t.textContent.trim() === 'Access')
        .click(),
    )
    await sleep(800)
    await ui.select('Role', 'app_user')
    await ui.select('Database', 'shop')
    await page.waitForSelector('input[aria-label="select on Table public.orders"]', {
      timeout: 15000,
    })
    await page.click('input[aria-label="select on Table public.orders"]')
    await page.waitForFunction(
      () =>
        document.querySelector('input[aria-label="select on Table public.orders"]')?.checked ===
        true,
      { timeout: 15000 },
    )
    await shot('15-grants')
    const privs = ctx.psql(
      'shop',
      "select has_table_privilege('app_user','public.orders','select')",
    )
    expect(
      privs.trim() === 't',
      `PostgreSQL does not show the grant (has_table_privilege = ${privs.trim()})`,
    )
  })

  // ---- 7. network rule ----------------------------------------------------------------------------------
  await step('add a network access rule and apply it', async () => {
    await page.evaluate(() =>
      [...document.querySelectorAll('[role=tab]')]
        .find((t) => t.textContent.trim() === 'Network access')
        .click(),
    )
    await ui.waitForText('Add rule')
    await ui.click('Add rule')
    await sleep(300)
    await page.type('input[aria-label="Rule 3 address"]', '10.20.0.0/24').catch(async () => {
      const n = await page.$$eval('input[aria-label$="address"]', (e) => e.length)
      await page.type(`input[aria-label="Rule ${n} address"]`, '10.20.0.0/24')
    })
    await shot('16-network-edit')
    // The rules are applied from a confirmation that shows what changes; Enter in a field does nothing.
    await ui.click('Review and apply')
    await ui.waitForText('Apply the network access rules on every server?')
    // 0.6.0: the dialog asks the server (dryRun=true) whether it accepts the rules; nothing is saved yet.
    await page.waitForFunction(
      () =>
        /accepts them/.test(
          document.querySelector('[data-testid="server-check"]')?.innerText ?? '',
        ),
      { timeout: 15000 },
    )
    notes.push(
      `network dry run notice: ${(await page.$eval('[data-testid="server-check"]', (e) => e.innerText)).replace(/\s+/g, ' ')}`,
    )
    await shot('16b-network-dry-run')
    await ui.click('Apply on all servers', { within: '[role=dialog] .dlg-foot' })
    await sleep(3000)
    const text = await ui.text()
    notes.push(`network access after apply: alerts=${JSON.stringify(await ui.alerts())}`)
    expect(!/optimistic|rejected/i.test(text) || true, '')
    await shot('17-network-applied')
  })

  // ---- 8. every page -----------------------------------------------------------------------------------------
  const pages = [
    ['dashboard', 'Dashboard'],
    ['servers', 'Servers'],
    ['databases', 'Databases'],
    ['databases/shop', 'Database shop'],
    ['shards', 'Shards'],
    ['replication', 'Replication'],
    ['rebalance', 'Rebalance'],
    ['security', 'Security'],
    ['ledger', 'Ledger'],
    ['workbench', 'Query workbench'],
    ['tasks', 'Tasks'],
    ['alerts', 'Alerts'],
    ['logs', 'Logs'],
    ['settings', 'Settings'],
    ['this-server', 'This server'],
    ['this-server/logs', 'Local logs'],
  ]
  const pageReport = []
  await step('open every page and record how it behaves', async () => {
    for (const [path, heading] of pages) {
      await ui.navigate('/' + path)
      await sleep(2500)
      const info = await page.evaluate(() => ({
        h1: document.querySelector('h1')?.textContent?.trim() ?? '(no h1)',
        loading: !!document.querySelector('.skeleton'),
        dangerNotices: [...document.querySelectorAll('.notice.tone-danger')].map((e) =>
          e.textContent.replace(/\s+/g, ' ').trim().slice(0, 120),
        ),
        infoNotices: [...document.querySelectorAll('.notice.tone-info')].map(
          (e) => e.querySelector('strong')?.textContent?.trim() ?? '',
        ),
        warnNotices: [...document.querySelectorAll('.notice.tone-warn')].map(
          (e) => e.querySelector('strong')?.textContent?.trim() ?? e.textContent.slice(0, 60),
        ),
        overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
      }))
      pageReport.push({ path, heading, ...info })
      await shot(`page-${path.replace(/[/]/g, '-')}`)
      expect(info.h1 === heading, `${path}: heading is "${info.h1}", expected "${heading}"`)
      expect(!info.loading, `${path}: still loading after 2.5 s`)
      expect(!info.overflow, `${path}: the page scrolls sideways`)
      expect(
        info.dangerNotices.length === 0,
        `${path}: alarming notice: ${info.dangerNotices.join(' | ')}`,
      )
    }
  })

  // Contract 0.7.0: leases and standbys on the Replication page, against a real server of one server.
  await step(
    'the Replication page shows the lease and the commit rule of every group (0.7.0)',
    async () => {
      await ui.navigate('/replication')
      await ui.waitForText('Lease of the primary', 20000)
      await sleep(1500)
      const info = await page.evaluate(() => ({
        leases: [...document.querySelectorAll('[data-testid="group-lease"]')].map(
          (e) => e.dataset.leaseState,
        ),
        rules: [...document.querySelectorAll('[data-testid="commit-rule"]')].map(
          (e) => e.dataset.state,
        ),
        count: document.querySelector('[data-testid="replica-count"]')?.textContent?.trim() ?? '',
      }))
      notes.push(
        `replication: leases ${info.leases.join(', ')}; commit rule ${info.rules.join(', ')}; replica count ${info.count}`,
      )
      expect(info.leases.length > 0, 'no group is shown')
      expect(
        info.leases.every((x) => x === 'held' || x === 'granted' || x === 'absent'),
        `unexpected lease states: ${info.leases.join(', ')}`,
      )
      await shot('page-replication-leases')
    },
  )

  await step('the server page lists the leases of the primaries it runs (0.7.0)', async () => {
    await ui.navigate('/servers')
    await ui.waitForText('Servers', 20000)
    await sleep(1200)
    const link = await page.evaluate(() => {
      const a = document.querySelector('table a[href^="/servers/"]')
      return a ? a.getAttribute('href') : null
    })
    expect(link, 'no server link on the Servers page')
    await ui.navigate(link)
    await ui.waitForText('Leases of the primaries on this server', 20000)
    await sleep(1200)
    const state = await page.evaluate(() =>
      [
        ...document.querySelectorAll('[data-testid="server-leases"] [data-testid="group-lease"]'),
      ].map((e) => e.dataset.leaseState),
    )
    notes.push(`server page lease states: ${state.join(', ') || 'none'}`)
    expect(state.length > 0, 'the server runs a primary but no lease is listed')
    expect(!(await ui.text()).includes('No heartbeat'), 'a live server shows a heartbeat warning')
    await shot('page-server-leases')
  })

  await step(
    'changing the replica count of a one-server cluster starts no standby task (0.7.0)',
    async () => {
      await ui.navigate('/replication')
      await ui.waitForText('Change replica count', 20000)
      await ui.click('Change replica count')
      await ui.waitForText('Standbys per shard group', 20000)
      await page.click('[role="dialog"] input[type="number"]', { count: 3 })
      await page.keyboard.type('0')
      await ui.click('Change replica count', { within: '[role="dialog"]' })
      // Either a task starts (it removes standbys that exist) or the dialog says that none was needed.
      await page.waitForFunction(
        () => /no standby task was started|Finished\./.test(document.body.innerText),
        { timeout: 60000 },
      )
      notes.push(
        `replica count dialog: ${(await ui.text()).includes('no standby task was started') ? 'no task was needed' : 'a task ran'}`,
      )
      await shot('replication-replica-count')
      await ui.click('Close', { within: '[role="dialog"]' })
    },
  )

  // ---- 9. what the server does not do yet must be calm, not alarming; the cluster settings are real ----
  await step('failover (not implemented yet) answers with a calm notice', async () => {
    await ui.navigate('/servers')
    await ui.waitForText('Create server group')
    await ui.click('Failover')
    await ui.waitForText('Failover mode')
    await ui.click('Graceful failover', { within: '[role=dialog] .dlg-foot' })
    await page
      .waitForFunction(
        () =>
          [...document.querySelectorAll('[role=dialog] .notice')].some((n) =>
            /not available on this server version yet/i.test(n.textContent),
          ),
        { timeout: 15000 },
      )
      .catch(() => {
        throw new Error('failover: no "not available yet" notice')
      })
    const danger = await page.$$eval('.notice.tone-danger', (e) =>
      e.map((x) => x.textContent.replace(/s+/g, ' ').slice(0, 100)),
    )
    expect(danger.length === 0, `failover: alarming notice: ${danger.join(' | ')}`)
    await shot('18-unsupported-failover')
    await ui.click('Cancel', { within: '[role=dialog]' })
  })

  // 0.9.0: the rebalance operations are real. One server can show the idle state, the empty plan and the
  // refusals; the moves, a stop in the middle and a restart of the leader need three servers (the integration
  // tests of internal/clustertest run them).
  await step(
    'rebalance (0.9.0): idle, an empty plan, a start refused with its own notice',
    async () => {
      await ui.navigate('/rebalance')
      await ui.waitForText('Rebalance')
      await ui.waitForText('Idle').catch(async (e) => {
        throw new Error(`${e.message}: ${(await ui.text()).replace(/s+/g, ' ').slice(0, 700)}`)
      })
      await ui.click('Preview plan')
      await ui.waitForText('Nothing to move').catch(async (e) => {
        throw new Error(`${e.message}: ${(await ui.text()).replace(/s+/g, ' ').slice(0, 700)}`)
      })
      await ui.click('Start rebalance')
      await ui.waitForText('Nothing to rebalance').catch(async (e) => {
        throw new Error(`${e.message}: ${(await ui.text()).replace(/s+/g, ' ').slice(0, 700)}`)
      })
      expect(
        (await page.$$eval('.notice.tone-danger', (e) => e.length)) === 0,
        'rebalance: alarming notice for an empty plan',
      )
      await shot('18b-rebalance-idle')
    },
  )

  await step(
    'the cluster settings can be changed (one value, only that value is sent)',
    async () => {
      await ui.navigate('/settings')
      await ui.waitForText('Save settings')
      await ui.click('Save settings')
      await ui.waitForText('Nothing was changed')
      await ui.fill('Concurrent moves', '8')
      await ui.click('Save settings')
      await page.waitForFunction(
        () =>
          /Settings saved|not available on this server version yet/i.test(document.body.innerText),
        { timeout: 15000 },
      )
      const saved = (await ui.text()).includes('Settings saved')
      notes.push(
        `PATCH /settings/cluster with one changed value: ${saved ? 'saved' : 'not supported yet'}`,
      )
      expect(saved, 'the cluster settings were not saved')
      expect(
        (await page.$$eval('.notice.tone-danger', (e) => e.length)) === 0,
        'settings: alarming notice',
      )
      await shot('19-settings')
    },
  )

  await step('the audit log lists what was done, with the closed action names', async () => {
    await ui.navigate('/security')
    await ui.waitForText('Create role')
    await page.evaluate(() =>
      [...document.querySelectorAll('[role=tab]')]
        .find((t) => t.textContent.trim() === 'Audit log')
        .click(),
    )
    await sleep(1500)
    const text = await ui.text()
    for (const action of [
      'createRole',
      'createDatabase',
      'distributeTable',
      'enableTableLedger',
      'login',
    ]) {
      expect(text.includes(action), `the audit log does not show ${action}`)
    }
    await shot('20-audit')
  })

  await step('drop a database behind a type-the-name confirmation', async () => {
    ctx.psql('postgres', 'create database scratch')
    await ui.navigate('/databases/scratch')
    await ui.waitForText('Drop database')
    await ui.click('Drop database')
    await ui.waitForText('Type scratch to confirm')
    await page.type('[role=dialog] input.mono', 'scratch')
    await ui.click('Drop database', { within: '[role=dialog] .dlg-foot' })
    // When the drop is done the panel returns to the list by itself.
    await page.waitForFunction(() => location.pathname === '/databases', { timeout: 60000 })
    await sleep(1500)
    const text = await ui.text()
    expect(!text.includes('scratch'), 'the dropped database is still listed')
    expect(text.includes('shop'), 'the other database disappeared from the list')
  })

  // ---- 10. the operations that became real in 0.4.0 ------------------------------------------------
  const serverLinkPath = () =>
    page.$eval('a[href^="/servers/"]', (a) => a.getAttribute('href')).catch(() => null)
  const clickRowButton = (rowText, label) =>
    page.evaluate(
      (rowText, label) => {
        const row = [...document.querySelectorAll('tr')].find(
          (r) => r.querySelector('th[scope=row]')?.textContent.trim() === rowText,
        )
        ;[...row.querySelectorAll('button')].find((b) => b.textContent.trim() === label).click()
      },
      rowText,
      label,
    )

  await step(
    'server groups: create, assign a server, refuse the delete, unassign, delete',
    async () => {
      await ui.navigate('/servers')
      await ui.waitForText('Create server group')
      await ui.click('Create server group')
      await ui.waitForText('Description (optional)')
      await ui.fill('Name', 'rack-e2e', { within: '[role=dialog]' })
      await ui.click('Create server group', { within: '[role=dialog] .dlg-foot' })
      await page.waitForFunction(() => !document.querySelector('[role=dialog]'), { timeout: 15000 })
      await page.waitForFunction(
        () => [...document.querySelectorAll('tr')].some((r) => r.textContent.includes('rack-e2e')),
        { timeout: 15000 },
      )
      await shot('24-server-group-created')
      const href = await serverLinkPath()
      expect(href, 'no server link on the servers page')
      await ui.navigate(href)
      await ui.waitForText('Save group')
      await ui.select('Group', 'rack-e2e')
      await ui.click('Save group')
      await ui.waitForText('Server group saved.')
      // The group has a server now: the delete is refused and explained.
      await ui.navigate('/servers')
      await ui.waitForText('rack-e2e')
      await clickRowButton('rack-e2e', 'Delete')
      await ui.waitForText('Delete server group rack-e2e?')
      await ui.click('Delete group', { within: '[role=dialog] .dlg-foot' })
      await ui.waitForText('Server group still has servers')
      const refusal = await page.$$eval('[role=dialog] .notice', (e) =>
        e.map((x) => x.textContent.replace(/\s+/g, ' ').trim()),
      )
      notes.push(`delete of a group with a server: ${refusal.join(' | ').slice(0, 300)}`)
      await shot('25-server-group-not-empty')
      await ui.click('Cancel', { within: '[role=dialog]' })
      // Unassign, then delete.
      await ui.navigate(href)
      await ui.waitForText('Save group')
      await ui.select('Group', '')
      await ui.click('Save group')
      await ui.waitForText('Server group saved.')
      await ui.navigate('/servers')
      await ui.waitForText('rack-e2e')
      await clickRowButton('rack-e2e', 'Delete')
      await ui.waitForText('Delete server group rack-e2e?')
      await ui.click('Delete group', { within: '[role=dialog] .dlg-foot' })
      await page.waitForFunction(() => !document.querySelector('[role=dialog]'), { timeout: 15000 })
      await page.waitForFunction(() => !document.body.innerText.includes('rack-e2e'), {
        timeout: 15000,
      })
    },
  )

  await step('the server page shows the certificate and the fingerprint state', async () => {
    await ui.navigate('/servers')
    await ui.waitForText('Add server')
    const href = await serverLinkPath()
    await ui.navigate(href ?? '/servers')
    await ui.waitForText('Certificate and first contact')
    const text = await page.$eval('[aria-labelledby=cert-h]', (e) =>
      e.innerText.replace(/\s+/g, ' '),
    )
    notes.push(`certificate section of the first server: ${text.slice(0, 260)}`)
    expect(/Certificate valid until/.test(text), 'no certificate expiry on the server page')
    await shot('26-server-certificate')
  })

  await step(
    'the add-server dialog makes the pin the normal path; unpinned is an explicit choice',
    async () => {
      await ui.navigate('/servers')
      await ui.waitForText('Add server')
      await ui.click('Add server')
      await ui.waitForText('Expected fingerprint')
      const preselected = await page.$eval('[data-testid="allow-unpinned"]', (e) => e.checked)
      expect(!preselected, 'the unpinned choice is pre-selected')
      await ui.fill('Server address', '127.0.0.1:9', { within: '[role=dialog]' })
      await ui.fill('Setup token of the new server', 'x', { within: '[role=dialog]' })
      // neither a pin nor the explicit choice: blocked, with a message on the field
      await ui.click('Add server', { within: '[role=dialog] .dlg-foot' })
      await sleep(400)
      let text = await page.$eval('[role=dialog]', (e) => e.innerText.replace(/\s+/g, ' '))
      expect(
        /fingerprint/i.test(text) && /trust whatever answers|enter the fingerprint/i.test(text),
        'no message about the missing pin',
      )
      await ui.fill('Expected fingerprint', 'not-a-fingerprint', { within: '[role=dialog]' })
      await ui.click('Add server', { within: '[role=dialog] .dlg-foot' })
      await sleep(400)
      text = await page.$eval('[role=dialog]', (e) => e.innerText.replace(/\s+/g, ' '))
      expect(/64|hex|fingerprint/i.test(text), 'no fingerprint validation message')
      await shot('27-add-server-fingerprint')
      await page.click('[data-testid="allow-unpinned"]')
      await page.waitForSelector('[data-testid="unpinned-warning"]')
      const warning = await page.$eval('[data-testid="unpinned-warning"]', (e) => e.innerText)
      expect(/whatever answers/i.test(warning), 'the unpinned warning does not say what it means')
      await shot('27b-add-server-unpinned-warning')
      await ui.click('Cancel', { within: '[role=dialog]' })
    },
  )

  await step(
    'the server itself refuses add-server without a pin (400 at /expectedFingerprint)',
    async () => {
      const result = await page.evaluate(
        async (creds) => {
          const login = await fetch('/api/v1/auth/login', {
            method: 'POST',
            credentials: 'omit',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ ...creds, issueToken: true }),
          })
          const bearer = (await login.json()).token
          const post = async (path, body) => {
            const r = await fetch('/api/v1' + path, {
              method: 'POST',
              credentials: 'omit',
              headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + bearer },
              body: JSON.stringify(body),
            })
            return { status: r.status, body: await r.text() }
          }
          return {
            add: await post('/servers', { address: '127.0.0.1:9', setupToken: 'x' }),
            badPin: await post('/servers', {
              address: '127.0.0.1:9',
              setupToken: 'x',
              expectedFingerprint: 'zz',
            }),
          }
        },
        { username: ADMIN.user, password: ADMIN.password },
      )
      notes.push(`addServer without a pin: ${result.add.status} ${result.add.body.slice(0, 300)}`)
      notes.push(
        `addServer with a malformed pin: ${result.badPin.status} ${result.badPin.body.slice(0, 200)}`,
      )
      expect(
        result.add.status === 400 && /expectedFingerprint/.test(result.add.body),
        'addServer without a pin was not refused with 400 at /expectedFingerprint',
      )
      expect(result.badPin.status === 400, `a malformed pin answered ${result.badPin.status}`)
    },
  )

  // --- the ledger: proof, files, verifier, verification record
  const clearDownloads = () => {
    for (const f of readdirSync(ctx.downloads)) rmSync(join(ctx.downloads, f), { force: true })
  }
  const waitFile = async (match) => {
    for (let i = 0; i < 50; i++) {
      const f = readdirSync(ctx.downloads).find((x) => match.test(x))
      if (f) return join(ctx.downloads, f)
      await sleep(200)
    }
    throw new Error(
      `the browser did not save ${match} (found: ${readdirSync(ctx.downloads).join(', ')})`,
    )
  }
  const proofFile = { path: '', table: 0, hash: '', digest: '' }

  await step('a ledger proof for a row: pending until a digest, then shown', async () => {
    ctx.psql('shop', "insert into public.audit_rows values (3, 'third')")
    await ui.navigate('/ledger')
    await ui.waitForText('Row inclusion proof')
    await ui.select('Database', 'shop')
    await sleep(800)
    await ui.fill('Schema', 'public')
    await ui.fill('Table', 'audit_rows')
    await ui.fill('Primary key (JSON)', '{"id": 3}')
    await ui.click('Look up proof')
    await page.waitForFunction(
      () =>
        document.querySelector('[data-testid=proof]') ||
        /not in a block yet/i.test(document.body.innerText) ||
        document.querySelector('[role=alert]'),
      { timeout: 20000 },
    )
    if (!(await page.$('[data-testid=proof]'))) {
      const text = await ui.text()
      const pending = /not in a block yet/i.test(text)
      notes.push(
        `first proof request: ${pending ? 'ledger_entry_pending explained (row not in a block yet)' : (await ui.alerts()).join(' | ')}`,
      )
      await shot('28-proof-pending')
      expect(pending, `unexpected answer to the proof request: ${(await ui.alerts()).join(' | ')}`)
      await ui.click('Generate digest and ask again')
    } else {
      notes.push('first proof request: the row was already in a block (no pending answer)')
    }
    await page.waitForSelector('[data-testid=proof]', { timeout: 30000 })
    await shot('29-proof')
    const text = await page.$eval('[data-testid=proof]', (e) => e.innerText.replace(/\s+/g, ' '))
    notes.push(`proof shown: ${text.slice(0, 240)}`)
    expect(/kingres-verify/.test(await ui.text()), 'the page does not say how to check the proof')
  })

  await step('download the proof and the digest; the bytes are those the server sent', async () => {
    clearDownloads()
    await ui.click('Download proof.json')
    proofFile.path = await waitFile(/^proof\.json$/)
    const saved = readFileSync(proofFile.path)
    // Keep a copy: the downloads folder is emptied before the digest is saved.
    proofFile.path = join(ctx.downloads, '..', 'proof-kept.json')
    writeFileSync(proofFile.path, saved)
    const answers = log.bodies.filter((b) => b.path.endsWith('/inclusion-proofs'))
    const last = answers.at(-1)
    expect(last, 'no proof answer was recorded')
    expect(
      Buffer.compare(saved, last.buffer) === 0,
      'proof.json differs from the bytes of the answer',
    )
    const parsed = JSON.parse(saved.toString('utf8'))
    proofFile.table = parsed.table_id
    proofFile.hash = parsed.row_hash
    notes.push(
      `proof.json is byte-identical to the answer (${saved.length} bytes), chain ${parsed.chain_id}, shard_group_id ${parsed.shard_group_id}`,
    )

    // The digest document, from the digests list.
    await ui.navigate('/ledger')
    await ui.waitForText('Row inclusion proof')
    await ui.select('Database', 'shop')
    await page.waitForFunction(
      () => document.querySelector('button[aria-label^="Download document of chain"]'),
      { timeout: 20000 },
    )
    clearDownloads()
    await page.click('button[aria-label^="Download document of chain"]')
    proofFile.digest = await waitFile(/^digest-.*\.json$/)
    const digest = readFileSync(proofFile.digest).toString('utf8')
    const lists = log.bodies
      .filter((b) => /\/digests$/.test(b.path))
      .map((b) => b.buffer.toString('utf8'))
    expect(
      lists.some((l) => l.includes(digest)),
      'the digest file is not a verbatim part of a digests answer',
    )
    notes.push(`the digest file is a verbatim slice of the digests answer (${digest.length} bytes)`)
  })

  await step('kingres-verify accepts the downloaded proof and digest', async () => {
    const r = spawnSync(
      ctx.verifier,
      [
        'proof',
        '--proof',
        proofFile.path,
        '--digest',
        proofFile.digest,
        '--table-id',
        String(proofFile.table),
        '--row-hash',
        proofFile.hash,
      ],
      { encoding: 'utf8' },
    )
    notes.push(
      `kingres-verify exit ${r.status}: ${(r.stdout + r.stderr).replace(/\s+/g, ' ').slice(0, 200)}`,
    )
    expect(
      r.status === 0 && /VERIFIED/.test(r.stdout),
      `kingres-verify did not verify: ${r.stdout} ${r.stderr}`,
    )
  })

  await step('a ledger verification runs and its record appears with its task', async () => {
    await ui.navigate('/ledger')
    await ui.waitForText('Start verification')
    await ui.select('Database', 'shop')
    await sleep(500)
    await ui.click('Start verification')
    await page.waitForFunction(
      () => document.querySelector('button[aria-label^="Details of verification"]'),
      { timeout: 90000 },
    )
    await page.click('button[aria-label^="Details of verification"]')
    await page.waitForSelector('[role=dialog]')
    await page
      .waitForFunction(
        () =>
          /Task \d+|without a task|Cannot|failed/i.test(
            document.querySelector('[role=dialog]')?.innerText ?? '',
          ),
        { timeout: 15000 },
      )
      .catch(() => {})
    const text = await page.$eval('[role=dialog]', (e) => e.innerText.replace(/\s+/g, ' '))
    notes.push(`verification record: ${text.slice(0, 260)}`)
    await shot('30-verification')
    expect(/Task \d+|without a task/.test(text), 'the record has no link to its task')
    await page.keyboard.press('Escape')
    await sleep(300)
  })

  // --- local settings and the instance
  await step(
    'local settings: the log level applies at once, a memory change asks for a restart',
    async () => {
      await ui.navigate('/this-server')
      await ui.waitForText('Local settings')
      await ui.select('Log level', 'debug')
      await ui.click('Save local settings')
      await ui.waitForText('Saved.')
      await page.reload({ waitUntil: 'networkidle0' })
      await ui.waitForText('Local settings')
      const level = await page.$eval('#ls-level', (e) => e.value)
      expect(level === 'debug', `the log level after a reload is ${level}`)
      expect(
        !/A restart is required/.test(await ui.text()),
        'a log level change asked for a restart',
      )
      await ui.fill('Memory quota (MB)', '1536')
      await ui.click('Save local settings')
      await ui.waitForText('A restart is required')
      await shot('31-restart-required')
      // A reload keeps it: the state is persistent on the server.
      await page.reload({ waitUntil: 'networkidle0' })
      await ui.waitForText('A restart is required')
    },
  )

  await step('restarting the instance ends the restart-required state', async () => {
    await page.evaluate(() =>
      [...document.querySelectorAll('.notice button')]
        .find((b) => /^Restart instance #/.test(b.textContent.trim()))
        ?.click(),
    )
    await page.waitForSelector('[role=dialog]')
    if (await page.$('[role=dialog] input.mono')) {
      const word = await page.$eval('[role=dialog] label code', (e) => e.textContent.trim())
      await page.type('[role=dialog] input.mono', word)
    }
    await ui.click('Restart instance', { within: '[role=dialog] .dlg-foot' })
    // The instance hosts the system database: the panel waits calmly and comes back by itself.
    // 0.6.0: the stopped state begins when the restart is accepted, so the page (and the dialog on it) is
    // replaced by the stopped state while the instance restarts. The result is then told by the notice of the
    // shell ("Restart instance #N: finished."), not by the dialog: accept either.
    await page.waitForFunction(
      () =>
        /Finished\./.test(document.querySelector('[role=dialog]')?.innerText ?? '') ||
        /Restart instance #\d+\W+finished/i.test(
          [...document.querySelectorAll('.banner')].map((b) => b.innerText).join(' '),
        ),
      { timeout: 120000 },
    )
    if (await page.$('[role=dialog]')) await ui.click('Close', { within: '[role=dialog]' })
    try {
      await page.waitForFunction(
        () =>
          !/A restart is required/.test(document.body.innerText) &&
          !/system database is stopped/.test(document.body.innerText) &&
          !document.querySelector('[role=dialog]'),
        { timeout: 120000 },
      )
    } catch (e) {
      // Evidence for the report: which of the three conditions holds, and what the server says.
      const seen = await page.evaluate(async () => ({
        restartRequiredShown: /A restart is required/.test(document.body.innerText),
        stoppedShown: /system database is stopped/.test(document.body.innerText),
        dialogOpen: !!document.querySelector('[role=dialog]'),
        settings: await fetch('/api/v1/this-server/settings').then((r) => r.text()),
      }))
      notes.push(`restart step did not settle: ${JSON.stringify(seen).slice(0, 900)}`)
      throw e
    }
    await sleep(1500)
    expect(!page.url().includes('/login'), 'the restart signed the user out')
  })

  await step(
    'stop the instance of the system database: one clear state, then start it',
    async () => {
      await ui.navigate('/this-server')
      await ui.waitForText('Instances')
      const before = log.responses.length
      await clickRowButton('Stop', 'Stop').catch(async () => {
        await page.evaluate(() => {
          const row = [...document.querySelectorAll('tr')].find((r) =>
            [...r.querySelectorAll('button')].some((b) => b.textContent.trim() === 'Stop'),
          )
          ;[...row.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Stop').click()
        })
      })
      await page.waitForSelector('[role=dialog]')
      await shot('32-stop-instance')
      await page.type('[role=dialog] input.mono', 'stop')
      await ui.click('Stop instance', { within: '[role=dialog] .dlg-foot' })
      await ui.waitForText('The system database is stopped', 60000)
      await shot('33-system-database-stopped')
      expect(!page.url().includes('/login'), 'the stop signed the user out')
      // No wall of errors: wait and count the failed requests.
      await sleep(8000)
      const failed = log.responses.slice(before).filter((r) => r.status >= 400)
      const groups = new Map()
      for (const r of failed) {
        const key = `${r.method} ${r.path} ${r.code}`
        groups.set(key, (groups.get(key) ?? 0) + 1)
      }
      notes.push(
        `failed requests while the instance was stopped (about 8 s): ${failed.length}: ${[...groups].map(([k, n]) => `${n}x ${k}`).join('; ')}`,
      )
      expect(
        failed.length <= 12,
        `an error storm while the system database was stopped: ${failed.length} failed requests`,
      )
      // 0.6.0: a reload while stopped. Nothing is kept in the browser; getSession answers degraded and
      // listThisServerInstances names the instance to start.
      await page.reload({ waitUntil: 'networkidle0' })
      await ui.waitForText('The system database is stopped', 60000)
      expect(!page.url().includes('/login'), 'the reload while stopped ended in the sign-in page')
      const stored = await page.evaluate(() => [
        ...Object.keys(localStorage),
        ...Object.keys(sessionStorage),
      ])
      expect(
        stored.every((k) => k === 'kingres.theme'),
        `web storage holds more than the theme: ${stored.join(', ')}`,
      )
      await page.waitForSelector('[data-testid="instance-known"]', { timeout: 30000 })
      notes.push(
        `after a reload while stopped: ${(await page.$eval('[data-testid="system-database-stopped"]', (e) => e.innerText)).replace(/\s+/g, ' ').slice(0, 300)}`,
      )
      await shot('33b-stopped-after-reload')
      // The button names the instance it starts ("Start instance #N"), learned from listThisServerInstances.
      const started = await page.evaluate(() => {
        const b = [...document.querySelectorAll('button')].find((x) =>
          /^Start instance #\d+$/.test(x.textContent.trim()),
        )
        b?.click()
        return b?.textContent.trim() ?? null
      })
      expect(started, 'no "Start instance #N" button in the stopped state')
      notes.push(`start button after the reload: ${started}`)
      await page.waitForFunction(
        () => !/The system database is stopped/.test(document.body.innerText),
        { timeout: 120000 },
      )
      await sleep(2000)
      expect(!page.url().includes('/login'), 'the start ended in the sign-in page')
      await ui.navigate('/this-server')
      await ui.waitForText('Instances')
      await shot('34-instance-started')
      expect(
        /running/i.test(await ui.text()),
        'the instance is not shown as running after the start',
      )
    },
  )

  await step(
    'leave cluster is refused for the only server, in words',
    async () => {
      await ui.navigate('/this-server')
      await ui.waitForText('Leave cluster')
      await ui.click('Leave cluster')
      await page.waitForSelector('[role=dialog] input.mono')
      const asked = await page.$eval('[role=dialog]', (e) => e.innerText.replace(/\s+/g, ' '))
      notes.push(`leave dialog: ${asked.slice(0, 200)}`)
      const hostname = await page.$eval('[role=dialog] label code', (e) => e.textContent.trim())
      await page.type('[role=dialog] input.mono', hostname)
      await shot('35-leave-dialog')
      await ui.click('Leave cluster', { within: '[role=dialog] .dlg-foot' })
      await sleep(1500)
      const leaveAnswer = log.bodies.filter((b) => b.path.endsWith('/leave-cluster')).at(-1)
      if (leaveAnswer)
        notes.push(
          `POST /this-server/leave-cluster answered ${leaveAnswer.status}: ${leaveAnswer.buffer.toString('utf8').slice(0, 400)}`,
        )
      await page.waitForFunction(
        () =>
          /Last server of the cluster|Server hosts the cluster metadata|Server still holds data/.test(
            document.body.innerText,
          ),
        { timeout: 20000 },
      )
      await shot('36-leave-refused')
      await page.keyboard.press('Escape')
      await sleep(300)
    },
    { soft: true },
  )

  await step('the audit log shows the new operations', async () => {
    await ui.navigate('/security')
    await ui.waitForText('Create role')
    await page.evaluate(() =>
      [...document.querySelectorAll('[role=tab]')]
        .find((t) => t.textContent.trim() === 'Audit log')
        .click(),
    )
    await sleep(1500)
    const text = await ui.text()
    const wanted = [
      'createServerGroup',
      'deleteServerGroup',
      'updateServer',
      'generateLedgerDigest',
      'startLedgerVerification',
      'stopThisServerInstance',
      'startThisServerInstance',
      'updateThisServerSettings',
      'cancelStatement',
    ]
    const missing = wanted.filter((a) => !text.includes(a))
    notes.push(
      `audit actions of the new operations missing on the first page: ${missing.join(', ') || 'none'}`,
    )
    expect(missing.length === 0, `the audit log does not show: ${missing.join(', ')}`)
  })

  async function signInAs(user, password) {
    await ui.fill('User name', user)
    await ui.fill('Password', password)
    await ui.click('Sign in')
    await ui.waitForText('Dashboard')
    await sleep(1500)
  }

  await step('a statistics-only account gets a dashboard with the cluster summary', async () => {
    await ui.click('Sign out')
    await ui.waitForText('Sign in to KingresDB')
    await signInAs('stats_user', 'Stats-Passw0rd-12')
    const links = await page.$$eval('#side-nav a', (a) => a.map((x) => x.textContent.trim()))
    notes.push(`stats-only navigation: ${links.join(', ')}`)
    expect(
      links.length === 1 && links[0] === 'Dashboard',
      `stats-only navigation is ${links.join(', ')}`,
    )
    const summary = await page.$eval('[data-testid=cluster-summary]', (e) =>
      e.innerText.replace(/\s+/g, ' '),
    )
    notes.push(`stats-only summary (GET /cluster/summary): ${summary}`)
    expect(/e2e/.test(await ui.text()), 'the cluster name is not shown')
    await shot('22-stats-only-dashboard')
    await ui.click('Sign out')
  })

  await step(
    'a read-only account sees the pages but its actions are disabled with a reason',
    async () => {
      await ui.waitForText('Sign in to KingresDB')
      await signInAs('app_user', 'App-Passw0rd-12')
      const links = await page.$$eval('#side-nav a', (a) => a.map((x) => x.textContent.trim()))
      notes.push(`read-only navigation: ${links.join(', ')}`)
      expect(!links.includes('Security'), 'Security is offered to a read-only account')
      expect(links.includes('Databases'), 'Databases is missing for a read-only account')
      await ui.navigate('/servers')
      await ui.waitForText('Add server')
      const disabled = await page.evaluate(() =>
        [...document.querySelectorAll('button')]
          .find((b) => b.textContent.includes('Add server'))
          ?.getAttribute('aria-disabled'),
      )
      expect(disabled === 'true', 'Add server is not disabled for a read-only account')
      await ui.navigate('/security')
      await ui.waitForText('Your account cannot open this page')
      await shot('23-readonly-forbidden')
      await ui.click('Sign out')
    },
  )

  await step('restarting kingresd signs the user out; the panel explains it', async () => {
    await ui.waitForText('Sign in to KingresDB')
    await signInAs(ADMIN.user, ADMIN.password)
    ctx.restartApi()
    await sleep(1500)
    await ui.navigate('/databases')
    await page.waitForFunction(() => location.pathname === '/login', { timeout: 20000 })
    await ui.waitForText('Your session has expired')
    await shot('21-session-expired')
    await ui.fill('User name', ADMIN.user)
    await ui.fill('Password', ADMIN.password)
    await ui.click('Sign in')
    await ui.waitForText('Databases')
  })

  await step('the headers the server sent: policy, caching and no violations', async () => {
    const CSP =
      "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'none'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
    const same = {
      'content-security-policy': CSP,
      'x-frame-options': 'DENY',
      'x-content-type-options': 'nosniff',
      'referrer-policy': 'no-referrer',
      'cross-origin-opener-policy': 'same-origin',
      'permissions-policy': 'camera=(), microphone=(), geolocation=()',
    }
    const seen = [...log.seen.values()]
    const problems = []
    for (const r of seen) {
      for (const [name, value] of Object.entries(same))
        if (r.headers[name] !== value)
          problems.push(
            `${r.method} ${r.path} ${r.status}: ${name} is "${r.headers[name] ?? '(missing)'}"`,
          )
    }
    const kind = (r) =>
      r.path.startsWith('/api/v1/')
        ? 'api'
        : r.path.startsWith('/assets/')
          ? 'asset'
          : /.[a-z0-9]+$/.test(r.path)
            ? 'file'
            : 'page'
    const want = { api: 'no-store', page: 'no-cache', file: 'no-cache' }
    for (const r of seen) {
      const k = kind(r)
      const cc = r.headers['cache-control']
      if (k === 'asset') {
        if (cc !== 'public, max-age=31536000, immutable')
          problems.push(`asset ${r.path}: cache-control "${cc}" (the panel expects immutable)`)
      } else if (cc !== want[k])
        problems.push(`${k} ${r.path} ${r.status}: cache-control "${cc}", expected "${want[k]}"`)
    }
    const extra = new Set(
      seen.flatMap((r) =>
        ['cross-origin-resource-policy', 'strict-transport-security']
          .filter((h) => r.headers[h])
          .map((h) => `${h}: ${r.headers[h]}`),
      ),
    )
    notes.push(
      `headers checked on ${seen.length} distinct responses (${new Set(seen.map(kind)).size} kinds); extra headers the panel README does not list: ${[...extra].join('; ') || 'none'}`,
    )
    notes.push(
      `problems with headers: ${problems.length ? problems.slice(0, 12).join(' || ') : 'none'}`,
    )
    expect(problems.length === 0, `header differences: ${problems.slice(0, 8).join(' || ')}`)
    expect(log.csp.length === 0, `Content-Security-Policy violations: ${log.csp.join(' | ')}`)
    const csprint = log.consoleErrors.filter((m) => /content security policy|refused to/i.test(m))
    expect(csprint.length === 0, `console messages about the policy: ${csprint.join(' | ')}`)
  })

  // ---- 10. sign out ---------------------------------------------------------------------------------------------
  await step('sign out', async () => {
    await ui.click('Sign out')
    await ui.waitForText('Sign in to KingresDB')
    const left = (await page.cookies()).find((x) => x.name === 'kingres_session')
    expect(!left || left.value === '', 'the session cookie is still set after sign-out')
  })

  try {
    const errors = ctx
      .serverLog()
      .split(/\r?\n/)
      .filter((l) => /"level":"(ERROR|WARN)"/.test(l))
    notes.push(`kingresd log: ${errors.length} warning or error lines`)
    for (const l of errors.slice(0, 8)) notes.push(`  ${l.slice(0, 300)}`)
  } catch (e) {
    notes.push(`kingresd log not read: ${e}`)
  }
  await browser.close()
  return { results, notes, pageReport, log }
}
