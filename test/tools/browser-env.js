/*
 * A minimal browser-shaped sandbox for running the client's classic scripts
 * under Node, so the pure-logic modules (protocol codec, keymap, pairing
 * crypto) can be unit-tested without a browser.
 *
 * Anything that genuinely needs a browser — WebRTC, pointer lock, the DOM — is
 * covered by the Playwright end-to-end suite instead, against a real Chromium
 * loading a real file:// URL.
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const ROOT = path.resolve(__dirname, '..', '..');

function createSandbox(extra) {
  const sandbox = Object.assign({
    console,
    performance: { now: () => Number(process.hrtime.bigint() / 1000n) / 1000 },
    TextEncoder,
    TextDecoder,
    crypto: require('crypto').webcrypto,
    setTimeout, clearTimeout, setInterval, clearInterval,
    queueMicrotask,
    requestAnimationFrame: (fn) => setTimeout(() => fn(Date.now()), 16),
    cancelAnimationFrame: (id) => clearTimeout(id),
    navigator: { userAgent: 'Mozilla/5.0 (X11; CrOS x86_64) Chrome/141' },
    location: { href: 'file:///allshare/index.html', origin: 'null' },
    localStorage: makeStorage(),
    sessionStorage: makeStorage(),
    document: makeDocument(),
    indexedDB: undefined,
    URL,
    Blob: typeof Blob !== 'undefined' ? Blob : undefined
  }, extra || {});
  sandbox.window = sandbox;
  sandbox.self = sandbox;
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  return sandbox;
}

function makeStorage() {
  const map = new Map();
  return {
    getItem: (k) => (map.has(k) ? map.get(k) : null),
    setItem: (k, v) => map.set(k, String(v)),
    removeItem: (k) => map.delete(k),
    clear: () => map.clear()
  };
}

function makeNode(tag) {
  const node = {
    tagName: String(tag || '').toUpperCase(),
    children: [], style: {}, dataset: {}, classList: {
      _set: new Set(),
      add(...c) { c.forEach((x) => this._set.add(x)); },
      remove(...c) { c.forEach((x) => this._set.delete(x)); },
      toggle(c, on) { if (on === undefined) on = !this._set.has(c); on ? this._set.add(c) : this._set.delete(c); return on; },
      contains(c) { return this._set.has(c); }
    },
    textContent: '', innerHTML: '', hidden: false,
    attributes: {},
    setAttribute(k, v) { this.attributes[k] = String(v); },
    getAttribute(k) { return k in this.attributes ? this.attributes[k] : null; },
    removeAttribute(k) { delete this.attributes[k]; },
    appendChild(child) { this.children.push(child); return child; },
    removeChild(child) { const i = this.children.indexOf(child); if (i >= 0) this.children.splice(i, 1); return child; },
    addEventListener() {}, removeEventListener() {},
    querySelector: () => null,
    querySelectorAll: () => [],
    get firstChild() { return this.children[0] || null; }
  };
  return node;
}

function makeDocument() {
  const doc = makeNode('#document');
  doc.createElement = (tag) => makeNode(tag);
  doc.createTextNode = (text) => ({ nodeType: 3, textContent: text });
  doc.documentElement = makeNode('html');
  doc.body = makeNode('body');
  doc.addEventListener = () => {};
  doc.removeEventListener = () => {};
  doc.querySelector = () => null;
  doc.querySelectorAll = () => [];
  return doc;
}

/** Load client scripts, in order, into one sandbox. */
function load(files, extra) {
  const sandbox = createSandbox(extra);
  for (const file of files) {
    const full = path.join(ROOT, 'client', file);
    vm.runInContext(fs.readFileSync(full, 'utf8'), sandbox, { filename: file });
  }
  return sandbox;
}

/** The pure-logic subset: no DOM, no network. */
const CORE_FILES = [
  'js/core/util.js',
  'js/core/log.js',
  'js/net/protocol.js',
  'js/input/keymap.js'
];

module.exports = { ROOT, load, createSandbox, CORE_FILES };
