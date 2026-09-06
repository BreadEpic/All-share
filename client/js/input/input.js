/*
 * ALL SHARE — input capture and forwarding.
 *
 * Design notes that matter:
 *
 *  Mouse Lock is Pointer Lock plus Keyboard Lock plus fullscreen, requested
 *  together. That combination is not decoration: with Keyboard Lock active in
 *  fullscreen, Chrome switches Escape from "exit pointer lock immediately" to
 *  "press and hold to exit", which is the only way a tap of Escape can reach
 *  the remote machine at all. Pointer Lock is also requested with
 *  unadjustedMovement so the OS pointer acceleration curve is bypassed, which
 *  is what makes aiming in a game feel right.
 *
 *  Stuck keys are prevented structurally rather than by cleanup. Every key
 *  packet carries the complete held-key bitmap, so the PC always applies an
 *  authoritative snapshot; a dropped packet self-corrects on the next one. On
 *  top of that, an all-clear snapshot is sent on blur, on tab hide, on pointer
 *  lock exit and on session end, and a low-rate heartbeat repeats the snapshot
 *  while anything is held.
 *
 *  Absolute pointer motion is coalesced to one packet per frame because only
 *  the newest position matters. Relative motion sends every coalesced sample,
 *  because deltas accumulate and dropping them loses real movement — that is
 *  what preserves a high-polling-rate mouse.
 */
