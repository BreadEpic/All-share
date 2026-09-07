/*
 * ALL SHARE end-to-end driver.
 *
 * Launches a real Chromium, opens the real client from a real file:// URL, and
 * performs the actions a user would: pair with a code, connect to the PC, move
 * the mouse, type, and disconnect. Results come back as JSON on stdout so the
 * Go test can assert on them alongside what the agent actually received.
 *
 * Nothing here stubs the product. The page under test is client/index.html
 * exactly as it ships.
 */
'use strict';

const path = require('path');
const { chromium } = require('playwright-core');

const CHROME = process.env.ALLSHARE_CHROME ||
  '/opt/pw-browsers/chromium-1194/chrome-linux/chrome';
const CLIENT = process.env.ALLSHARE_CLIENT ||
  path.resolve(__dirname, '..', '..', 'client', 'index.html');

function readOptions() {
  const opts = {};
  for (const arg of process.argv.slice(2)) {
    const eq = arg.indexOf('=');
    if (arg.startsWith('--') && eq > 0) opts[arg.slice(2, eq)] = arg.slice(eq + 1);
  }
  return opts;
}

const out = { steps: [], errors: [], console: [], ok: false };
function step(name, detail) {
  out.steps.push({ name, detail: detail === undefined ? null : detail, at: Date.now() });
}
function fail(name, detail) {
  out.errors.push({ name, detail: String(detail) });
}

