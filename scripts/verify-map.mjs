// Optional browser acceptance; uses an explicitly supplied temporary Playwright
// installation and the isolated TestMapPreview server, never the user's DB.
// COCKPIT_PLAYWRIGHT_MODULE=/tmp/.../node_modules/playwright/index.mjs node scripts/verify-map.mjs
import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

const base = process.env.COCKPIT_MAP_BASE_URL || 'http://127.0.0.1:8766';
assert(['127.0.0.1', '[::1]'].includes(new URL(base).hostname), 'Acceptance is restricted to loopback previews');
const modulePath = process.env.COCKPIT_PLAYWRIGHT_MODULE;
assert(modulePath, 'Set COCKPIT_PLAYWRIGHT_MODULE to an isolated Playwright installation');
const { chromium } = await import(pathToFileURL(modulePath).href);
const evidence = process.env.COCKPIT_MAP_EVIDENCE || '/tmp/cockpit-map-browser-evidence';
await mkdir(evidence, { recursive: true });
const browser = await chromium.launch({
  executablePath: process.env.COCKPIT_CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  headless: true,
});
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
await context.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort());
const page = await context.newPage();
const scriptErrors = [];
page.on('pageerror', error => scriptErrors.push(error.message));
const form = (action, target = page) => target.locator(`form:has(input[name="action"][value="${action}"])`).first();
const field = (target, name) => target.locator(`[name="${name}"]`);
async function submit(target, expectedStatus = 200) {
  const [response] = await Promise.all([
    page.waitForNavigation(),
    target.locator('button[type="submit"], button:not([type])').first().click(),
  ]);
  assert.equal(response.status(), expectedStatus, await page.locator('main').innerText());
}
async function data(id, item = '') {
  const response = await context.request.get(`${base}/map/data?workstream=${id}&item=${item}`);
  assert.equal(response.status(), 200);
  return response.json();
}
async function addItem(kind, values) {
  const target = form('create_item');
  await field(target, 'kind').selectOption(kind);
  for (const [key, value] of Object.entries(values)) {
    const control = field(target, key);
    if (await control.evaluate(el => el.tagName === 'SELECT')) await control.selectOption(value);
    else await control.fill(value);
  }
  await submit(target);
  return new URL(page.url()).searchParams.get('item');
}

