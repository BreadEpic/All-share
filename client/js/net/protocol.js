/*
 * ALL SHARE — the binary data plane, browser side.
 *
 * This is a second implementation of shared/protocol. The two are kept honest
 * by a conformance corpus: the Go tests emit shared/protocol/testdata/vectors.json,
 * and the browser test suite decodes and re-encodes every entry, so a change to
 * one side that the other does not match fails the build rather than producing
 * a mysterious input bug months later.
 *
 * Everything is little-endian, matching DataView(…, true) here and a
 * little-endian host at the other end, so neither side byte-swaps on the hot
 * path.
 */
(function (AS) {
  'use strict';

  const U = AS.Util;
  const P = {};

  P.VERSION_MAJOR = 1;
  P.VERSION_MINOR = 0;

  // Input plane, client to agent.
  P.TYPE_MOUSE_MOVE_ABS = 0x01;
  P.TYPE_MOUSE_MOVE_REL = 0x02;
  P.TYPE_MOUSE_BUTTON = 0x03;
  P.TYPE_MOUSE_WHEEL = 0x04;
  P.TYPE_KEY = 0x05;
  P.TYPE_KEY_STATE_SYNC = 0x06;
  P.TYPE_INPUT_PING = 0x07;
  P.TYPE_TEXT_INPUT = 0x08;

  // Control plane, agent to client.
  P.TYPE_HELLO = 0x80;
  P.TYPE_STATS = 0x81;
  P.TYPE_CURSOR_SHAPE = 0x82;
  P.TYPE_CURSOR_STATE = 0x83;
  P.TYPE_CLIPBOARD_OUT = 0x84;
  P.TYPE_DISPLAY_CHANGED = 0x85;
  P.TYPE_FRAME_MARK = 0x86;
  P.TYPE_PONG = 0x87;
  P.TYPE_NOTICE = 0x88;
  P.TYPE_CONTROL_STATE = 0x89;

  // Control plane, client to agent.
  P.TYPE_SET_QUALITY = 0xC0;
  P.TYPE_SELECT_MONITOR = 0xC1;
  P.TYPE_CLIPBOARD_IN = 0xC2;
  P.TYPE_REQUEST_KEYFRAME = 0xC3;
  P.TYPE_SET_CURSOR_MODE = 0xC4;
  P.TYPE_CTRL_PING = 0xC5;
  P.TYPE_VIEWPORT = 0xC6;
  P.TYPE_DISCONNECT = 0xC7;
  P.TYPE_REQUEST_CONTROL = 0xC8;
  P.TYPE_SET_POINTER_MODE = 0xC9;
  P.TYPE_SYSTEM_ACTION = 0xCA;

  P.KEY_BITMAP_BYTES = 32;
  P.MAX_CTRL_MESSAGE = 192 * 1024;
  // Must match protocol.MaxClipboardBytes in Go. This is a *byte* limit, not a
  // character one: the agent measures UTF-8, so counting characters here would
  // let anything non-Latin through and have it rejected at the far end.
  P.MAX_CLIPBOARD_BYTES = 64 * 1024;
  P.WHEEL_TICKS_PER_NOTCH = 120;

  P.BUTTON_LEFT = 1;
  P.BUTTON_RIGHT = 2;
  P.BUTTON_MIDDLE = 4;
  P.BUTTON_X1 = 8;
  P.BUTTON_X2 = 16;

  P.SIZE_MOUSE_MOVE_ABS = 14;
  P.SIZE_MOUSE_MOVE_REL = 14;
  P.SIZE_MOUSE_BUTTON = 12;
  P.SIZE_MOUSE_WHEEL = 17;
  P.SIZE_KEY = 12 + P.KEY_BITMAP_BYTES;
  P.SIZE_KEY_STATE_SYNC = 10 + P.KEY_BITMAP_BYTES;
  P.SIZE_INPUT_PING = 13;
  P.SIZE_CURSOR_STATE = 15;
  P.SIZE_FRAME_MARK = 26;
  P.SIZE_PONG = 21;

  // ------------------------------------------------------- key bitmap

  /** The set of currently-held keys, indexed by USB HID usage ID. */
  P.KeyBitmap = function () {
    this.bytes = new Uint8Array(P.KEY_BITMAP_BYTES);
  };
  P.KeyBitmap.prototype.set = function (usage, down) {
    if (usage < 0 || usage >= P.KEY_BITMAP_BYTES * 8) return;
    const mask = 1 << (usage % 8);
    if (down) this.bytes[usage >> 3] |= mask;
    else this.bytes[usage >> 3] &= ~mask;
  };
  P.KeyBitmap.prototype.get = function (usage) {
    if (usage < 0 || usage >= P.KEY_BITMAP_BYTES * 8) return false;
    return (this.bytes[usage >> 3] & (1 << (usage % 8))) !== 0;
  };
  P.KeyBitmap.prototype.any = function () {
    for (let i = 0; i < this.bytes.length; i++) if (this.bytes[i] !== 0) return true;
    return false;
  };
  P.KeyBitmap.prototype.clear = function () { this.bytes.fill(0); };
  P.KeyBitmap.prototype.heldUsages = function () {
    const out = [];
    for (let i = 0; i < this.bytes.length; i++) {
      if (this.bytes[i] === 0) continue;
      for (let bit = 0; bit < 8; bit++) {
        if (this.bytes[i] & (1 << bit)) out.push(i * 8 + bit);
      }
    }
    return out;
  };

  // -------------------------------------------------------- encoders

  function writer(size) {
    const buffer = new ArrayBuffer(size);
    return { bytes: new Uint8Array(buffer), view: new DataView(buffer), pos: 0 };
  }

  P.encodeMouseMoveAbs = function (seq, tsMilli, x, y, buttons) {
    const w = writer(P.SIZE_MOUSE_MOVE_ABS);
    w.view.setUint8(0, P.TYPE_MOUSE_MOVE_ABS);
    w.view.setUint32(1, seq >>> 0, true);
    w.view.setUint32(5, tsMilli >>> 0, true);
    w.view.setUint16(9, x & 0xFFFF, true);
    w.view.setUint16(11, y & 0xFFFF, true);
    w.view.setUint8(13, buttons & 0xFF);
    return w.bytes;
  };

  P.encodeMouseMoveRel = function (seq, tsMilli, dx, dy, buttons) {
    const w = writer(P.SIZE_MOUSE_MOVE_REL);
    w.view.setUint8(0, P.TYPE_MOUSE_MOVE_REL);
    w.view.setUint32(1, seq >>> 0, true);
    w.view.setUint32(5, tsMilli >>> 0, true);
    w.view.setInt16(9, clampInt(dx, -32768, 32767), true);
    w.view.setInt16(11, clampInt(dy, -32768, 32767), true);
    w.view.setUint8(13, buttons & 0xFF);
    return w.bytes;
  };

  P.encodeMouseButton = function (seq, tsMilli, button, down, buttons) {
    const w = writer(P.SIZE_MOUSE_BUTTON);
    w.view.setUint8(0, P.TYPE_MOUSE_BUTTON);
    w.view.setUint32(1, seq >>> 0, true);
    w.view.setUint32(5, tsMilli >>> 0, true);
    w.view.setUint8(9, button & 0xFF);
    w.view.setUint8(10, down ? 1 : 0);
    w.view.setUint8(11, buttons & 0xFF);
    return w.bytes;
  };

  P.encodeMouseWheel = function (seq, tsMilli, dx, dy) {
    const w = writer(P.SIZE_MOUSE_WHEEL);
    w.view.setUint8(0, P.TYPE_MOUSE_WHEEL);
    w.view.setUint32(1, seq >>> 0, true);
    w.view.setUint32(5, tsMilli >>> 0, true);
    w.view.setInt32(9, clampInt(dx, -2147483648, 2147483647), true);
    w.view.setInt32(13, clampInt(dy, -2147483648, 2147483647), true);
    return w.bytes;
  };

  P.encodeKey = function (seq, tsMilli, usage, down, bitmap) {
    const w = writer(P.SIZE_KEY);
    w.view.setUint8(0, P.TYPE_KEY);
    w.view.setUint32(1, seq >>> 0, true);
    w.view.setUint32(5, tsMilli >>> 0, true);
    w.view.setUint16(9, usage & 0xFFFF, true);
    w.view.setUint8(11, down ? 1 : 0);
    w.bytes.set(bitmap.bytes, 12);
    return w.bytes;
  };

  P.encodeKeyStateSync = function (seq, tsMilli, buttons, bitmap) {
    const w = writer(P.SIZE_KEY_STATE_SYNC);
    w.view.setUint8(0, P.TYPE_KEY_STATE_SYNC);
    w.view.setUint32(1, seq >>> 0, true);
    w.view.setUint32(5, tsMilli >>> 0, true);
    w.view.setUint8(9, buttons & 0xFF);
    w.bytes.set(bitmap.bytes, 10);
    return w.bytes;
  };

  /**
   * Microsecond timestamps travel as u64. A JavaScript number holds them
   * exactly up to 2^53 microseconds, which is the year 2255 for a Unix-epoch
   * value and about 285 years for a page-uptime value, so a plain number is
   * used on the hot path and a BigInt is accepted for completeness.
   */
  P.encodeInputPing = function (seq, tsMicro) {
    const w = writer(P.SIZE_INPUT_PING);
    w.view.setUint8(0, P.TYPE_INPUT_PING);
    w.view.setUint32(1, seq >>> 0, true);
    const micro = typeof tsMicro === 'bigint' ? tsMicro : BigInt(Math.max(0, Math.round(tsMicro)));
    w.view.setBigUint64(5, micro, true);
    return w.bytes;
  };

  P.encodeTextInput = function (seq, text) {
    const body = U.utf8(text);
    const w = writer(9 + body.length);
    w.view.setUint8(0, P.TYPE_TEXT_INPUT);
    w.view.setUint32(1, seq >>> 0, true);
    w.view.setUint32(5, body.length, true);
    w.bytes.set(body, 9);
    return w.bytes;
  };

  /** Frame a JSON control message: one type byte then the document. */
  P.encodeJSON = function (type, value) {
    const body = U.utf8(JSON.stringify(value));
    const out = new Uint8Array(1 + body.length);
    out[0] = type;
    out.set(body, 1);
    return out;
  };

  // -------------------------------------------------------- decoders

  /**
   * Decode a control-plane message.
   *
   * Every branch validates its length before reading. These bytes arrive from
   * the remote PC over a data channel; a truncated or hostile message must
   * produce null, never an exception that tears down the session and never an
   * out-of-bounds read.
   */
  P.decode = function (buffer) {
    const bytes = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
    if (bytes.length < 1) return null;
    const type = bytes[0];
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);

    switch (type) {
      case P.TYPE_CURSOR_STATE: {
        if (bytes.length < P.SIZE_CURSOR_STATE) return null;
        return {
          type: type,
          seq: view.getUint32(1, true),
          shapeId: view.getUint32(5, true),
          x: view.getUint16(9, true),
          y: view.getUint16(11, true),
          visible: view.getUint8(13) !== 0,
          relative: view.getUint8(14) !== 0
        };
      }
      case P.TYPE_FRAME_MARK: {
        if (bytes.length < P.SIZE_FRAME_MARK) return null;
        return {
          type: type,
          rtpTimestamp: view.getUint32(1, true),
          inputSeq: view.getUint32(5, true),
          captureMicro: Number(view.getBigUint64(9, true)),
          encodeMicro: view.getUint32(17, true),
          sizeBytes: view.getUint32(21, true),
          keyframe: view.getUint8(25) !== 0
        };
      }
      case P.TYPE_PONG: {
        if (bytes.length < P.SIZE_PONG) return null;
        return {
          type: type,
          seq: view.getUint32(1, true),
          clientTSMicro: Number(view.getBigUint64(5, true)),
          agentTSMicro: Number(view.getBigUint64(13, true))
        };
      }
      case P.TYPE_CURSOR_SHAPE: {
        if (bytes.length < 5) return null;
        const headLen = view.getUint32(1, true);
        if (headLen > bytes.length - 5) return null;
        let meta;
        try {
          meta = JSON.parse(U.fromUtf8(bytes.subarray(5, 5 + headLen)));
        } catch (err) {
          return null;
        }
        return { type: type, meta: meta, image: bytes.subarray(5 + headLen) };
      }
      case P.TYPE_HELLO:
      case P.TYPE_STATS:
      case P.TYPE_CLIPBOARD_OUT:
      case P.TYPE_DISPLAY_CHANGED:
      case P.TYPE_NOTICE:
      case P.TYPE_CONTROL_STATE: {
        if (bytes.length - 1 > P.MAX_CTRL_MESSAGE) return null;
        try {
          return { type: type, body: JSON.parse(U.fromUtf8(bytes.subarray(1))) };
        } catch (err) {
          return null;
        }
      }
      default:
        // Unknown types are reported, not fatal, so a newer agent can add
        // messages without breaking an older client.
        return { type: type, unknown: true };
    }
  };

  // The input-plane decoders exist for the conformance tests and for any future
  // client that also receives input (a second viewer, a recorder).
  P.decodeMouseMoveAbs = function (bytes) {
    if (bytes.length < P.SIZE_MOUSE_MOVE_ABS) return null;
    const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    return {
      seq: v.getUint32(1, true), ts: v.getUint32(5, true),
      x: v.getUint16(9, true), y: v.getUint16(11, true), buttons: v.getUint8(13)
    };
  };
  P.decodeMouseMoveRel = function (bytes) {
    if (bytes.length < P.SIZE_MOUSE_MOVE_REL) return null;
    const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    return {
      seq: v.getUint32(1, true), ts: v.getUint32(5, true),
      dx: v.getInt16(9, true), dy: v.getInt16(11, true), buttons: v.getUint8(13)
    };
  };
  P.decodeMouseButton = function (bytes) {
    if (bytes.length < P.SIZE_MOUSE_BUTTON) return null;
    const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    return {
      seq: v.getUint32(1, true), ts: v.getUint32(5, true),
      button: v.getUint8(9), down: v.getUint8(10) !== 0, buttons: v.getUint8(11)
    };
  };
  P.decodeMouseWheel = function (bytes) {
    if (bytes.length < P.SIZE_MOUSE_WHEEL) return null;
    const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    return {
      seq: v.getUint32(1, true), ts: v.getUint32(5, true),
      dx: v.getInt32(9, true), dy: v.getInt32(13, true)
    };
  };
  P.decodeKey = function (bytes) {
    if (bytes.length < P.SIZE_KEY) return null;
    const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    const bitmap = new P.KeyBitmap();
    bitmap.bytes.set(bytes.subarray(12, 12 + P.KEY_BITMAP_BYTES));
    return {
      seq: v.getUint32(1, true), ts: v.getUint32(5, true),
      usage: v.getUint16(9, true), down: v.getUint8(11) !== 0, state: bitmap
    };
  };
  P.decodeKeyStateSync = function (bytes) {
    if (bytes.length < P.SIZE_KEY_STATE_SYNC) return null;
    const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    const bitmap = new P.KeyBitmap();
    bitmap.bytes.set(bytes.subarray(10, 10 + P.KEY_BITMAP_BYTES));
    return {
      seq: v.getUint32(1, true), ts: v.getUint32(5, true),
      buttons: v.getUint8(9), state: bitmap
    };
  };
  P.decodeInputPing = function (bytes) {
    if (bytes.length < P.SIZE_INPUT_PING) return null;
    const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    return { seq: v.getUint32(1, true), tsMicro: Number(v.getBigUint64(5, true)) };
  };
  P.decodeTextInput = function (bytes) {
    if (bytes.length < 9) return null;
    const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    const n = v.getUint32(5, true);
    if (n > bytes.length - 9) return null;
    return { seq: v.getUint32(1, true), text: U.fromUtf8(bytes.subarray(9, 9 + n)) };
  };

  // ----------------------------------------------------- coordinates

  /**
   * Map a pixel coordinate into the 0..65535 space used on the wire.
   * Absolute positions are normalized rather than sent in pixels so that a
   * mid-session resolution change cannot misplace the remote cursor.
   */
  P.normalize = function (pixel, size) {
    if (size <= 1) return 0;
    const clamped = U.clamp(pixel, 0, size - 1);
    return Math.round((clamped * 65535) / (size - 1)) & 0xFFFF;
  };

  P.denormalize = function (norm, size) {
    if (size <= 1) return 0;
    return Math.round((norm * (size - 1)) / 65535);
  };

  function clampInt(value, min, max) {
    const n = Math.round(value);
    return n < min ? min : n > max ? max : n;
  }

  AS.Protocol = P;
})(window.AllShare);