async function main() {
  const opts = readOptions();
  for (const key of ['service', 'code', 'device']) {
    if (!opts[key]) throw new Error('missing --' + key);
  }

  const browser = await chromium.launch({
    executablePath: CHROME,
    headless: opts.headless !== 'false',
    args: [
      '--no-sandbox',
      '--autoplay-policy=no-user-gesture-required',
      '--use-fake-device-for-media-stream'
    ]
  });

  const context = await browser.newContext({
    viewport: { width: 1280, height: 800 },
    colorScheme: 'dark'
  });
  const page = await context.newPage();

  page.on('console', (m) => {
    out.console.push('[' + m.type() + '] ' + m.text());
    if (out.console.length > 300) out.console.shift();
  });
  page.on('pageerror', (e) => fail('pageerror', e.message + '\n' + (e.stack || '')));

  await page.goto('file://' + CLIENT);
  await page.waitForFunction(
    'window.AllShare && window.AllShare.app && window.AllShare.Identity.deviceId',
    null, { timeout: 15000 });
  step('client-loaded', await page.evaluate(() => window.AllShare.Identity.fingerprint));

  // Point the client at the test service exactly as its setup screen would.
  await page.evaluate((url) => {
    window.AllShare.Store.set('rendezvous', url);
    window.AllShare.app.reconnectService();
  }, opts.service);

  await page.waitForFunction('window.AllShare.app.rv.isConnected()', null, { timeout: 15000 });
  step('service-connected', await page.evaluate(() => window.AllShare.app.rv.serverVersion));

  // ---------------------------------------------------------------- pairing
  const pairResult = await page.evaluate(async (code) => {
    try {
      const record = await window.AllShare.PairingUI.run(window.AllShare.app, code, () => {});
      return { ok: true, name: record.name, deviceId: record.deviceId };
    } catch (err) {
      return { ok: false, error: String((err && err.message) || err), code: err && err.code };
    }
  }, opts.code);
  if (!pairResult.ok) throw new Error('pairing failed: ' + pairResult.error);
  if (pairResult.deviceId !== opts.device) {
    throw new Error('paired with the wrong PC: ' + pairResult.deviceId);
  }
  step('paired', pairResult.name);

  await page.evaluate(() => window.AllShare.app.refreshDevices());
  await page.waitForFunction(
    (id) => {
      const device = window.AllShare.app.deviceById(id);
      return !!(device && device.online && device.known);
    },
    opts.device, { timeout: 15000 });
  step('device-online');

  // ------------------------------------------------------------- connecting
  await page.evaluate((id) => window.AllShare.app.connectTo(id), opts.device);
  await page.waitForFunction(
    () => !!(window.AllShare.app.session && window.AllShare.app.session.state === 'connected'),
    null, { timeout: 45000 });
  step('session-connected');

  await page.waitForFunction(
    () => !!(window.AllShare.app.session && window.AllShare.app.session.hello),
    null, { timeout: 20000 });
  const hello = await page.evaluate(() => window.AllShare.app.session.hello);
  step('hello', hello);

  // The real assertion: the browser decoded frames off the wire.
  const decoded = await page.evaluate(async () => {
    const session = window.AllShare.app.session;
    const deadline = Date.now() + 25000;
    let best = { framesDecoded: 0 };
    while (Date.now() < deadline) {
      const report = await session._pc.getStats();
      let inbound = null;
      report.forEach((s) => { if (s.type === 'inbound-rtp' && s.kind === 'video') inbound = s; });
      if (inbound) {
        best = {
          framesDecoded: inbound.framesDecoded || 0,
          framesReceived: inbound.framesReceived || 0,
          bytesReceived: inbound.bytesReceived || 0,
          frameWidth: inbound.frameWidth || 0,
          frameHeight: inbound.frameHeight || 0,
          keyFramesDecoded: inbound.keyFramesDecoded || 0,
          decoder: inbound.decoderImplementation || '',
          freezeCount: inbound.freezeCount || 0,
          nackCount: inbound.nackCount || 0,
          packetsLost: inbound.packetsLost || 0
        };
        if (best.framesDecoded >= 20) break;
      }
      await new Promise((r) => setTimeout(r, 400));
    }
    const video = document.querySelector('[data-el="video"]');
    best.videoWidth = video.videoWidth;
    best.videoHeight = video.videoHeight;
    best.readyState = video.readyState;
    return best;
  });
  step('video-decoded', decoded);
  if (!decoded.framesDecoded || decoded.framesDecoded < 5) {
    throw new Error('the browser decoded only ' + decoded.framesDecoded + ' frames');
  }
  if (!decoded.videoWidth) throw new Error('the video element never received dimensions');

  const route = await page.evaluate(async () => {
    const report = await window.AllShare.app.session._pc.getStats();
    const byId = new Map();
    report.forEach((s) => byId.set(s.id, s));
    let pair = null;
    report.forEach((s) => {
      if (s.type === 'candidate-pair' && (s.nominated || s.state === 'succeeded')) pair = s;
    });
    if (!pair) return null;
    const local = byId.get(pair.localCandidateId);
    return {
      state: pair.state,
      localType: local ? local.candidateType : '',
      rttMs: pair.currentRoundTripTime ? pair.currentRoundTripTime * 1000 : null
    };
  });
  step('route', route);

  // -------------------------------------------------------------- input
  await page.waitForFunction(
    () => {
      const s = window.AllShare.app.session;
      return !!(s && s._input && s._input.readyState === 'open');
    },
    null, { timeout: 20000 });
  step('input-channel-open');

  const target = await page.evaluate(() => {
    const rect = window.AllShare.app.input.videoRect();
    return {
      x: Math.round(rect.left + rect.width * 0.25),
      y: Math.round(rect.top + rect.height * 0.75),
      rect: {
        left: rect.left, top: rect.top, width: rect.width, height: rect.height,
        videoWidth: rect.videoWidth, videoHeight: rect.videoHeight
      }
    };
  });
  await page.mouse.move(target.x, target.y);
  await page.waitForTimeout(150);
  await page.mouse.down();
  await page.waitForTimeout(80);
  await page.mouse.up();
  step('mouse-sent', target);

  await page.keyboard.press('KeyH');
  await page.waitForTimeout(50);
  await page.keyboard.press('KeyI');
  await page.waitForTimeout(50);
  await page.keyboard.down('Shift');
  await page.waitForTimeout(50);
  await page.keyboard.press('KeyW');
  await page.waitForTimeout(50);
  await page.keyboard.up('Shift');
  await page.waitForTimeout(50);
  await page.keyboard.press('ArrowUp');
  await page.waitForTimeout(80);
  step('keys-sent');

  await page.mouse.wheel(0, -120);
  await page.waitForTimeout(150);
  step('wheel-sent');

  // Clipboard, both the ordinary case and the oversized one. Pasting is driven
  // by a real ClipboardEvent because that is exactly how the product works —
  // there is no background clipboard read, so nothing happens unless the paste
  // event fires.
  const clipboardText = 'ALL SHARE clipboard test — ünïcödé and a tab\there';
  const oversize = 'x'.repeat(80 * 1024);
  const clipboardResult = await page.evaluate(async (payload) => {
    const results = { warned: null };
    const input = window.AllShare.app.input;
    const firePaste = function (text) {
      const data = new DataTransfer();
      data.setData('text/plain', text);
      document.dispatchEvent(new ClipboardEvent('paste', {
        clipboardData: data, bubbles: true, cancelable: true
      }));
    };
    let refused = false;
    input.on('clipboardTooLarge', function (e) { refused = true; results.warned = e.bytes; });

    firePaste(payload.text);
    await new Promise((r) => setTimeout(r, 200));
    firePaste(payload.oversize);
    await new Promise((r) => setTimeout(r, 200));
    results.refusedOversize = refused;
    return results;
  }, { text: clipboardText, oversize });
  await page.waitForTimeout(300);

  // The other direction: the agent is pushing a known string on a repeat, so
  // waiting for one is not a race. Writing it to the *local* clipboard needs a
  // focused document and a permission the headless browser may withhold, so
  // what is asserted is that the text arrived — the local write is best-effort
  // by design and the UI offers a manual paste when it fails.
  const received = await page.evaluate(async () => {
    const session = window.AllShare.app.session;
    return await new Promise((resolve) => {
      const timer = setTimeout(() => resolve(null), 8000);
      session.on('clipboard', function (body) {
        clearTimeout(timer);
        resolve(body && body.text);
      });
    });
  });
  step('clipboard-received', received);
  out.clipboard = {
    sent: clipboardText,
    refusedOversize: !!clipboardResult.refusedOversize,
    received: received
  };

  // A held key that is never released, so the test can prove the agent is told
  // to let go when the session ends rather than leaving it stuck down.
  await page.keyboard.down('KeyW');
  await page.waitForTimeout(500);
  step('key-held');

  if (opts.screenshot) {
    await page.screenshot({ path: opts.screenshot });
    step('screenshot', opts.screenshot);
  }

  // The locally drawn cursor is a headline feature — it is what makes pointer
  // movement feel instant — so the test checks the client actually received a
  // bitmap and painted something, not merely that the messages were sent.
  const cursor = await page.evaluate(async () => {
    const ui = window.AllShare.app.sessionUI;
    const deadline = Date.now() + 8000;
    while (Date.now() < deadline && ui._cursorShapes.size === 0) {
      await new Promise((r) => setTimeout(r, 200));
    }
    const canvas = document.querySelector('[data-el="cursor-canvas"]');
    let painted = 0;
    if (canvas && canvas.width && canvas.height) {
      const ctx = canvas.getContext('2d');
      const data = ctx.getImageData(0, 0, canvas.width, canvas.height).data;
      for (let i = 3; i < data.length; i += 4) if (data[i] > 0) painted++;
    }
    return {
      shapes: ui._cursorShapes.size,
      state: ui._cursorState,
      canvas: canvas ? { w: canvas.width, h: canvas.height } : null,
      paintedPixels: painted
    };
  });
  step('cursor', cursor);
  if (cursor.shapes === 0) throw new Error('no cursor bitmap ever reached the client');
  if (cursor.paintedPixels === 0) throw new Error('the client never drew the cursor');

  // End-to-end input latency, measured the way the product measures it: the
  // agent tags each frame with the input it reflects, and the browser reports
  // when that exact frame reached the screen.
  // Real pointer movement drives the measurement: each move produces a new
  // input sequence, the agent tags the next captured frame with it, and the
  // browser reports when that frame reached the screen.
  const collect = page.evaluate(async () => {
    const ui = window.AllShare.app.sessionUI;
    const samples = [];
    const seen = new Set();
    const deadline = Date.now() + 8000;
    while (Date.now() < deadline && samples.length < 40) {
      await new Promise((r) => setTimeout(r, 40));
      if (ui._lastFrameLatency > 0 && !seen.has(ui._lastMeasuredSeq)) {
        seen.add(ui._lastMeasuredSeq);
        samples.push(ui._lastFrameLatency);
      }
    }
    if (!samples.length) return null;
    samples.sort((a, b) => a - b);
    return {
      samples: samples.length,
      minMs: Math.round(samples[0]),
      medianMs: Math.round(samples[Math.floor(samples.length / 2)]),
      p90Ms: Math.round(samples[Math.floor(samples.length * 0.9)]),
      maxMs: Math.round(samples[samples.length - 1])
    };
  });
  for (let i = 0; i < 60; i++) {
    await page.mouse.move(target.x + (i % 20), target.y + ((i * 3) % 20));
    await page.waitForTimeout(60);
  }
  const latency = await collect;
  step('latency', latency);

  const clientStats = await page.evaluate(() => {
    const s = window.AllShare.app.session.stats;
    return {
      fps: s.fps, width: s.width, height: s.height, codec: s.codec,
      bitrateKbps: Math.round(s.bitrateKbps), rttMs: Math.round(s.rttMs),
      decodeMs: Number(s.decodeMs.toFixed(2)), jitterBufferMs: Math.round(s.jitterBufferMs),
      lossPercent: Number(s.lossPercent.toFixed(2)), quality: s.quality,
      relayed: s.relayed, decoder: s.decoder, freezes: s.freezes
    };
  });
  step('client-stats', clientStats);

  if (opts.hold) await page.waitForTimeout(Number(opts.hold));

  await page.evaluate(() => window.AllShare.app.disconnect('e2e finished'));
  await page.waitForTimeout(700);
  step('disconnected');

  await context.close();
  await browser.close();
  out.decoded = decoded;
  out.hello = hello;
  out.clientStats = clientStats;
  out.target = target;
  out.route = route;
  out.cursor = cursor;
  out.latency = latency;
  out.ok = out.errors.length === 0;
}

main()
  .catch((err) => { fail('fatal', (err && err.stack) || err); })
  .finally(() => {
    process.stdout.write('\n===ALLSHARE_E2E_JSON===\n' + JSON.stringify(out) + '\n');
    process.exit(out.ok ? 0 : 1);
  });
