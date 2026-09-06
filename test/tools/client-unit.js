/*
 * Unit tests for the client's pure-logic modules, run under Node.
 * Browser-dependent behaviour is covered by the Playwright suite instead.
 */
'use strict';

const env = require('./browser-env');

let failures = 0;
let checks = 0;
const groups = [];

function group(name, fn) { groups.push([name, fn]); }
function ok(name, condition, detail) {
  checks++;
  if (condition) return;
  failures++;
  console.error('  FAIL ' + name + (detail ? ' — ' + detail : ''));
}
function eq(name, got, want) {
  ok(name, Object.is(got, want) || String(got) === String(want), 'got ' + got + ', want ' + want);
}

// ---------------------------------------------------------------- util

group('util', () => {
  const AS = env.load(env.CORE_FILES).window.AllShare;
  const U = AS.Util;

  for (let n = 0; n < 40; n++) {
    const bytes = new Uint8Array(n);
    for (let i = 0; i < n; i++) bytes[i] = (i * 37 + n) & 255;
    const encoded = U.toB64(bytes);
    ok('base64url matches Node (len ' + n + ')', encoded === Buffer.from(bytes).toString('base64url'));
    ok('base64url round trip (len ' + n + ')', Buffer.compare(Buffer.from(U.fromB64(encoded)), Buffer.from(bytes)) === 0);
  }

  ok('fromB64 rejects invalid characters', (() => {
    try { U.fromB64('!!!!'); return false; } catch (e) { return true; }
  })());

  eq('clamp low', U.clamp(-5, 0, 10), 0);
  eq('clamp high', U.clamp(50, 0, 10), 10);

  const ema = new U.Ema(0.5);
  ema.push(10); ema.push(20);
  eq('ema tracks', ema.get().toFixed(1), '15.0');
  ok('ema ignores non-finite', (() => { const e = new U.Ema(0.5, 5); e.push(NaN); return e.get() === 5; })());

  eq('formatBitrate mbps', U.formatBitrate(3_400_000), '3.4 Mbps');
  eq('formatBitrate kbps', U.formatBitrate(450_000), '450 kbps');
  eq('formatBitrate zero', U.formatBitrate(0), '0');
  eq('formatMs', U.formatMs(23.4), '23 ms');
  eq('formatMs unknown', U.formatMs(-1), '—');
  eq('formatDuration minutes', U.formatDuration(900), 'about 15 minutes');

  // Listeners must actually detach; a session can be entered many times.
  let fired = 0;
  const target = { _h: [], addEventListener(t, h) { this._h.push([t, h]); }, removeEventListener(t, h) { this._h = this._h.filter((e) => e[1] !== h); } };
  const listeners = new U.Listeners();
  listeners.add(target, 'x', () => fired++);
  listeners.add(target, 'y', () => fired++);
  eq('listeners tracked', listeners.size(), 2);
  listeners.removeAll();
  eq('listeners detached from target', target._h.length, 0);
  eq('listeners emptied', listeners.size(), 0);

  // An emitter handler that throws must not stop the others.
  const emitter = new U.Emitter();
  let reached = false;
  emitter.on('e', () => { throw new Error('boom'); });
  emitter.on('e', () => { reached = true; });
  emitter.emit('e');
  ok('emitter isolates a throwing handler', reached);
});

// ------------------------------------------------------------ protocol

group('protocol', () => {
  const AS = env.load(env.CORE_FILES).window.AllShare;
  const P = AS.Protocol;

  const bitmap = new P.KeyBitmap();
  bitmap.set(0x1A, true);
  bitmap.set(0xE1, true);
  ok('bitmap set/get', bitmap.get(0x1A) && bitmap.get(0xE1));
  ok('bitmap unset stays unset', !bitmap.get(0x04));
  eq('bitmap held list', bitmap.heldUsages().join(','), '26,225');
  bitmap.set(0x1A, false);
  ok('bitmap clear one', !bitmap.get(0x1A) && bitmap.get(0xE1));
  bitmap.clear();
  ok('bitmap clear all', !bitmap.any());
  bitmap.set(9999, true);
  ok('bitmap ignores out-of-range usage', !bitmap.any());

  // Sizes must match the constants, which the Go vectors also pin.
  eq('mouseMoveAbs size', P.encodeMouseMoveAbs(1, 2, 3, 4, 5).length, P.SIZE_MOUSE_MOVE_ABS);
  eq('key size', P.encodeKey(1, 2, 3, true, bitmap).length, P.SIZE_KEY);

  // Relative deltas must saturate rather than wrap: a wrapped delta would fling
  // the remote cursor to the opposite edge.
  const huge = P.decodeMouseMoveRel(P.encodeMouseMoveRel(0, 0, 999999, -999999, 0));
  eq('relative delta clamps high', huge.dx, 32767);
  eq('relative delta clamps low', huge.dy, -32768);

  // Hostile control messages must decode to null, never throw.
  for (const buf of [new Uint8Array(0), new Uint8Array([0x83]), new Uint8Array([0x86, 1, 2]),
                     new Uint8Array([0x80, 0x7B, 0x7B]), new Uint8Array([0x82, 255, 255, 255, 255])]) {
    let threw = false, result;
    try { result = P.decode(buf); } catch (e) { threw = true; }
    ok('hostile control message len ' + buf.length + ' handled', !threw && (result === null || result.unknown || result.type !== undefined));
  }

  // An unknown type must be reported rather than fatal, so a newer agent can
  // add messages without breaking an older client.
  const unknown = P.decode(new Uint8Array([0xFE, 1, 2, 3]));
  ok('unknown type is not fatal', unknown && unknown.unknown === true);

  // A cursor shape whose declared header runs past the buffer must be rejected.
  const bad = new Uint8Array(9);
  bad[0] = P.TYPE_CURSOR_SHAPE;
  new DataView(bad.buffer).setUint32(1, 0xFFFFFF, true);
  eq('oversize cursor header rejected', P.decode(bad), null);
});

