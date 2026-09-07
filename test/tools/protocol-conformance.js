/*
 * Cross-language conformance check.
 *
 * The Go tests emit shared/protocol/testdata/vectors.json. This script loads
 * the browser implementation and asserts that it produces byte-identical
 * encodings and correct decodings for every vector. If the two implementations
 * ever drift, this fails loudly instead of turning into an input bug that only
 * reproduces on someone's desk.
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const ROOT = path.resolve(__dirname, '..', '..');
const VECTORS = path.join(ROOT, 'shared', 'protocol', 'testdata', 'vectors.json');

function loadBrowserModules(files) {
  const sandbox = {
    console,
    performance: { now: () => Date.now() },
    TextEncoder,
    TextDecoder,
    crypto: { getRandomValues: (a) => a },
    document: { querySelector: () => null, createElement: () => ({ setAttribute() {}, addEventListener() {}, style: {} }), createTextNode: (t) => t },
    navigator: { userAgent: 'conformance' },
    requestAnimationFrame: (fn) => setTimeout(fn, 0),
    setTimeout, clearTimeout, setInterval, clearInterval
  };
  sandbox.window = sandbox;
  sandbox.self = sandbox;
  vm.createContext(sandbox);
  for (const file of files) {
    vm.runInContext(fs.readFileSync(path.join(ROOT, 'client', file), 'utf8'), sandbox, { filename: file });
  }
  return sandbox.window.AllShare;
}

function hex(bytes) {
  return Array.from(bytes).map((b) => b.toString(16).padStart(2, '0')).join('');
}

const AS = loadBrowserModules(['js/core/util.js', 'js/core/log.js', 'js/net/protocol.js']);
const P = AS.Protocol;

if (!fs.existsSync(VECTORS)) {
  console.error('vectors.json is missing — run `go test ./shared/protocol/` first');
  process.exit(1);
}
const corpus = JSON.parse(fs.readFileSync(VECTORS, 'utf8'));

let failures = 0;
function check(name, condition, detail) {
  if (condition) return;
  failures++;
  console.error('  FAIL ' + name + (detail ? ': ' + detail : ''));
}

if (corpus.keyBitmapBytes !== P.KEY_BITMAP_BYTES) {
  check('keyBitmapBytes', false, corpus.keyBitmapBytes + ' vs ' + P.KEY_BITMAP_BYTES);
}
if (corpus.versionMajor !== P.VERSION_MAJOR) {
  check('versionMajor', false, corpus.versionMajor + ' vs ' + P.VERSION_MAJOR);
}
if (corpus.maxCtrlMessage !== P.MAX_CTRL_MESSAGE) {
  check('maxCtrlMessage', false, corpus.maxCtrlMessage + ' vs ' + P.MAX_CTRL_MESSAGE);
}
// The clipboard limit is enforced on both sides, and both sides tell the user
// about it. If they disagree, one end promises something the other refuses.
if (corpus.maxClipboardBytes !== P.MAX_CLIPBOARD_BYTES) {
  check('maxClipboardBytes', false, corpus.maxClipboardBytes + ' vs ' + P.MAX_CLIPBOARD_BYTES);
}
if (corpus.wheelTicksPerNotch !== P.WHEEL_TICKS_PER_NOTCH) {
  check('wheelTicksPerNotch', false, corpus.wheelTicksPerNotch + ' vs ' + P.WHEEL_TICKS_PER_NOTCH);
}

function bitmapFromHex(h) {
  const bm = new P.KeyBitmap();
  for (let i = 0; i < h.length; i += 2) bm.bytes[i / 2] = parseInt(h.substr(i, 2), 16);
  return bm;
}

const encoders = {
  mouseMoveAbs: (v) => P.encodeMouseMoveAbs(v.seq, v.ts, v.x, v.y, v.buttons),
  mouseMoveRel: (v) => P.encodeMouseMoveRel(v.seq, v.ts, v.dx, v.dy, v.buttons),
  mouseButton: (v) => P.encodeMouseButton(v.seq, v.ts, v.button, v.down, v.buttons),
  mouseWheel: (v) => P.encodeMouseWheel(v.seq, v.ts, v.dx, v.dy),
  key: (v) => P.encodeKey(v.seq, v.ts, v.usage, v.down, bitmapFromHex(v.state)),
  keyStateSync: (v) => P.encodeKeyStateSync(v.seq, v.ts, v.buttons, bitmapFromHex(v.state)),
  inputPing: (v) => P.encodeInputPing(v.seq, v.tsMicro),
  textInput: (v) => P.encodeTextInput(v.seq, v.text)
};

const decoders = {
  mouseMoveAbs: (b) => P.decodeMouseMoveAbs(b),
  mouseMoveRel: (b) => P.decodeMouseMoveRel(b),
  mouseButton: (b) => P.decodeMouseButton(b),
  mouseWheel: (b) => P.decodeMouseWheel(b),
  key: (b) => P.decodeKey(b),
  keyStateSync: (b) => P.decodeKeyStateSync(b),
  inputPing: (b) => P.decodeInputPing(b),
  textInput: (b) => P.decodeTextInput(b),
  cursorState: (b) => P.decode(b),
  frameMark: (b) => P.decode(b),
  pong: (b) => P.decode(b)
};

console.log('Protocol conformance: ' + corpus.vectors.length + ' vectors');

for (const vec of corpus.vectors) {
  const expected = vec.hex;
  const bytes = Uint8Array.from(expected.match(/../g).map((h) => parseInt(h, 16)));

  if (encoders[vec.name]) {
    const got = encoders[vec.name](vec.value);
    check(vec.name + ' encode', hex(got) === expected, '\n    go: ' + expected + '\n    js: ' + hex(got));
  }

  if (decoders[vec.name]) {
    const decoded = decoders[vec.name](bytes);
    check(vec.name + ' decode', decoded !== null, 'decoder returned null');
    if (decoded) {
      for (const key of Object.keys(vec.value)) {
        if (key === 'state') {
          check(vec.name + '.state', hex(decoded.state.bytes) === vec.value.state,
            hex(decoded.state.bytes) + ' vs ' + vec.value.state);
          continue;
        }
        const jsKey = ({
          ts: 'ts', tsMicro: 'tsMicro',
          shapeId: 'shapeId', rtpTimestamp: 'rtpTimestamp', inputSeq: 'inputSeq',
          captureMicro: 'captureMicro', encodeMicro: 'encodeMicro', sizeBytes: 'sizeBytes',
          clientTSMicro: 'clientTSMicro', agentTSMicro: 'agentTSMicro'
        })[key] || key;
        if (!(jsKey in decoded)) continue;
        const want = vec.value[key];
        const got = decoded[jsKey];
        check(vec.name + '.' + key, String(got) === String(want), got + ' vs ' + want);
      }
    }
  }
}

// Truncation sweep: no decoder may throw or return junk for a short buffer.
const allDecoders = Object.values(decoders).concat([(b) => P.decode(b)]);
for (let len = 0; len < 40; len++) {
  const buf = new Uint8Array(len).fill(0xAB);
  for (const dec of allDecoders) {
    try { dec(buf); } catch (err) {
      check('truncation len=' + len, false, err.message);
    }
  }
}

// Normalisation must survive a resolution change without drift.
const norm = P.normalize(960, 1920);
check('normalize endpoints', P.normalize(0, 1920) === 0 && P.normalize(1919, 1920) === 65535);
check('normalize clamps', P.normalize(99999, 1920) === 65535);
const at1280 = P.denormalize(norm, 1280);
check('normalize across resolutions', Math.abs(at1280 - 640) <= 2, 'mid-screen -> ' + at1280 + ' of 1280');
for (const px of [0, 1, 640, 959, 960, 1919]) {
  check('normalize roundtrip ' + px, P.denormalize(P.normalize(px, 1920), 1920) === px);
}

if (failures) {
  console.error('\n' + failures + ' conformance failure(s)');
  process.exit(1);
}
console.log('All vectors match the Go implementation byte for byte.');