(function (AS) {
  'use strict';

  const U = AS.Util;
  const P = AS.Protocol;
  const K = AS.Keymap;
  const Log = AS.Log;

  // Button indices on the wire. Chosen to match the order Windows thinks in.
  const BTN_LEFT = 0, BTN_RIGHT = 1, BTN_MIDDLE = 2, BTN_X1 = 3, BTN_X2 = 4;
  const BUTTON_BITS = [P.BUTTON_LEFT, P.BUTTON_RIGHT, P.BUTTON_MIDDLE, P.BUTTON_X1, P.BUTTON_X2];
  // DOM PointerEvent.button → our index.
  const DOM_BUTTON_TO_INDEX = { 0: BTN_LEFT, 1: BTN_MIDDLE, 2: BTN_RIGHT, 3: BTN_X1, 4: BTN_X2 };

  const KEY_HEARTBEAT_MS = 250;
  const MAX_COALESCED = 32;
  const PING_INTERVAL_MS = 1000;

  function InputController(options) {
    U.Emitter.call(this);
    this.session = options.session;
    this.stage = options.stage;
    this.video = options.video;
    this.settings = options.settings || {};

    this.enabled = false;
    this.pointerLocked = false;
    this.keyboardLocked = false;
    this.relativeMode = false;

    this._seq = 0;
    this._pingSeq = 0;
    this._buttons = 0;
    this._keys = new P.KeyBitmap();
    this._listeners = new U.Listeners();
    this._pendingAbs = null;
    this._flushAbs = null;
    this._heartbeatTimer = 0;
    this._pingTimer = 0;
    this._subPixel = { x: 0, y: 0 };
    this._escDownAt = 0;
    this._lastRect = null;

    // Latency measurement: which input sequence was in flight most recently.
    this.lastInputSentAt = new Map();
  }
  InputController.prototype = Object.create(U.Emitter.prototype);
  InputController.prototype.constructor = InputController;

  InputController.prototype.attach = function () {
    if (this.enabled) return;
    this.enabled = true;
    const self = this;
    const L = this._listeners;
    const stage = this.stage;

    this._flushAbs = U.rafThrottle(function () { self._sendPendingAbsolute(); });

    // Pointer -------------------------------------------------------------
    L.add(stage, 'pointermove', function (e) { self._onPointerMove(e); }, { passive: true });
    L.add(stage, 'pointerdown', function (e) { self._onPointerDown(e); });
    L.add(window, 'pointerup', function (e) { self._onPointerUp(e); });
    L.add(stage, 'wheel', function (e) { self._onWheel(e); }, { passive: false });
    L.add(stage, 'contextmenu', function (e) { e.preventDefault(); });
    L.add(stage, 'auxclick', function (e) { e.preventDefault(); });
    L.add(stage, 'dragstart', function (e) { e.preventDefault(); });

    // Keyboard ------------------------------------------------------------
    L.add(window, 'keydown', function (e) { self._onKeyDown(e); }, { capture: true });
    L.add(window, 'keyup', function (e) { self._onKeyUp(e); }, { capture: true });

    // Anything that can steal focus must release every held key. Losing this
    // is how a remote machine ends up walking forever because W stayed down.
    L.add(window, 'blur', function () { self.releaseAll('window lost focus'); });
    L.add(document, 'visibilitychange', function () {
      if (document.hidden) self.releaseAll('tab hidden');
    });
    L.add(document, 'pointerlockchange', function () { self._onPointerLockChange(); });
    L.add(document, 'pointerlockerror', function () {
      Log.warn('the browser refused to lock the pointer');
      self.emit('lockError', { message: 'Your browser would not let ALL SHARE capture the mouse.' });
    });
    L.add(document, 'fullscreenchange', function () { self._onFullscreenChange(); });

    // Clipboard -----------------------------------------------------------
    // Listening for copy and paste is what lets clipboard sharing work with no
    // permission prompt at all: the events carry the data because the user
    // pressed the key themselves.
    L.add(document, 'paste', function (e) { self._onPaste(e); });
    L.add(document, 'copy', function (e) { self._onCopy(e); });

    // Touch ---------------------------------------------------------------
    L.add(stage, 'touchstart', function (e) { self._onTouch(e, 'start'); }, { passive: false });
    L.add(stage, 'touchmove', function (e) { self._onTouch(e, 'move'); }, { passive: false });
    L.add(stage, 'touchend', function (e) { self._onTouch(e, 'end'); }, { passive: false });

    this._heartbeatTimer = setInterval(function () { self._heartbeat(); }, KEY_HEARTBEAT_MS);
    this._pingTimer = setInterval(function () { self._sendPing(); }, PING_INTERVAL_MS);
    Log.info('input attached');
  };

  InputController.prototype.detach = function () {
    if (!this.enabled) return;
    this.enabled = false;
    this.releaseAll('input detached');
    clearInterval(this._heartbeatTimer);
    clearInterval(this._pingTimer);
    this._listeners.removeAll();
    this.exitLock();
    this.lastInputSentAt.clear();
    Log.info('input detached');
  };

  InputController.prototype._nextSeq = function () {
    this._seq = (this._seq + 1) >>> 0;
    return this._seq;
  };

  InputController.prototype._stamp = function (seq) {
    const now = U.now();
    this.lastInputSentAt.set(seq, now);
    if (this.lastInputSentAt.size > 500) {
      const oldest = this.lastInputSentAt.keys().next().value;
      this.lastInputSentAt.delete(oldest);
    }
    return Math.round(now) >>> 0;
  };

  // ------------------------------------------------------------ pointer

  /**
   * The rectangle the video actually occupies inside the stage.
   *
   * The element is letterboxed, so the drawn area is smaller than the element
   * whenever the aspect ratios differ. Mapping through the element's own box
   * instead would offset every click by the size of the black bars.
   */
  InputController.prototype._videoRect = function () {
    const video = this.video;
    const box = video.getBoundingClientRect();
    const vw = video.videoWidth || 0;
    const vh = video.videoHeight || 0;
    if (!vw || !vh || !box.width || !box.height) return box;

    const scale = Math.min(box.width / vw, box.height / vh);
    const drawnW = vw * scale;
    const drawnH = vh * scale;
    return {
      left: box.left + (box.width - drawnW) / 2,
      top: box.top + (box.height - drawnH) / 2,
      width: drawnW,
      height: drawnH,
      videoWidth: vw,
      videoHeight: vh
    };
  };

  InputController.prototype.videoRect = function () { return this._videoRect(); };

  InputController.prototype._onPointerMove = function (event) {
    if (!this.session) return;

    if (this.pointerLocked) {
      // Relative motion: every coalesced sample carries real displacement, so
      // all of them are forwarded rather than merged into one per frame.
      const samples = coalesced(event);
      const sensitivity = this.settings.mouseSensitivity || 1;
      for (const sample of samples) {
        let dx = (sample.movementX || 0) * sensitivity;
        let dy = (sample.movementY || 0) * sensitivity;
        // Carry the fractional part forward so slow, precise movement is not
        // quantised away by rounding each sample independently.
        dx += this._subPixel.x;
        dy += this._subPixel.y;
        const ix = Math.trunc(dx);
        const iy = Math.trunc(dy);
        this._subPixel.x = dx - ix;
        this._subPixel.y = dy - iy;
        if (ix === 0 && iy === 0) continue;
        const seq = this._nextSeq();
        this.session.sendInput(P.encodeMouseMoveRel(seq, this._stamp(seq), ix, iy, this._buttons));
      }
      return;
    }

    const rect = this._videoRect();
    if (!rect.width || !rect.height) return;
    this._lastRect = rect;
    const nx = P.normalize(((event.clientX - rect.left) / rect.width) * (rect.videoWidth - 1), rect.videoWidth);
    const ny = P.normalize(((event.clientY - rect.top) / rect.height) * (rect.videoHeight - 1), rect.videoHeight);
    // Absolute positions are idempotent, so only the newest one matters.
    this._pendingAbs = { x: nx, y: ny };
    this._flushAbs();
  };

  InputController.prototype._sendPendingAbsolute = function () {
    if (!this._pendingAbs || !this.session) return;
    const seq = this._nextSeq();
    this.session.sendInput(P.encodeMouseMoveAbs(seq, this._stamp(seq),
      this._pendingAbs.x, this._pendingAbs.y, this._buttons));
    this._pendingAbs = null;
  };

  InputController.prototype._onPointerDown = function (event) {
    if (!this.session) return;
    const index = DOM_BUTTON_TO_INDEX[event.button];
    if (index === undefined) return;
    event.preventDefault();
    // Send the position first so the click lands where the user is pointing
    // even if the last move packet was dropped.
    if (!this.pointerLocked) {
      this._onPointerMove(event);
      this._sendPendingAbsolute();
    }
    this._buttons |= BUTTON_BITS[index];
    const seq = this._nextSeq();
    this.session.sendInput(P.encodeMouseButton(seq, this._stamp(seq), index, true, this._buttons));
    this.emit('activity', {});
  };

  InputController.prototype._onPointerUp = function (event) {
    if (!this.session) return;
    const index = DOM_BUTTON_TO_INDEX[event.button];
    if (index === undefined) return;
    this._buttons &= ~BUTTON_BITS[index];
    const seq = this._nextSeq();
    this.session.sendInput(P.encodeMouseButton(seq, this._stamp(seq), index, false, this._buttons));
  };

  InputController.prototype._onWheel = function (event) {
    if (!this.session) return;
    event.preventDefault();

    // wheelDeltaY is exactly ±120 per notch in Chromium, matching the Windows
    // WHEEL_DELTA the PC will inject, so it is preferred over deltaY, whose
    // units depend on deltaMode and on the input device.
    let dy, dx;
    if (event.wheelDeltaY !== undefined && event.wheelDeltaY !== 0) {
      dy = event.wheelDeltaY;
      dx = event.wheelDeltaX || 0;
    } else {
      const unit = event.deltaMode === 1 ? 40 : event.deltaMode === 2 ? 400 : 1;
      dy = -event.deltaY * unit * (P.WHEEL_TICKS_PER_NOTCH / 100);
      dx = -event.deltaX * unit * (P.WHEEL_TICKS_PER_NOTCH / 100);
    }
    if (this.settings.invertScroll) { dy = -dy; dx = -dx; }
    if (Math.abs(dx) < 1 && Math.abs(dy) < 1) return;

    const seq = this._nextSeq();
    this.session.sendInput(P.encodeMouseWheel(seq, this._stamp(seq), Math.round(dx), Math.round(dy)));
    this.emit('activity', {});
  };

  // ----------------------------------------------------------- keyboard

  InputController.prototype._onKeyDown = function (event) {
    if (!this.session) return;

    // Hold Escape to leave Mouse Lock. Chrome enforces its own hold-to-exit
    // when Keyboard Lock is active; this is the same gesture for the case where
    // Keyboard Lock could not be acquired, so the way out is always the same.
    if (event.code === 'Escape' && this.pointerLocked) {
      if (!this._escDownAt) this._escDownAt = U.now();
      else if (U.now() - this._escDownAt > 600) {
        this.exitLock();
        this.emit('lockReleased', { reason: 'escape held' });
        return;
      }
    }

    // A deliberate panic chord that releases everything, for the case where a
    // remote application has swallowed the pointer and the user is stuck.
    if (event.ctrlKey && event.altKey && event.shiftKey && event.code === 'KeyQ') {
      event.preventDefault();
      this.exitLock();
      this.releaseAll('emergency release');
      this.emit('emergencyRelease', {});
      return;
    }

    if (K.shouldPreventDefault(event, this.settings.captureAllKeys)) event.preventDefault();

    // Windows generates its own key repeat from a held key, so forwarding the
    // browser's repeats would double it.
    if (event.repeat) return;

    const usage = K.usageFor(event, { chromebookTopRow: this.settings.chromebookTopRow });
    if (!usage) {
      Log.debug('no mapping for key', event.code);
      return;
    }
    this._keys.set(usage, true);
    const seq = this._nextSeq();
    this.session.sendInput(P.encodeKey(seq, this._stamp(seq), usage, true, this._keys));
    this.emit('activity', {});
  };

  InputController.prototype._onKeyUp = function (event) {
    if (!this.session) return;
    if (event.code === 'Escape') this._escDownAt = 0;
    if (K.shouldPreventDefault(event, this.settings.captureAllKeys)) event.preventDefault();

    const usage = K.usageFor(event, { chromebookTopRow: this.settings.chromebookTopRow });
    if (!usage) return;
    this._keys.set(usage, false);
    const seq = this._nextSeq();
    this.session.sendInput(P.encodeKey(seq, this._stamp(seq), usage, false, this._keys));
  };

  /** Send an authoritative snapshot of all input state. */
  InputController.prototype.syncState = function () {
    if (!this.session) return;
    const seq = this._nextSeq();
    this.session.sendInput(P.encodeKeyStateSync(seq, this._stamp(seq), this._buttons, this._keys));
  };

  /** Release every key and button, then tell the PC. */
  InputController.prototype.releaseAll = function (reason) {
    const hadInput = this._keys.any() || this._buttons !== 0;
    this._keys.clear();
    this._buttons = 0;
    this._subPixel.x = 0;
    this._subPixel.y = 0;
    this._escDownAt = 0;
    if (!this.session) return;
    // Sent unconditionally: it is cheap, and the one time it matters is exactly
    // the time our idea of the state was already wrong.
    this.syncState();
    if (hadInput) Log.info('released all input', reason || '');
  };

  InputController.prototype._heartbeat = function () {
    // Repeat the snapshot while anything is held. On an unreliable channel this
    // is what guarantees a held key survives a lost packet even if the user
    // then does nothing at all.
    if (this._keys.any() || this._buttons !== 0) this.syncState();
  };

  InputController.prototype._sendPing = function () {
    if (!this.session) return;
    this._pingSeq = (this._pingSeq + 1) >>> 0;
    this.session.sendInput(P.encodeInputPing(this._pingSeq, U.nowMicro()));
  };

  /** Type literal text on the remote machine, bypassing the key mapping. */
  InputController.prototype.sendText = function (text) {
    if (!this.session || !text) return false;
    const trimmed = String(text).slice(0, 8192);
    const seq = this._nextSeq();
    return this.session.sendInput(P.encodeTextInput(seq, trimmed));
  };

  // -------------------------------------------------------------- lock

  /**
   * Enter Mouse Lock.
   *
   * Fullscreen first, because Keyboard Lock requires it; then Keyboard Lock,
   * because it is what turns Escape into a key the remote machine can see; then
   * Pointer Lock with unadjustedMovement for raw, unaccelerated deltas.
   *
   * Each step degrades on its own: without fullscreen there is no Keyboard
   * Lock, and without Keyboard Lock a tap of Escape releases the pointer. The
   * caller is told what was actually acquired so the UI can say so.
   */
  InputController.prototype.enterLock = async function (options) {
    const opts = options || {};
    const result = { fullscreen: false, keyboard: false, pointer: false };

    if (opts.fullscreen !== false && !document.fullscreenElement) {
      try {
        await this.stage.requestFullscreen({ navigationUI: 'hide' });
        result.fullscreen = true;
      } catch (err) {
        Log.warn('fullscreen was refused', err);
      }
    } else {
      result.fullscreen = !!document.fullscreenElement;
    }

    if (result.fullscreen && navigator.keyboard && navigator.keyboard.lock) {
      try {
        await navigator.keyboard.lock();
        this.keyboardLocked = true;
        result.keyboard = true;
      } catch (err) {
        Log.warn('keyboard lock was refused', err);
      }
    }

    try {
      // unadjustedMovement bypasses OS pointer acceleration. Chrome returns a
      // promise; older builds return undefined, so both shapes are handled.
      const request = this.stage.requestPointerLock({ unadjustedMovement: true });
      if (request && typeof request.then === 'function') await request;
      result.pointer = true;
    } catch (err) {
      // A browser that does not know the option rejects rather than ignoring
      // it, so fall back to accelerated movement instead of no lock at all.
      Log.warn('raw pointer movement unavailable, falling back', err);
      try {
        const fallback = this.stage.requestPointerLock();
        if (fallback && typeof fallback.then === 'function') await fallback;
        result.pointer = true;
        result.accelerated = true;
      } catch (err2) {
        Log.error('pointer lock failed', err2);
      }
    }
    return result;
  };

  InputController.prototype.exitLock = function () {
    if (document.pointerLockElement) {
      try { document.exitPointerLock(); } catch (err) { /* already exiting */ }
    }
    if (this.keyboardLocked && navigator.keyboard && navigator.keyboard.unlock) {
      try { navigator.keyboard.unlock(); } catch (err) { /* ignore */ }
      this.keyboardLocked = false;
    }
  };

  InputController.prototype._onPointerLockChange = function () {
    const locked = document.pointerLockElement === this.stage;
    if (locked === this.pointerLocked) return;
    this.pointerLocked = locked;
    this._subPixel.x = 0;
    this._subPixel.y = 0;

    if (this.session) {
      this.session.sendCtrl(P.TYPE_SET_POINTER_MODE, { mode: locked ? 'relative' : 'absolute' });
    }
    if (!locked) {
      // Leaving the lock almost always means the user's attention went
      // elsewhere; anything still held would be stuck.
      this.releaseAll('pointer lock released');
      if (this.keyboardLocked && navigator.keyboard && navigator.keyboard.unlock) {
        try { navigator.keyboard.unlock(); } catch (err) { /* ignore */ }
        this.keyboardLocked = false;
      }
    }
    this.emit('lockChange', { locked: locked });
  };

  InputController.prototype._onFullscreenChange = function () {
    const isFull = !!document.fullscreenElement;
    if (!isFull) {
      this.keyboardLocked = false;
      this.releaseAll('left fullscreen');
    }
    this.emit('fullscreenChange', { fullscreen: isFull });
  };

  // --------------------------------------------------------- clipboard

  InputController.prototype._onPaste = function (event) {
    if (!this.settings.clipboardToRemote || !this.session) return;
    const data = event.clipboardData && event.clipboardData.getData('text/plain');
    if (!data) return;
    // Deliberately not prevented: the key event that triggered this paste has
    // already been forwarded, so the remote application receives Ctrl+V with
    // the right clipboard contents already in place.
    const text = data.slice(0, P.MAX_CTRL_MESSAGE / 2);
    this.session.sendCtrl(P.TYPE_CLIPBOARD_IN, { text: text });
    this.emit('clipboardSent', { bytes: text.length });
    Log.info('sent ' + text.length + ' characters of clipboard text to the PC');
  };

  InputController.prototype._onCopy = function () {
    // Copying inside the client page is not meaningful — the content lives on
    // the PC — but the event confirms the browser will deliver clipboard events
    // here, which the UI uses to decide whether to offer clipboard sharing.
    this.emit('copyObserved', {});
  };

  /** Push text into the local clipboard, for content that came from the PC. */
  InputController.prototype.receiveClipboard = async function (text) {
    if (!this.settings.clipboardFromRemote || !text) return false;
    try {
      await navigator.clipboard.writeText(text);
      this.emit('clipboardReceived', { bytes: text.length });
      return true;
    } catch (err) {
      // Writing needs the document to be focused and, in some configurations,
      // a permission. Failing here is not an error worth interrupting the user
      // for; the UI offers a manual copy instead.
      Log.warn('could not write to the local clipboard', err);
      return false;
    }
  };

  // ------------------------------------------------------------- touch

  /**
   * Touch support, deliberately simple: a Chromebook touchscreen drives the
   * remote pointer absolutely, which is the behaviour that matches what the
   * user sees. Gestures are not invented on top of it.
   */
  InputController.prototype._onTouch = function (event, phase) {
    if (!this.session || this.pointerLocked) return;
    const touch = event.changedTouches && event.changedTouches[0];
    if (!touch) return;
    event.preventDefault();

    const rect = this._videoRect();
    if (!rect.width || !rect.height) return;
    const nx = P.normalize(((touch.clientX - rect.left) / rect.width) * (rect.videoWidth - 1), rect.videoWidth);
    const ny = P.normalize(((touch.clientY - rect.top) / rect.height) * (rect.videoHeight - 1), rect.videoHeight);

    let seq = this._nextSeq();
    this.session.sendInput(P.encodeMouseMoveAbs(seq, this._stamp(seq), nx, ny, this._buttons));

    if (phase === 'start') {
      this._buttons |= P.BUTTON_LEFT;
      seq = this._nextSeq();
      this.session.sendInput(P.encodeMouseButton(seq, this._stamp(seq), BTN_LEFT, true, this._buttons));
    } else if (phase === 'end') {
      this._buttons &= ~P.BUTTON_LEFT;
      seq = this._nextSeq();
      this.session.sendInput(P.encodeMouseButton(seq, this._stamp(seq), BTN_LEFT, false, this._buttons));
    }
  };

  // ------------------------------------------------------------ state

  InputController.prototype.heldKeyCount = function () {
    return this._keys.heldUsages().length;
  };

  InputController.prototype.heldKeyNames = function () {
    return this._keys.heldUsages().map(K.nameFor);
  };

  InputController.prototype.buttonsMask = function () { return this._buttons; };

  function coalesced(event) {
    if (typeof event.getCoalescedEvents !== 'function') return [event];
    let samples;
    try {
      samples = event.getCoalescedEvents();
    } catch (err) {
      return [event];
    }
    if (!samples || samples.length === 0) return [event];
    // Bound the worst case so a pathological burst cannot flood the channel.
    return samples.length > MAX_COALESCED ? samples.slice(-MAX_COALESCED) : samples;
  }

  InputController.BUTTON_INDEX = {
    LEFT: BTN_LEFT, RIGHT: BTN_RIGHT, MIDDLE: BTN_MIDDLE, X1: BTN_X1, X2: BTN_X2
  };
  AS.InputController = InputController;
})(window.AllShare);
