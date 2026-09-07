/*
 * ALL SHARE UI probe.
 *
 * Opens the real client from a real file:// URL and drives the parts of the
 * interface that no network is needed for: the hidden developer mode, the
 * service-address policy, and the settings screen in both colour schemes.
 *
 * It exists because these are exactly the places where a bug is invisible to a
 * unit test — a panel that throws on open, a control that looks like a button
 * when it should not, a reveal gesture that fires on an accidental click.
 * Results come back as JSON on stdout for the Go test to assert on.
 */
'use strict';

const path = require('path');
const { chromium } = require('playwright-core');

const CHROME = process.env.ALLSHARE_CHROME ||
  '/opt/pw-browsers/chromium-1194/chrome-linux/chrome';
const CLIENT = process.env.ALLSHARE_CLIENT ||
  path.resolve(__dirname, '..', '..', 'client', 'index.html');

const out = { checks: [], errors: [], ok: false };
function check(name, pass, detail) {
  out.checks.push({ name, pass: !!pass, detail: detail === undefined ? null : String(detail) });
  if (!pass) out.errors.push(name + (detail === undefined ? '' : ': ' + detail));
}

/** Open a page with a clean profile, capturing anything the page throws. */
async function openClient(browser, scheme) {
  const context = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    colorScheme: scheme || 'dark'
  });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (err) => thrown.push(String(err)));
  page.on('console', (msg) => { if (msg.type() === 'error') thrown.push('console: ' + msg.text()); });
  await page.goto('file://' + CLIENT);
  await page.waitForFunction(() => window.AllShare && window.AllShare.app && window.AllShare.app.home);
  return { context, page, thrown };
}

/** Put the app past first-run without going through the network. */
async function configure(page) {
  await page.evaluate(() => {
    window.AllShare.Store.set('rendezvous', 'wss://example.invalid/rv');
    window.AllShare.app.home.render();
  });
  await page.waitForTimeout(300);
}

async function checkDeveloperMode(browser) {
  const { context, page, thrown } = await openClient(browser);
  try {
    await configure(page);
    await page.evaluate(() => window.AllShare.SettingsUI.open(window.AllShare.app, 'general'));
    await page.waitForSelector('.about', { timeout: 5000 });

    const tabs = () => page.$$eval('.modal__tab', (n) => n.map((e) => e.textContent));
    check('developer tab is hidden by default', !(await tabs()).includes('Developer'));

    const cursor = await page.$eval('.about', (n) => getComputedStyle(n).cursor);
    check('the version line does not look clickable', cursor === 'default', cursor);

    // Six taps must do nothing. A user who taps a few times out of curiosity
    // should never end up somewhere they did not intend to be.
    for (let i = 0; i < 6; i++) await page.click('.about');
    check('six taps do not reveal it',
      (await page.evaluate(() => window.AllShare.Store.get('developerMode'))) === false);

    await page.click('.about');
    check('the seventh tap reveals it',
      (await page.evaluate(() => window.AllShare.Store.get('developerMode'))) === true);

    await page.waitForSelector('.devlog', { timeout: 5000 });
    check('the developer tab is added', (await tabs()).includes('Developer'));
    check('exactly one modal is open', (await page.$$eval('.modal', (n) => n.length)) === 1);

    const log = await page.textContent('.devlog');
    check('the log panel has content', log.trim().length > 0);

    const internals = await page.$$eval('.rowlist__sub', (n) => n.map((e) => e.textContent));
    check('the internals list is populated', internals.length >= 8, internals.length + ' rows');

    // No secret may appear here. The device *public* key is expected; a private
    // key or a pairing code is not.
    const panelText = await page.textContent('.modal__panel:not([hidden])');
    check('no private key material is shown', !/private|BEGIN |secret/i.test(panelText));

    await page.click('text=Hide developer tools');
    await page.waitForTimeout(300);
    check('it can be turned off again',
      (await page.evaluate(() => window.AllShare.Store.get('developerMode'))) === false);
    check('the developer tab goes away', !(await tabs()).includes('Developer'));

    // A run of taps must not accumulate across a pause, or a click today plus
    // another next week would eventually add up to a reveal.
    for (let i = 0; i < 4; i++) await page.click('.about');
    await page.waitForTimeout(3300);
    for (let i = 0; i < 3; i++) await page.click('.about');
    check('a stale run of taps does not accumulate',
      (await page.evaluate(() => window.AllShare.Store.get('developerMode'))) === false);

    check('the page threw nothing', thrown.length === 0, thrown.join(' | '));
  } finally {
    await context.close();
  }
}

