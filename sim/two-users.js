// two-users.js — the README's two-user demo, driven through real browsers.
//
// Two headless Chrome contexts open the same sheet as two people would: user A
// clicks a cell, types a value and a formula through the actual keyboard path;
// user B joins later, sees both arrive over SSE, then bolds the cell, fills it
// red and sets a currency format through the real toolbar. Both pages are
// screenshotted at the end.
//
// The point it demonstrates: what goes on the wire is the window as it now is,
// so a viewer that was not there for the edit still converges — and presence
// (the avatar chips, the "Brisk Koala" cursor label) rides the same pushes.
//
// Requires: a server on 127.0.0.1:8099 (run one first), node, and Chrome:
//     cd sim && npm install --no-audit --no-fund && node two-users.js
// Screenshots land beside the script. Set BASE_URL to point at another
// server, CHROME to override the browser binary.

const { chromium } = require('playwright-core');

const BASE = process.env.BASE_URL || 'http://127.0.0.1:8099';
const EXE = process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const sleep = ms => new Promise(r => setTimeout(r, ms));
const cell = (page, id) => page.evaluate(id => {
  const el = document.getElementById(id);
  return el ? { cls: el.className, text: el.textContent } : null;
}, id);

(async () => {
  // A fresh sheet, so the screenshots tell one story from a blank grid.
  const created = await fetch(BASE + '/sheets', { method: 'POST', redirect: 'manual' });
  const URL = BASE + created.headers.get('location');
  console.log('sheet:', URL);

  const browser = await chromium.launch({ executablePath: EXE, headless: true, args: ['--no-sandbox'] });

  // ── User A: C4 = 42, D4 = =C4*2 ──
  const a = await (await browser.newContext({ viewport: { width: 1280, height: 900 } })).newPage();
  await a.goto(URL, { waitUntil: 'domcontentloaded' });
  await a.waitForSelector('#b', { timeout: 15000 });
  await sleep(3000);

  // An empty sheet renders no cell elements, so aim by geometry: the gutter
  // number gives the row, the column rules (first covers the gutter) the column.
  const pt = await a.evaluate(() => {
    const n4 = document.getElementById('n4').getBoundingClientRect();
    const rules = [...document.querySelectorAll('#b>i')].filter(e => !e.className);
    const rc = rules[3].getBoundingClientRect(); // column C
    return { x: rc.x + rc.width / 2, y: n4.y + n4.height / 2 };
  });
  await a.mouse.click(pt.x, pt.y);
  await sleep(1000);
  await a.keyboard.type('42');
  await a.keyboard.press('Enter');
  await sleep(1200);
  await a.mouse.click(pt.x + 96, pt.y); // one column over: D4
  await sleep(1000);
  await a.keyboard.type('=C4*2');
  await a.keyboard.press('Enter');
  await sleep(1500);
  console.log('A sees C4:', JSON.stringify(await cell(a, 'C4')), 'D4:', JSON.stringify(await cell(a, 'D4')));

  // ── User B joins late, sees both values arrive over SSE ──
  const b = await (await browser.newContext({ viewport: { width: 1280, height: 900 } })).newPage();
  await b.goto(URL, { waitUntil: 'domcontentloaded' });
  await b.waitForSelector('#b', { timeout: 15000 });
  await sleep(3500);
  console.log('B sees C4:', JSON.stringify(await cell(b, 'C4')), 'D4:', JSON.stringify(await cell(b, 'D4')));

  // ── User B formats D4 through the toolbar: bold, red fill, currency ──
  await b.click('#D4');
  await sleep(1200);
  await b.getByRole('button', { name: 'bold' }).click();
  await sleep(800);
  await b.getByLabel('fill colour').fill('#ff0000');
  await sleep(800);
  await b.getByLabel('number format').selectOption('currency');
  await sleep(2000);
  console.log('B formatted D4:', JSON.stringify(await cell(b, 'D4')));
  await sleep(1000);
  console.log('A sees D4 now:', JSON.stringify(await cell(a, 'D4')));

  await a.screenshot({ path: 'user-a.png' });
  await b.screenshot({ path: 'user-b.png' });
  console.log('screenshots saved: sim/user-a.png sim/user-b.png');
  await browser.close();
})().catch(e => { console.error('FATAL', e.message); process.exit(1); });