try {
  await page.goto(`${base}/map?form=new`);
  const initialCount = (await data('')).Workstreams.Total;
  const create = form('create_workstream');
  await field(create, 'name').fill('Cancelled draft');
  await create.getByRole('link', { name: 'Cancel', exact: true }).click();
  assert.equal((await data('')).Workstreams.Total, initialCount, 'Cancel saved a workstream');
  await page.goto(`${base}/map?form=new`);
  const name = `Credits browser acceptance ${Date.now()}`;
  await field(form('create_workstream'), 'name').fill(name);
  await field(form('create_workstream'), 'outcome').fill('Ship credits without increasing customer latency. Isolated acceptance fixture.');
  await field(form('create_workstream'), 'owner').fill('Ada');
  await field(form('create_workstream'), 'sponsor').fill('Platform');
  await field(form('create_workstream'), 'target_date').fill('2030-10-01');
  await submit(form('create_workstream'));
  const id = new URL(page.url()).searchParams.get('workstream');
  assert(id, 'Creation did not select the workstream');

  const task = await addItem('task', { title: 'Review credits change', tracking_state: 'blocked', blocker_reason: 'Need compatibility review', due_date: '2020-01-01' });
  const reference = await addItem('reference', { title: 'Credits RFC', source_url: 'https://example.com/credits#decision', source_label: 'RFC context' });
  const ask = await addItem('ask', { title: 'Confirm billing compatibility', counterpart: 'Billing', ask_status: 'waiting', follow_up_at: '2020-01-02T09:15', last_contact_at: '2020-01-01T10:00' });
  const signal = await addItem('signal', { title: 'Credits latency', value: '210', unit: 'ms', assessment: 'concerning', observed_at: '2026-09-20T09:00', review_by: '2030-10-01T09:00' });
  let view = await data(id, signal);
  assert.equal(view.Selected.AttentionCount, 3);
  assert.equal(view.SelectedItem.Value, '210');

  // An edit must round-trip all kind-specific context, not clear unseen fields.
  await field(form('edit_item'), 'description').fill('Observed during the pilot');
  await submit(form('edit_item'));
  view = await data(id, signal);
  assert.equal(view.SelectedItem.Value, '210');
  assert.equal(view.SelectedItem.Unit, 'ms');
  assert(view.SelectedItem.ObservedAt && view.SelectedItem.ReviewBy);

  // Local cached PR lookup is selectable, paginated, and honest when empty.
  const picker = form('create_item');
  await field(picker, 'kind').selectOption('reference');
  await field(picker, 'title').fill('Cached credits PR');
  await picker.locator('[data-map-pr-search]').fill('no-such-preview-pr-xyz');
  await picker.getByText('No matching cached PRs.', { exact: false }).waitFor();
  await picker.locator('[data-map-pr-search]').fill('octo/repo');
  await picker.getByRole('button', { name: 'Next cached PR page' }).click();
  await picker.getByRole('button', { name: 'First cached PR page' }).click();
  await picker.locator('[data-map-pr-search]').fill('octo/repo#42');
  await picker.locator('[data-map-pr-results] button').filter({ hasText: 'octo/repo#42' }).click();
  await submit(picker);
  const cachedReference = new URL(page.url()).searchParams.get('item');
  const cachedSource = (await data(id, cachedReference)).SelectedItem.Source;
  assert(cachedSource.PRID && cachedSource.LocalReviewURL.startsWith('/pr/'));

  await field(form('record_decision'), 'decision_note').fill('Keep the pilot small until latency is normal.');
  await submit(form('record_decision'));
  assert.match(await page.locator('main').innerText(), /Keep the pilot small/);

  // Graph, timeline, and Markdown are real read-only views of the same data.
  for (const tab of ['graph', 'timeline', 'obsidian']) {
    await page.goto(`${base}/map?workstream=${id}&item=${signal}&tab=${tab}&q=Credits`);
    assert.match(await page.locator('main').innerText(), /Credits latency/);
    if (tab === 'graph') {
      assert.equal(await page.locator('svg a').count(), 9); // root + 3 categories + 5 items
      await page.locator('svg a').filter({ hasText: 'Credits latency' }).first().click();
      assert.equal(new URL(page.url()).searchParams.get('item'), signal);
      assert.equal(new URL(page.url()).searchParams.get('q'), 'Credits');
      await page.goBack();
    }
    if (tab === 'obsidian') assert.match(await page.locator('pre').first().innerText(), /## Outcome/);
  }

  // Stale browser tabs must retain drafts and may not silently overwrite.
  await page.goto(`${base}/map?workstream=${id}&item=${task}`);
  const stale = await context.newPage();
  await stale.goto(page.url());
  const oldRevision = await field(form('edit_item', stale), 'expected_revision').inputValue();
  await field(form('edit_item'), 'title').fill('Review credits change — updated');
  await submit(form('edit_item'));
  await field(form('edit_item', stale), 'description').fill('Unsaved stale draft');
  const [conflict] = await Promise.all([stale.waitForNavigation(), form('edit_item', stale).locator('button[type="submit"], button:not([type])').first().click()]);
  assert.equal(conflict.status(), 409);
  assert.equal(await field(form('edit_item', stale), 'description').inputValue(), 'Unsaved stale draft');
  assert.equal(await field(form('edit_item', stale), 'expected_revision').inputValue(), oldRevision);
  await stale.close();

  // Confirmed detach and restore preserve identity.
  await page.goto(`${base}/map?workstream=${id}&item=${reference}`);
  await field(form('detach_item'), 'confirm').check();
  await submit(form('detach_item'));
  assert((await data(id, reference)).SelectedItem.DetachedAt);
  const restore = page.locator(`form:has(input[name="action"][value="restore_item"]):has(input[name="item_id"][value="${reference}"])`).first();
  await submit(restore);
  assert.equal((await data(id, reference)).SelectedItem.DetachedAt, null);

  // Move between two active parents using explicit destination controls.
  await page.goto(`${base}/map?form=new`);
  await field(form('create_workstream'), 'name').fill(`Acceptance destination ${Date.now()}`);
  await field(form('create_workstream'), 'outcome').fill('Verify atomic movement');
  await submit(form('create_workstream'));
  const destination = new URL(page.url()).searchParams.get('workstream');
  await page.goto(`${base}/map?workstream=${id}&item=${task}`);
  await field(form('move_item'), 'destination_workstream_id').selectOption(destination);
  await submit(form('move_item'));
  assert.equal(new URL(page.url()).searchParams.get('workstream'), destination);
  assert.equal((await data(destination, task)).SelectedItem.ID, task);
  await field(form('move_item'), 'destination_workstream_id').selectOption(id);
  await submit(form('move_item'));

  // Completion is confirmed, requires an override, and does not resolve leaves.
  await page.locator('summary').filter({ hasText: 'Workstream details and lifecycle' }).click();
  await field(form('complete_workstream'), 'notes').fill('Shipped a pilot with tracked follow-ups.');
  await field(form('complete_workstream'), 'confirm').check();
  await submit(form('complete_workstream'), 422);
  assert.equal(await field(form('complete_workstream'), 'notes').inputValue(), 'Shipped a pilot with tracked follow-ups.');
  await field(form('complete_workstream'), 'override_reason').fill('Accepted pilot constraints; unfinished work remains tracked.');
  await field(form('complete_workstream'), 'confirm').check();
  await submit(form('complete_workstream'));
  assert.equal((await data(id)).Selected.Lifecycle, 'completed');
  assert.equal(await page.locator('form:has(input[name="action"][value="create_item"])').count(), 0);
  await page.locator('summary').filter({ hasText: 'Workstream details and lifecycle' }).click();
  await field(form('archive_workstream'), 'confirm').check();
  await submit(form('archive_workstream'));
  assert.equal((await data(id)).Selected.Archived, true);
  await page.locator('summary').filter({ hasText: 'Workstream details and lifecycle' }).click();
  await submit(form('restore_workstream'));
  assert.equal((await data(id)).Selected.Lifecycle, 'completed');
  await page.locator('summary').filter({ hasText: 'Workstream details and lifecycle' }).click();
  await submit(form('reopen_workstream'));
  assert.equal((await data(id)).Selected.Lifecycle, 'active');
  assert.equal((await data(id)).Selected.CompletionNote, 'Shipped a pilot with tracked follow-ups.');

  // Reopening changes only the parent lifecycle. Resolve each leaf explicitly;
  // the later normal signal observation must clear the final attention reason.
  await page.goto(`${base}/map?workstream=${id}&item=${task}`);
  await field(form('edit_item'), 'tracking_state').selectOption('done');
  await submit(form('edit_item'));
  await page.goto(`${base}/map?workstream=${id}&item=${ask}`);
  await field(form('edit_item'), 'ask_status').selectOption('resolved');
  await submit(form('edit_item'));
  await page.goto(`${base}/map?workstream=${id}&item=${signal}`);
  const originalObservedAt = (await data(id, signal)).SelectedItem.ObservedAt;
  await field(form('edit_item'), 'assessment').selectOption('normal');
  await field(form('edit_item'), 'observed_at').fill('2026-09-21T10:00');
  await submit(form('edit_item'));
  view = await data(id, signal);
  assert.equal(view.Selected.TargetDate, '2030-10-01');
  assert.equal(view.Selected.AttentionCount, 0, 'Resolved leaves and a normal signal should clear attention');
  assert.equal(view.SelectedItem.Assessment, 'normal');
  assert(Date.parse(view.SelectedItem.ObservedAt) > Date.parse(originalObservedAt), 'Signal observation did not advance');

  // Use browser forms at their stated bounds. Keep the primary lifecycle
  // fixture readable and isolate hostile wrapping data in this second fixture.
  const longName = `Long fixture ${'n'.repeat(200 - 'Long fixture '.length)}`;
  const longTitle = `Long title ${'t'.repeat(190 - 'Long title '.length)}`;
  const longDescription = 'd'.repeat(32000);
  const longURLPrefix = 'https://example.test/';
  const longURL = `${longURLPrefix}${'u'.repeat(4096 - longURLPrefix.length)}`;
  await page.goto(`${base}/map?form=new`);
  await field(form('create_workstream'), 'name').fill(longName);
  await field(form('create_workstream'), 'outcome').fill('Contain layout boundary fixture.');
  await submit(form('create_workstream'));
  const longFixture = new URL(page.url()).searchParams.get('workstream');
  assert(longFixture, 'Long fixture creation did not select the workstream');
  const longReference = await addItem('reference', {
    title: longTitle,
    description: longDescription,
    source_url: longURL,
    source_label: 'Unbroken URL boundary fixture',
  });
  view = await data(longFixture, longReference);
  assert.equal(view.Selected.Name.length, 200);
  assert.equal(view.SelectedItem.Title.length, 190);
  assert.equal(view.SelectedItem.Description.length, 32000);
  assert.equal(view.SelectedItem.Source.URL.length, 4096);

  // Keyboard behavior, typing suppression, and native-dialog focus return.
  await page.goto(`${base}/map?workstream=${id}&tab=graph`);
  await page.locator('#map-detail').focus();
  await page.keyboard.press('/');
  assert.equal(await page.evaluate(() => document.activeElement.id), 'map-search');
  await page.keyboard.type('jk?');
  assert.equal(await page.locator('dialog').evaluate(el => el.open), false);
  await page.locator('#map-detail').focus();
  await page.keyboard.press('j');
  assert(await page.evaluate(() => document.activeElement.hasAttribute('data-map-row')));
  await page.keyboard.press('?');
  assert.equal(await page.locator('dialog').evaluate(el => el.open), true);
  await page.keyboard.press('Escape');
  assert.equal(await page.locator('dialog').evaluate(el => el.open), false);
  assert(await page.evaluate(() => document.activeElement.hasAttribute('data-map-row')));

  for (const width of [375, 768, 1440]) {
    await page.setViewportSize({ width, height: 1000 });
    for (const tab of ['overview', 'graph', 'obsidian']) {
      await page.goto(`${base}/map?workstream=${longFixture}&item=${longReference}&tab=${tab}`);
      const scrollWidth = await page.evaluate(() => document.documentElement.scrollWidth);
      assert(scrollWidth <= width, `Page overflow at ${width}px on ${tab} (${scrollWidth}px document width)`);
      await page.screenshot({ path: `${evidence}/${tab}-${width}.png`, fullPage: true });
    }
  }
  assert.deepEqual(scriptErrors, []);
  console.log(JSON.stringify({ outcome: 'pass', workstreamURL: `${base}/map?workstream=${id}`, fixtureIDs: { task, reference, ask, signal, longFixture, longReference }, evidence }, null, 2));
} catch (error) {
  console.error(JSON.stringify({ outcome: 'failure', url: page.url(), evidence }, null, 2));
  await page.screenshot({ path: `${evidence}/failure.png`, fullPage: true }).catch(() => {});
  throw error;
} finally {
  await browser.close();
}