async function checkServiceAddressPolicy(browser) {
  const { context, page, thrown } = await openClient(browser);
  try {
    // The address policy has to hold in the browser, not just in the unit test:
    // an unencrypted address would put the SDP and relay credentials in clear.
    const verdicts = await page.evaluate(() => {
      const U = window.AllShare.Util;
      return {
        wss: U.serviceAddressProblem('wss://rv.example.com/rv'),
        wsPublic: U.serviceAddressProblem('ws://rv.example.com/rv'),
        wsLan: U.serviceAddressProblem('ws://192.168.1.10:8080/rv'),
        wsLoopback: U.serviceAddressProblem('ws://127.0.0.1:8443/rv')
      };
    });
    check('wss is accepted', verdicts.wss === '', verdicts.wss);
    check('ws to a public host is refused', verdicts.wsPublic !== '');
    check('ws to a LAN host is accepted', verdicts.wsLan === '', verdicts.wsLan);
    check('ws to loopback is accepted', verdicts.wsLoopback === '', verdicts.wsLoopback);

    // And the connection code must refuse it too, not only the settings screen:
    // an address can arrive from a shipped config.js that never passed through
    // the form.
    const state = await page.evaluate(async () => {
      const rv = new window.AllShare.Rendezvous();
      rv.start('ws://rv.example.com/rv');
      await new Promise((r) => setTimeout(r, 200));
      return { state: rv.state, message: rv.lastError && rv.lastError.message };
    });
    check('the connection code refuses an unencrypted address', state.state === 'failed', state.state);
    check('and says why in plain language',
      !!state.message && !/ws:|protocol|scheme/i.test(state.message), state.message);

    check('the page threw nothing', thrown.length === 0, thrown.join(' | '));
  } finally {
    await context.close();
  }
}

async function checkSessionMenus(browser) {
  // Every popover in the session toolbar, opened and measured.
  //
  // These are pure-CSS failures that no assertion about the DOM would catch:
  // the title and description in a choice row are both spans, so without an
  // explicit display they render as one run-on line — "Ctrl + Alt + DeleteOpens
  // the Windows security screen". The check is geometric: the description must
  // start below the title, not beside it.
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (err) => thrown.push(String(err)));
  try {
    await page.goto('file://' + CLIENT);
    await page.waitForFunction(() => window.AllShare && window.AllShare.app && window.AllShare.app.sessionUI);
    await page.evaluate(() => {
      const app = window.AllShare.app;
      app.home.hide();
      app.sessionUI.root.hidden = false;
      const overlay = document.querySelector('[data-el="overlay"]');
      if (overlay) overlay.hidden = true;
    });

    const menus = {
      'send-to-PC': 'showSendKeysMenu',
      quality: 'showQualityMenu',
      clipboard: 'showClipboardMenu'
    };
    for (const [label, method] of Object.entries(menus)) {
      const result = await page.evaluate(async (name) => {
        const ui = window.AllShare.app.sessionUI;
        ui[name]();
        await new Promise((r) => setTimeout(r, 120));
        const rows = Array.from(document.querySelectorAll('.choice'));
        const measured = rows.map((row) => {
          const title = row.querySelector('.choice__title');
          const desc = row.querySelector('.choice__desc');
          if (!title || !desc) return { stacked: true, text: title ? title.textContent : '' };
          const t = title.getBoundingClientRect();
          const d = desc.getBoundingClientRect();
          return { stacked: d.top >= t.bottom - 1, text: title.textContent };
        });
        // Nothing may overflow the popover.
        const popover = document.querySelector('.popover');
        const box = popover ? popover.getBoundingClientRect() : null;
        const overflows = box
          ? rows.some((r) => {
              const b = r.getBoundingClientRect();
              return b.right > box.right + 1 || b.left < box.left - 1;
            })
          : false;
        ui._closePopover();
        return { rows: measured, overflows, count: rows.length, onScreen: !!box && box.top >= 0 && box.bottom <= window.innerHeight };
      }, method);

      check(label + ' menu has rows', result.count > 0, String(result.count));
      check(label + ' menu fits on screen', result.onScreen);
      check(label + ' menu does not overflow its popover', !result.overflows);
      for (const row of result.rows) {
        check(label + ': "' + row.text.slice(0, 28) + '" reads on two lines', row.stacked);
      }
    }

    check('the page threw nothing', thrown.length === 0, thrown.join(' | '));
  } finally {
    await context.close();
  }
}

