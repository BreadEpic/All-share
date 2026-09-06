/*
 * ALL SHARE — small shared utilities.
 *
 * Everything is hung off one global namespace. ES modules cannot be used here:
 * they are fetched with CORS, and a page opened from file:// has an opaque
 * origin, so every module import fails. Classic scripts have no such
 * restriction, so the app is a set of classic scripts that populate
 * window.AllShare.
 */
window.AllShare = window.AllShare || {};

(function (AS) {
  'use strict';

  const Util = {};

  // -------------------------------------------------------------- DOM

  /** Query one element within root (default: document). */
  Util.$ = function (selector, root) {
    return (root || document).querySelector(selector);
  };

  /** Query all elements within root as a real array. */
  Util.$$ = function (selector, root) {
    return Array.prototype.slice.call((root || document).querySelectorAll(selector));
  };

  /** Query by the data-el convention used throughout the markup. */
  Util.el = function (name, root) {
    return (root || document).querySelector('[data-el="' + name + '"]');
  };

  /**
   * Create an element.
   *
   * `props.html` assigns innerHTML and is only ever used with strings this
   * codebase itself produces (icons, static markup). Anything that originates
   * from a remote device — a PC name, an error message — goes through
   * `props.text`, which cannot inject markup.
   */
  Util.h = function (tag, props, children) {
    const node = document.createElement(tag);
    if (props) {
      for (const key in props) {
        const value = props[key];
        if (value === null || value === undefined || value === false) continue;
        if (key === 'class') node.className = value;
        else if (key === 'text') node.textContent = value;
        else if (key === 'html') node.innerHTML = value;
        else if (key === 'style' && typeof value === 'object') Object.assign(node.style, value);
        else if (key.startsWith('on') && typeof value === 'function') {
          node.addEventListener(key.slice(2).toLowerCase(), value);
        } else if (value === true) node.setAttribute(key, '');
        else node.setAttribute(key, value);
      }
    }
    if (children) {
      (Array.isArray(children) ? children : [children]).forEach(function (child) {
        if (child === null || child === undefined || child === false) return;
        node.appendChild(typeof child === 'string' ? document.createTextNode(child) : child);
      });
    }
    return node;
  };

  /** Remove every child of a node. */
  Util.clear = function (node) {
    while (node && node.firstChild) node.removeChild(node.firstChild);
    return node;
  };

  /** Show or hide via the hidden attribute, which the stylesheet respects. */
  Util.show = function (node, visible) {
    if (node) node.hidden = !visible;
  };

  // ------------------------------------------------------- listeners

  /**
   * A disposable collection of event listeners.
   *
   * Every listener the app adds to window, document or a media element goes
   * through one of these. A remote-desktop session adds dozens of listeners and
   * can be entered and left repeatedly in one page lifetime; without a single
   * teardown point, duplicates accumulate and input starts firing twice.
   */
  Util.Listeners = function () {
    this._entries = [];
  };
  Util.Listeners.prototype.add = function (target, type, handler, options) {
    if (!target) return handler;
    target.addEventListener(type, handler, options);
    this._entries.push([target, type, handler, options]);
    return handler;
  };
  Util.Listeners.prototype.removeAll = function () {
    for (const entry of this._entries) {
      try {
        entry[0].removeEventListener(entry[1], entry[2], entry[3]);
      } catch (err) {
        /* the target may already be gone; nothing useful to do */
      }
    }
    this._entries.length = 0;
  };
  Util.Listeners.prototype.size = function () {
    return this._entries.length;
  };

  // ------------------------------------------------------------ events

  /** Minimal event emitter. Handlers that throw are logged, never propagated. */
  Util.Emitter = function () {
    this._handlers = Object.create(null);
  };
  Util.Emitter.prototype.on = function (type, fn) {
    (this._handlers[type] || (this._handlers[type] = [])).push(fn);
    return this;
  };
  Util.Emitter.prototype.off = function (type, fn) {
    const list = this._handlers[type];
    if (!list) return this;
    const idx = list.indexOf(fn);
    if (idx >= 0) list.splice(idx, 1);
    return this;
  };
  Util.Emitter.prototype.emit = function (type, payload) {
    const list = this._handlers[type];
    if (!list) return;
    for (const fn of list.slice()) {
      try {
        fn(payload);
      } catch (err) {
        if (AS.Log) AS.Log.error('handler for "' + type + '" threw', err);
        else console.error(err);
      }
    }
  };

  // ------------------------------------------------------------ timing

  Util.sleep = function (ms) {
    return new Promise(function (resolve) { setTimeout(resolve, ms); });
  };

  /** Run fn at most once per animation frame. */
  Util.rafThrottle = function (fn) {
    let scheduled = false;
    let lastArgs = null;
    return function () {
      lastArgs = arguments;
      if (scheduled) return;
      scheduled = true;
      requestAnimationFrame(function () {
        scheduled = false;
        fn.apply(null, lastArgs);
      });
    };
  };

  Util.debounce = function (fn, ms) {
    let timer = 0;
    return function () {
      const args = arguments;
      clearTimeout(timer);
      timer = setTimeout(function () { fn.apply(null, args); }, ms);
    };
  };

  /**
   * A monotonic millisecond clock.
   *
   * performance.now() is used rather than Date.now() because input timestamps
   * are differenced to measure latency, and a clock that can step backwards
   * (NTP, timezone change, suspend) would produce nonsense measurements.
   */
  Util.now = function () {
    return performance.now();
  };

  Util.nowMicro = function () {
    return Math.round(performance.now() * 1000);
  };

  // ------------------------------------------------------------- maths

  Util.clamp = function (value, min, max) {
    return value < min ? min : value > max ? max : value;
  };

  /** Exponential moving average, resistant to a first-sample spike. */
  Util.Ema = function (alpha, initial) {
    this.alpha = alpha;
    this.value = initial === undefined ? null : initial;
  };
  Util.Ema.prototype.push = function (sample) {
    if (!isFinite(sample)) return this.value;
    this.value = this.value === null ? sample : this.value + this.alpha * (sample - this.value);
    return this.value;
  };
  Util.Ema.prototype.get = function (fallback) {
    return this.value === null ? (fallback === undefined ? 0 : fallback) : this.value;
  };

  /** Fixed-size ring buffer, used for the HUD's history graph. */
  Util.Ring = function (capacity) {
    this.capacity = capacity;
    this.items = [];
  };
  Util.Ring.prototype.push = function (value) {
    this.items.push(value);
    if (this.items.length > this.capacity) this.items.shift();
  };
  Util.Ring.prototype.max = function () {
    let max = 0;
    for (const v of this.items) if (v > max) max = v;
    return max;
  };

  // --------------------------------------------------------- encoding

  const B64_CHARS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_';

  /** Unpadded base64url, matching Go's base64.RawURLEncoding. */
  Util.toB64 = function (bytes) {
    const view = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
    let out = '';
    let i = 0;
    for (; i + 2 < view.length; i += 3) {
      const n = (view[i] << 16) | (view[i + 1] << 8) | view[i + 2];
      out += B64_CHARS[(n >> 18) & 63] + B64_CHARS[(n >> 12) & 63] +
             B64_CHARS[(n >> 6) & 63] + B64_CHARS[n & 63];
    }
    const rest = view.length - i;
    if (rest === 1) {
      const n = view[i] << 16;
      out += B64_CHARS[(n >> 18) & 63] + B64_CHARS[(n >> 12) & 63];
    } else if (rest === 2) {
      const n = (view[i] << 16) | (view[i + 1] << 8);
      out += B64_CHARS[(n >> 18) & 63] + B64_CHARS[(n >> 12) & 63] + B64_CHARS[(n >> 6) & 63];
    }
    return out;
  };

  const B64_LOOKUP = (function () {
    const table = new Uint8Array(256).fill(255);
    for (let i = 0; i < B64_CHARS.length; i++) table[B64_CHARS.charCodeAt(i)] = i;
    // Accept standard base64 too, so a value pasted from another tool works.
    table['+'.charCodeAt(0)] = 62;
    table['/'.charCodeAt(0)] = 63;
    return table;
  })();

  Util.fromB64 = function (text) {
    const clean = String(text).replace(/=+$/, '');
    const out = new Uint8Array(Math.floor((clean.length * 6) / 8));
    let acc = 0;
    let bits = 0;
    let outIdx = 0;
    for (let i = 0; i < clean.length; i++) {
      const value = B64_LOOKUP[clean.charCodeAt(i)];
      if (value === 255) throw new Error('not valid base64');
      acc = (acc << 6) | value;
      bits += 6;
      if (bits >= 8) {
        bits -= 8;
        out[outIdx++] = (acc >> bits) & 0xFF;
      }
    }
    return out.subarray(0, outIdx);
  };

  const encoder = new TextEncoder();
  const decoder = new TextDecoder();
  Util.utf8 = function (text) { return encoder.encode(text); };
  Util.fromUtf8 = function (bytes) { return decoder.decode(bytes); };

  Util.concatBytes = function (parts) {
    let total = 0;
    for (const p of parts) total += p.length;
    const out = new Uint8Array(total);
    let offset = 0;
    for (const p of parts) { out.set(p, offset); offset += p.length; }
    return out;
  };

  Util.bytesEqual = function (a, b) {
    if (!a || !b || a.length !== b.length) return false;
    let diff = 0;
    for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i];
    return diff === 0;
  };

  Util.randomBytes = function (n) {
    const out = new Uint8Array(n);
    crypto.getRandomValues(out);
    return out;
  };

  Util.randomId = function () {
    return Util.toB64(Util.randomBytes(12));
  };

  // ------------------------------------------------------- formatting

  Util.formatBitrate = function (bitsPerSecond) {
    if (!isFinite(bitsPerSecond) || bitsPerSecond <= 0) return '0';
    if (bitsPerSecond >= 1e6) return (bitsPerSecond / 1e6).toFixed(1) + ' Mbps';
    return Math.round(bitsPerSecond / 1e3) + ' kbps';
  };

  Util.formatMs = function (ms) {
    if (!isFinite(ms) || ms < 0) return '—';
    return (ms >= 100 ? Math.round(ms) : ms >= 10 ? ms.toFixed(0) : ms.toFixed(1)) + ' ms';
  };

  Util.formatAge = function (timestampMs) {
    if (!timestampMs) return 'never';
    const seconds = Math.max(0, (Date.now() - timestampMs) / 1000);
    if (seconds < 60) return 'just now';
    if (seconds < 3600) return Math.floor(seconds / 60) + ' min ago';
    if (seconds < 86400) return Math.floor(seconds / 3600) + ' h ago';
    const days = Math.floor(seconds / 86400);
    return days === 1 ? 'yesterday' : days + ' days ago';
  };

  Util.formatDuration = function (seconds) {
    if (!isFinite(seconds) || seconds <= 0) return 'a moment';
    if (seconds < 60) return Math.round(seconds) + ' seconds';
    const minutes = Math.round(seconds / 60);
    return minutes === 1 ? 'about a minute' : 'about ' + minutes + ' minutes';
  };

  /** A default label for this device, shown on the PC during pairing. */
  Util.guessDeviceLabel = function () {
    const ua = navigator.userAgent || '';
    if (/CrOS/.test(ua)) return 'Chromebook';
    if (/Android/.test(ua)) return 'Android device';
    if (/iPhone|iPad/.test(ua)) return 'iPad or iPhone';
    if (/Macintosh/.test(ua)) return 'Mac';
    if (/Windows/.test(ua)) return 'Windows PC';
    if (/Linux/.test(ua)) return 'Linux PC';
    return 'Browser';
  };

  AS.Util = Util;
})(window.AllShare);
