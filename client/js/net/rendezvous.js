/*
 * ALL SHARE — the rendezvous connection, browser side.
 *
 * A WebSocket to the ALL SHARE service, used for presence, pairing and WebRTC
 * signalling. It never carries screen data.
 *
 * Two properties matter here:
 *
 *  - It reconnects forever. A Chromebook lid closes, Wi-Fi roams, a captive
 *    portal intercepts; none of those should require the user to do anything.
 *    Backoff is exponential with jitter and resets after a long-lived
 *    connection, so a transient blip recovers instantly while a real outage
 *    does not turn into a reconnect storm.
 *
 *  - It authenticates cryptographically. The server issues a nonce, this client
 *    signs it with its Ed25519 identity, and the signature is bound to the
 *    server's identity and this connection's role, so a captured signature
 *    cannot be replayed to a different server or to gain a different role.
 */
(function (AS) {
  'use strict';

  const U = AS.Util;
  const Log = AS.Log;

  const SIGNAL_VERSION = 1;
  const BACKOFF_MIN = 1000;
  const BACKOFF_MAX = 30000;
  const BACKOFF_FACTOR = 1.7;
  const REQUEST_TIMEOUT = 20000;
  const STABLE_AFTER = 60000;

  function Rendezvous() {
    U.Emitter.call(this);
    this.url = '';
    this.state = 'idle';   // idle | connecting | connected | retrying
    this.iceServers = [];
    this.turnExpiresAt = 0;
    this.serverVersion = '';
    this.lastError = null;

    this._ws = null;
    this._backoff = BACKOFF_MIN;
    this._retryTimer = 0;
    this._connectedAt = 0;
    this._reqSeq = 0;
    this._waiters = new Map();
    this._shouldRun = false;
    this._authTimer = 0;
  }
  Rendezvous.prototype = Object.create(U.Emitter.prototype);
  Rendezvous.prototype.constructor = Rendezvous;

  /** Start (or restart) the connection loop against url. */
  Rendezvous.prototype.start = function (url) {
    this.url = String(url || '').trim();
    this._shouldRun = true;
    this._backoff = BACKOFF_MIN;
    this._openSocket();
  };

  /** Stop and stay stopped. */
  Rendezvous.prototype.stop = function () {
    this._shouldRun = false;
    clearTimeout(this._retryTimer);
    clearTimeout(this._authTimer);
    this._failWaiters(new Error('disconnected'));
    if (this._ws) {
      try { this._ws.close(1000, 'client stopping'); } catch (err) { /* already closing */ }
      this._ws = null;
    }
    this._setState('idle');
  };

  /** Reconnect right now, ignoring any pending backoff. */
  Rendezvous.prototype.retryNow = function () {
    if (!this._shouldRun) return;
    clearTimeout(this._retryTimer);
    this._backoff = BACKOFF_MIN;
    this._openSocket();
  };

  Rendezvous.prototype.isConnected = function () { return this.state === 'connected'; };

  Rendezvous.prototype._setState = function (state, detail) {
    if (this.state === state) return;
    this.state = state;
    this.emit('state', { state: state, detail: detail || null });
  };

  Rendezvous.prototype._openSocket = function () {
    if (!this._shouldRun || !this.url) return;
    if (this._ws) {
      try { this._ws.close(); } catch (err) { /* ignore */ }
      this._ws = null;
    }

    let ws;
    try {
      ws = new WebSocket(this.url);
    } catch (err) {
      // A malformed address throws synchronously; that is a configuration
      // problem, not a network one, so say so instead of retrying silently.
      this.lastError = { code: 'bad_address', message: 'That service address is not valid.' };
      this._setState('retrying', this.lastError);
      this._scheduleRetry();
      return;
    }
    ws.binaryType = 'arraybuffer';
    this._ws = ws;
    this._setState('connecting');

    const self = this;
    ws.onopen = function () { self._onOpen(ws); };
    ws.onmessage = function (event) { self._onMessage(ws, event); };
    ws.onerror = function () { Log.debug('rendezvous socket error'); };
    ws.onclose = function (event) { self._onClose(ws, event); };
  };

  Rendezvous.prototype._onOpen = function (ws) {
    const self = this;
    Log.debug('rendezvous socket open, starting handshake');
    this._send(ws, 'hello', '', {
      version: SIGNAL_VERSION,
      role: 'client',
      deviceId: AS.Identity.deviceId,
      agent: 'allshare-web'
    });
    // If the server never answers the handshake, fail rather than hang.
    clearTimeout(this._authTimer);
    this._authTimer = setTimeout(function () {
      if (self.state !== 'connected') {
        Log.warn('rendezvous did not complete the handshake in time');
        try { ws.close(4000, 'handshake timeout'); } catch (err) { /* ignore */ }
      }
    }, 20000);
  };

  Rendezvous.prototype._onMessage = function (ws, event) {
    if (ws !== this._ws) return;
    let env;
    try {
      env = JSON.parse(event.data);
    } catch (err) {
      Log.warn('rendezvous sent a message that was not valid JSON');
      return;
    }
    const type = env.t;
    const body = env.b || {};

    if (type === 'challenge') { this._answerChallenge(ws, body); return; }

    if (type === 'authOk') {
      clearTimeout(this._authTimer);
      this.iceServers = Array.isArray(body.iceServers) ? body.iceServers : [];
      this.turnExpiresAt = body.turnExpiresAt || 0;
      this.serverVersion = body.serverVersion || '';
      this.lastError = null;
      this._connectedAt = Date.now();
      this._setState('connected');
      Log.info('rendezvous connected', this.serverVersion, this.iceServers.length + ' ICE servers');
      this.emit('connected', body);
      return;
    }

    // Replies addressed to a pending request resolve it; everything else is an
    // unsolicited event.
    if (env.ref && this._waiters.has(env.ref)) {
      const waiter = this._waiters.get(env.ref);
      this._waiters.delete(env.ref);
      clearTimeout(waiter.timer);
      if (type === 'error') waiter.reject(rendezvousError(body));
      else waiter.resolve({ type: type, body: body, ref: env.ref });
      return;
    }

    if (type === 'error') {
      this.lastError = { code: body.code, message: body.message };
      Log.warn('rendezvous error', body.code, body.message);
      this.emit('error', this.lastError);
      return;
    }

    this.emit(type, body);
    this.emit('message', { type: type, body: body, ref: env.ref || '' });
  };

  Rendezvous.prototype._answerChallenge = async function (ws, challenge) {
    try {
      const nonce = U.fromB64(challenge.nonce);
      const signature = await AS.Identity.sign(AS.Identity.DOMAIN_AUTH,
        [nonce, challenge.serverId || '', 'client']);
      if (ws !== this._ws || ws.readyState !== WebSocket.OPEN) return;
      this._send(ws, 'auth', '', {
        deviceId: AS.Identity.deviceId,
        nonce: challenge.nonce,
        sig: U.toB64(signature)
      });
    } catch (err) {
      Log.error('could not answer the rendezvous challenge', err);
      try { ws.close(4001, 'auth failed'); } catch (e) { /* ignore */ }
    }
  };

  Rendezvous.prototype._onClose = function (ws, event) {
    if (ws !== this._ws) return;
    this._ws = null;
    clearTimeout(this._authTimer);
    const wasConnected = this.state === 'connected';
    this._failWaiters(new Error('the connection to the service was lost'));

    if (!this._shouldRun) { this._setState('idle'); return; }

    // A connection that lasted a while hit a transient problem; start over from
    // the short backoff. One that failed immediately is more likely a
    // configuration or outage problem, so let the backoff grow.
    if (wasConnected && Date.now() - this._connectedAt > STABLE_AFTER) {
      this._backoff = BACKOFF_MIN;
    }
    Log.info('rendezvous disconnected', 'code=' + (event && event.code));
    this.emit('disconnected', { code: event && event.code, wasConnected: wasConnected });
    this._setState('retrying', this.lastError);
    this._scheduleRetry();
  };

  Rendezvous.prototype._scheduleRetry = function () {
    const self = this;
    const wait = this._backoff / 2 + Math.random() * (this._backoff / 2);
    this._backoff = Math.min(this._backoff * BACKOFF_FACTOR, BACKOFF_MAX);
    Log.debug('reconnecting to the service in ' + Math.round(wait) + 'ms');
    clearTimeout(this._retryTimer);
    this._retryTimer = setTimeout(function () { self._openSocket(); }, wait);
    this.emit('retryScheduled', { inMs: wait });
  };

  Rendezvous.prototype._send = function (ws, type, ref, body) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return false;
    const envelope = { t: type };
    if (ref) envelope.ref = ref;
    if (body !== undefined && body !== null) envelope.b = body;
    try {
      ws.send(JSON.stringify(envelope));
      return true;
    } catch (err) {
      Log.warn('could not send to the service', err);
      return false;
    }
  };

  /** Fire-and-forget. */
  Rendezvous.prototype.send = function (type, body) {
    return this._send(this._ws, type, '', body);
  };

  /** Fire-and-forget with an explicit correlation reference. */
  Rendezvous.prototype.sendRef = function (type, ref, body) {
    return this._send(this._ws, type, ref, body);
  };

  /** Send and await the matching reply. Rejects on error or timeout. */
  Rendezvous.prototype.request = function (type, body, timeoutMs) {
    const self = this;
    return new Promise(function (resolve, reject) {
      if (!self.isConnected()) {
        reject(new Error('Not connected to the ALL SHARE service.'));
        return;
      }
      const ref = 'r' + (++self._reqSeq);
      const timer = setTimeout(function () {
        self._waiters.delete(ref);
        reject(new Error('The service did not answer in time.'));
      }, timeoutMs || REQUEST_TIMEOUT);
      self._waiters.set(ref, { resolve: resolve, reject: reject, timer: timer });
      if (!self._send(self._ws, type, ref, body)) {
        clearTimeout(timer);
        self._waiters.delete(ref);
        reject(new Error('Not connected to the ALL SHARE service.'));
      }
    });
  };

  Rendezvous.prototype._failWaiters = function (err) {
    for (const waiter of this._waiters.values()) {
      clearTimeout(waiter.timer);
      waiter.reject(err);
    }
    this._waiters.clear();
  };

  /** Wrap a signalling payload in an end-to-end signature. */
  Rendezvous.prototype.signPayload = async function (sessionId, peerId, payload) {
    const raw = U.utf8(JSON.stringify(payload));
    const signature = await AS.Identity.sign(AS.Identity.DOMAIN_SIGNAL,
      [sessionId, AS.Identity.deviceId, peerId, raw]);
    return { sessionId: sessionId, payload: U.toB64(raw), sig: U.toB64(signature) };
  };

  /**
   * Verify a relayed signalling payload against a pinned peer key.
   *
   * This is the check that makes the rendezvous untrustworthy-by-design: the
   * signature covers the SDP, and the SDP carries the DTLS fingerprint, so a
   * server that rewrites either produces a mismatch and the session is refused.
   */
  Rendezvous.prototype.verifyPayload = async function (peerPublicKeyBytes, sessionId, peerId, body, maxSkewMs) {
    let raw, signature;
    try {
      raw = U.fromB64(body.payload);
      signature = U.fromB64(body.sig);
    } catch (err) {
      throw new Error('The PC sent a signalling message that could not be read.');
    }
    const good = await AS.Identity.verify(peerPublicKeyBytes, AS.Identity.DOMAIN_SIGNAL,
      signature, [sessionId, peerId, AS.Identity.deviceId, raw]);
    if (!good) {
      throw new Error('That message was not signed by the PC you paired with.');
    }
    let payload;
    try {
      payload = JSON.parse(U.fromUtf8(raw));
    } catch (err) {
      throw new Error('The PC sent a signalling message that could not be read.');
    }
    const skew = maxSkewMs === undefined ? 120000 : maxSkewMs;
    if (skew > 0 && payload.ts) {
      const age = Date.now() - payload.ts;
      if (age > skew || age < -skew) {
        throw new Error('That message is too old to be trusted.');
      }
    }
    return payload;
  };

  function rendezvousError(body) {
    const err = new Error(body.message || 'The service reported a problem.');
    err.code = body.code || 'internal';
    err.retryAfter = body.retryAfter || 0;
    return err;
  }

  AS.Rendezvous = Rendezvous;
  AS.rendezvousError = rendezvousError;
})(window.AllShare);