async function checkSettingsPersist(browser) {
  // Settings must survive a reload, and must survive storage being unavailable.
  // A Chromebook in a restricted profile throws on localStorage rather than
  // returning null, and an app that dies there is an app that will not open for
  // the people most likely to be handed a managed device.
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (err) => thrown.push(String(err)));
  try {
    await page.goto('file://' + CLIENT);
    await page.waitForFunction(() => window.AllShare && window.AllShare.Store);

    const identityBefore = await page.evaluate(async () => {
      await new Promise((r) => setTimeout(r, 300));
      return window.AllShare.Identity.deviceId;
    });

    await page.evaluate(() => {
      const S = window.AllShare.Store;
      S.set('rendezvous', 'wss://example.invalid/rv');
      S.set('preset', 'gaming');
      S.set('maxFps', 90);
      S.set('latencyMode', 'balanced');
      S.set('clipboardToRemote', false);
      S.set('theme', 'light');
    });
    await page.waitForTimeout(200);
    await page.reload();
    await page.waitForFunction(() => window.AllShare && window.AllShare.Store);
    await page.waitForTimeout(400);

    const after = await page.evaluate(() => {
      const S = window.AllShare.Store;
      return {
        rendezvous: S.get('rendezvous'),
        preset: S.get('preset'),
        maxFps: S.get('maxFps'),
        latencyMode: S.get('latencyMode'),
        clipboardToRemote: S.get('clipboardToRemote'),
        theme: S.get('theme'),
        deviceId: window.AllShare.Identity.deviceId
      };
    });
    check('the service address survives a reload', after.rendezvous === 'wss://example.invalid/rv', after.rendezvous);
    check('the streaming preset survives a reload', after.preset === 'gaming', after.preset);
    check('a numeric setting survives as a number', after.maxFps === 90, typeof after.maxFps + ' ' + after.maxFps);
    check('the latency mode survives a reload', after.latencyMode === 'balanced', after.latencyMode);
    check('a false switch survives as false', after.clipboardToRemote === false, String(after.clipboardToRemote));
    check('the theme survives a reload', after.theme === 'light', after.theme);

    // The device identity is the credential a PC pairs with. If it changed on
    // reload, every paired PC would stop recognising this device.
    check('the device identity survives a reload',
      !!after.deviceId && after.deviceId === identityBefore,
      identityBefore + ' -> ' + after.deviceId);

    check('the page threw nothing', thrown.length === 0, thrown.join(' | '));
  } finally {
    await context.close();
  }
}