// -------------------------------------------------------------- keymap

group('keymap', () => {
  const AS = env.load(env.CORE_FILES).window.AllShare;
  const K = AS.Keymap;

  const expected = {
    KeyW: 0x1A, KeyA: 0x04, KeyZ: 0x1D, Digit1: 0x1E, Digit0: 0x27,
    Enter: 0x28, Escape: 0x29, Space: 0x2C, CapsLock: 0x39,
    F1: 0x3A, F12: 0x45, Insert: 0x49, Delete: 0x4C,
    ArrowUp: 0x52, ArrowDown: 0x51, ArrowLeft: 0x50, ArrowRight: 0x4F,
    NumLock: 0x53, NumpadEnter: 0x58, Numpad0: 0x62, NumpadDecimal: 0x63,
    ContextMenu: 0x65, ControlLeft: 0xE0, ShiftLeft: 0xE1, AltLeft: 0xE2,
    MetaLeft: 0xE3, ControlRight: 0xE4, ShiftRight: 0xE5, AltRight: 0xE6, MetaRight: 0xE7
  };
  for (const code in expected) {
    eq('usage for ' + code, K.usageFor({ code: code }), expected[code]);
  }

  eq('unmapped code yields 0', K.usageFor({ code: 'MediaTrackNext' }), 0);
  eq('missing code yields 0', K.usageFor({}), 0);

  // Every usage must be addressable in the 256-bit held-key bitmap.
  for (const code in K.CODE_TO_HID) {
    ok('usage in bitmap range: ' + code, K.CODE_TO_HID[code] < 256);
  }
  for (const code in K.CHROMEOS_TOP_ROW) {
    ok('top-row usage in range: ' + code, K.CHROMEOS_TOP_ROW[code] < 256);
  }

  // Every letter and digit must be present; a hole here is a typing bug.
  for (let i = 0; i < 26; i++) {
    const code = 'Key' + String.fromCharCode(65 + i);
    eq('letter ' + code, K.usageFor({ code: code }), 0x04 + i);
  }

  eq('chromebook top row off by default', K.usageFor({ code: 'BrowserRefresh' }), 0);
  eq('chromebook top row on request', K.usageFor({ code: 'BrowserRefresh' }, { chromebookTopRow: true }), 0x3C);
  eq('chromebook volume up becomes F10', K.usageFor({ code: 'AudioVolumeUp' }, { chromebookTopRow: true }), 0x43);

  ok('modifiers identified', K.isModifier(0xE0) && K.isModifier(0xE7));
  ok('non-modifier not identified', !K.isModifier(0x04));

  // Duplicated HID usages would make two physical keys indistinguishable.
  const seen = new Map();
  for (const code in K.CODE_TO_HID) {
    const usage = K.CODE_TO_HID[code];
    // OSLeft/OSRight are deliberate aliases of MetaLeft/MetaRight.
    if (code === 'OSLeft' || code === 'OSRight') continue;
    ok('usage 0x' + usage.toString(16) + ' is unique', !seen.has(usage),
       code + ' collides with ' + seen.get(usage));
    seen.set(usage, code);
  }
});

// ---------------------------------------------------------------- run

console.log('Client unit tests');
for (const [name, fn] of groups) {
  const before = failures;
  fn();
  console.log('  ' + (failures === before ? 'ok  ' : 'FAIL') + '  ' + name);
}
console.log(checks + ' checks, ' + failures + ' failure(s)');
process.exit(failures ? 1 : 0);
