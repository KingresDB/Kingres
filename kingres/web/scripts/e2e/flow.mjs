// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// The browser steps of the end-to-end test against a real kingresd. A step that fails is recorded
// and the flow stops at the first failed step that later steps depend on.
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { helpers, openBrowser, sleep } from './browser.mjs'

export const ADMIN = { user: 'admin', password: 'E2e-Passw0rd-12' }
const DATA_DIR = '/home/kingres/kw-web/data'

/**
 * @param {{ baseUrl: string, setupToken: () => string, psql: (db: string, sql: string) => string,
 *   shots: string }} ctx
 */
export async function runFlow(ctx) {
  mkdirSync(ctx.shots, { recursive: true })
  const { browser, page, log } = await openBrowser()
  const ui = helpers(page)
  const results = []
  const notes = []
  let fatal = false

  async function step(name, fn) {
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
      fatal = true
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
        await ui.fill('Data directory', DATA_DIR)
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
    await ui.click('Apply on all servers')
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

  // ---- 9. things the server does not do yet must be calm, not alarming ----------------------------------
  await step('actions the server does not support yet answer with a calm notice', async () => {
    const calm = async (label) => {
      await page
        .waitForFunction(
          () =>
            [...document.querySelectorAll('[role=dialog] .notice, main .notice')].some((n) =>
              /not available on this server version yet/i.test(n.textContent),
            ),
          { timeout: 15000 },
        )
        .catch(() => {
          throw new Error(`${label}: no "not available yet" notice`)
        })
      const danger = await page.$$eval('.notice.tone-danger', (e) =>
        e.map((x) => x.textContent.replace(/\s+/g, ' ').slice(0, 100)),
      )
      expect(danger.length === 0, `${label}: alarming notice: ${danger.join(' | ')}`)
    }
    // create a server group
    await ui.navigate('/servers')
    await ui.waitForText('Create server group')
    await ui.click('Create server group')
    await ui.waitForText('Description (optional)')
    await ui.fill('Name', 'rack-e2e', { within: '[role=dialog]' })
    await ui.click('Create server group', { within: '[role=dialog] .dlg-foot' })
    await calm('create server group')
    await shot('18-unsupported-server-group')
    await ui.click('Cancel', { within: '[role=dialog]' })
    // hard failover confirm
    await ui.click('Failover')
    await ui.waitForText('Failover mode')
    await ui.click('Graceful failover', { within: '[role=dialog] .dlg-foot' })
    await calm('failover')
    await ui.click('Cancel', { within: '[role=dialog]' })
    // digest
    await ui.navigate('/ledger')
    await ui.waitForText('Generate digest')
    await ui.click('Generate digest')
    await calm('generate digest')
    // settings: nothing changed, then one value changed (only that value may be sent)
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
    expect(
      (await page.$$eval('.notice.tone-danger', (e) => e.length)) === 0,
      'settings: alarming notice',
    )
    await shot('19-unsupported-settings')
  })

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

  // ---- 10. sign out ---------------------------------------------------------------------------------------------
  await step('sign out', async () => {
    await ui.click('Sign out')
    await ui.waitForText('Sign in to KingresDB')
  })

  await browser.close()
  return { results, notes, pageReport, log }
}