async function checkStorageFailureIsSurvivable(browser) {
  // localStorage that throws on every access, as a locked-down profile does.
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (err) => thrown.push(String(err)));
  try {
    await page.addInitScript(() => {
      const boom = () => { throw new DOMException('denied', 'SecurityError'); };
      Object.defineProperty(window, 'localStorage', {
        configurable: true,
        get() { return { getItem: boom, setItem: boom, removeItem: boom, clear: boom }; }
      });
    });
    await page.goto('file://' + CLIENT);
    await page.waitForFunction(() => window.AllShare && window.AllShare.Store, { timeout: 5000 })
      .catch(() => {});
    await page.waitForTimeout(500);

    const state = await page.evaluate(() => ({
      loaded: !!(window.AllShare && window.AllShare.Store),
      preset: window.AllShare && window.AllShare.Store && window.AllShare.Store.get('preset'),
      wrote: (() => {
        try { window.AllShare.Store.set('preset', 'gaming'); return true; } catch (e) { return String(e); }
      })()
    }));
    check('the app still starts when storage is unavailable', state.loaded === true);
    check('and falls back to defaults', state.preset === 'balanced' || state.preset === 'gaming', String(state.preset));
    check('and writing does not throw', state.wrote === true, String(state.wrote));
    check('the page threw nothing', thrown.length === 0, thrown.join(' | '));
  } finally {
    await context.close();
  }
}

async function checkOldBrowserRefusal(browser) {
  // A browser without Ed25519 must be told so on the first screen, not left to
  // fail with a bare NotSupportedError when it generates its identity. The only
  // honest way to test this is to take the algorithm away.
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  try {
    await page.addInitScript(() => {
      const real = crypto.subtle.generateKey.bind(crypto.subtle);
      crypto.subtle.generateKey = function (algorithm) {
        const name = algorithm && (algorithm.name || algorithm);
        if (String(name).toLowerCase() === 'ed25519') {
          return Promise.reject(new DOMException('Unrecognized name.', 'NotSupportedError'));
        }
        return real.apply(null, arguments);
      };
    });
    await page.goto('file://' + CLIENT);
    await page.waitForFunction(() => /cannot run in this browser/i.test(document.body.textContent), { timeout: 5000 })
      .catch(() => {});

    const body = await page.textContent('body');
    check('an old browser is refused up front', /cannot run in this browser/i.test(body), body.slice(0, 120));
    check('the message names the real cause', /Ed25519/.test(body));
    check('and says how to fix it', /137|update/i.test(body));
  } finally {
    await context.close();
  }
}

async function checkSettingsRender(browser) {
  // Every settings tab must build without throwing, in both colour schemes.
  // A panel that throws on open takes the whole modal with it.
  for (const scheme of ['dark', 'light']) {
    const { context, page, thrown } = await openClient(browser, scheme);
    try {
      await configure(page);
      await page.evaluate(() => window.AllShare.Store.set('developerMode', true));
      for (const tab of ['general', 'streaming', 'input', 'sound', 'advanced', 'developer']) {
        await page.evaluate((id) => window.AllShare.SettingsUI.open(window.AllShare.app, id), tab);
        await page.waitForTimeout(150);
        const visible = await page.$$eval('.modal__panel:not([hidden])', (n) => n.length);
        check(scheme + ': the ' + tab + ' tab opens', visible === 1, visible + ' panels visible');
      }
      check(scheme + ': the page threw nothing', thrown.length === 0, thrown.join(' | '));
    } finally {
      await context.close();
    }
  }
}

async function main() {
  const browser = await chromium.launch({
    executablePath: CHROME,
    headless: true,
    args: ['--no-sandbox']
  });
  try {
    await checkDeveloperMode(browser);
    await checkServiceAddressPolicy(browser);
    await checkSessionMenus(browser);
    await checkSettingsPersist(browser);
    await checkStorageFailureIsSurvivable(browser);
    await checkOldBrowserRefusal(browser);
    await checkSettingsRender(browser);
  } finally {
    await browser.close();
  }
  out.ok = out.errors.length === 0;
}

main()
  .catch((err) => { out.errors.push('probe crashed: ' + String(err)); })
  .then(() => {
    process.stdout.write(JSON.stringify(out, null, 2) + '\n');
    process.exit(out.ok ? 0 : 1);
  });
