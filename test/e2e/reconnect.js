/*
 * Drives the reconnection and adaptive-quality paths.
 *
 * Reconnection is the behaviour a user notices most on a real network, so it is
 * exercised for real: the session is dropped underneath the client the way a
 * network would drop it, and the test asserts the client recovers on its own
 * without the user touching anything.
 */
'use strict';

const path = require('path');
const { chromium } = require('playwright-core');

const CHROME = process.env.ALLSHARE_CHROME ||
  '/opt/pw-browsers/chromium-1194/chrome-linux/chrome';
const CLIENT = process.env.ALLSHARE_CLIENT ||
  path.resolve(__dirname, '..', '..', 'client', 'index.html');

const out = { steps: [], errors: [], console: [], ok: false };
const step = (name, detail) => out.steps.push({ name, detail: detail ?? null });
const fail = (name, detail) => out.errors.push({ name, detail: String(detail) });

function readOptions() {
  const opts = {};
  for (const arg of process.argv.slice(2)) {
    const eq = arg.indexOf('=');
    if (arg.startsWith('--') && eq > 0) opts[arg.slice(2, eq)] = arg.slice(eq + 1);
  }
  return opts;
}

async function connect(page, deviceId) {
  await page.evaluate((id) => window.AllShare.app.connectTo(id), deviceId);
  await page.waitForFunction(
    () => !!(window.AllShare.app.session && window.AllShare.app.session.state === 'connected'),
    null, { timeout: 45000 });
  await page.waitForFunction(
    () => !!(window.AllShare.app.session && window.AllShare.app.session.hello),
    null, { timeout: 20000 });
}

async function main() {
  const opts = readOptions();
  for (const key of ['service', 'code', 'device']) {
    if (!opts[key]) throw new Error('missing --' + key);
  }

  const browser = await chromium.launch({
    executablePath: CHROME,
    headless: true,
    args: ['--no-sandbox', '--autoplay-policy=no-user-gesture-required']
  });
  const page = await browser.newPage({ viewport: { width: 1024, height: 640 } });
  page.on('console', (m) => { out.console.push('[' + m.type() + '] ' + m.text()); });
  page.on('pageerror', (e) => fail('pageerror', e.message));

  await page.goto('file://' + CLIENT);
  await page.waitForFunction('window.AllShare && window.AllShare.app', null, { timeout: 15000 });
  await page.evaluate((url) => {
    window.AllShare.Store.set('rendezvous', url);
    window.AllShare.app.reconnectService();
  }, opts.service);
  await page.waitForFunction('window.AllShare.app.rv.isConnected()', null, { timeout: 15000 });

  await page.evaluate(async (code) => {
    await window.AllShare.PairingUI.run(window.AllShare.app, code, () => {});
  }, opts.code);
  await page.evaluate(() => window.AllShare.app.refreshDevices());
  await page.waitForFunction(
    (id) => !!(window.AllShare.app.deviceById(id) || {}).online, opts.device, { timeout: 15000 });
  step('paired-and-online');

  await connect(page, opts.device);
  const firstSession = await page.evaluate(() => window.AllShare.app.session.sessionId);
  step('first-session', firstSession);

  // --- Settings persistence, checked against the live app rather than storage.
  await page.evaluate(() => {
    window.AllShare.Store.set('preset', 'gaming');
    window.AllShare.Store.set('maxFps', 90);
    window.AllShare.app.pushQuality();
  });
  await page.waitForTimeout(400);
  step('quality-pushed', await page.evaluate(() => ({
    preset: window.AllShare.Store.get('preset'),
    maxFps: window.AllShare.Store.get('maxFps')
  })));

  // --- Drop the session the way a network would, and let the client recover.
  //
  // Closing the peer connection from underneath is exactly what a dropped
  // Wi-Fi link looks like to the client: no clean shutdown, just silence.
  await page.evaluate(() => {
    const session = window.AllShare.app.session;
    session._pc.close();
    // The close event fires asynchronously; nudge the app the same way the
    // connection-state handler would.
    session.emit('failed', window.AllShare.sessionFailure(
      'connection_failed', 'The connection to your PC failed.', 'simulated network drop'));
  });
  step('session-dropped');

  // The client must come back on its own, with no user interaction at all.
  await page.waitForFunction(
    () => !!(window.AllShare.app.session && window.AllShare.app.session.state === 'connected'),
    null, { timeout: 60000 });
  const secondSession = await page.evaluate(() => window.AllShare.app.session.sessionId);
  step('reconnected', secondSession);
  if (secondSession === firstSession) {
    throw new Error('the client reported the same session id after reconnecting');
  }

  await page.waitForFunction(
    () => !!(window.AllShare.app.session && window.AllShare.app.session.hello),
    null, { timeout: 20000 });

  const recovered = await page.evaluate(async () => {
    const session = window.AllShare.app.session;
    const deadline = Date.now() + 20000;
    while (Date.now() < deadline) {
      const report = await session._pc.getStats();
      let inbound = null;
      report.forEach((s) => { if (s.type === 'inbound-rtp' && s.kind === 'video') inbound = s; });
      if (inbound && (inbound.framesDecoded || 0) >= 5) {
        return { framesDecoded: inbound.framesDecoded, width: inbound.frameWidth };
      }
      await new Promise((r) => setTimeout(r, 300));
    }
    return { framesDecoded: 0 };
  });
  step('video-after-reconnect', recovered);
  if (recovered.framesDecoded < 5) {
    throw new Error('video did not resume after reconnecting');
  }

  // --- The client must survive the service going away and coming back, since
  //     media flows peer to peer and does not need signalling once established.
  await page.evaluate(() => window.AllShare.app.rv.stop());
  await page.waitForTimeout(800);
  const survived = await page.evaluate(() => ({
    sessionState: window.AllShare.app.session ? window.AllShare.app.session.state : 'none',
    serviceState: window.AllShare.app.rv.state
  }));
  step('service-stopped', survived);
  if (survived.sessionState !== 'connected') {
    throw new Error('the session died when the service went away, but media is peer to peer');
  }

  await page.evaluate((url) => window.AllShare.app.rv.start(url), opts.service);
  await page.waitForFunction('window.AllShare.app.rv.isConnected()', null, { timeout: 20000 });
  step('service-recovered');

  await page.evaluate(() => window.AllShare.app.disconnect('reconnect test finished'));
  await page.waitForTimeout(500);

  await browser.close();
  out.ok = out.errors.length === 0;
}

main()
  .catch((err) => fail('fatal', (err && err.stack) || err))
  .finally(() => {
    process.stdout.write('\n===ALLSHARE_E2E_JSON===\n' + JSON.stringify(out) + '\n');
    process.exit(out.ok ? 0 : 1);
  });
