/*
 * ALL SHARE — the WebRTC session.
 *
 * Why WebRTC media tracks rather than a data channel feeding WebCodecs:
 *
 *  - WebRTC is the only browser transport that can traverse NAT. WebTransport
 *    and WebSocket both need a reachable server, so either would force every
 *    byte of video through a relay. ICE gives a direct path most of the time,
 *    which is both faster and cheaper.
 *  - A media track gets the browser's own hardware decoder, packet loss
 *    recovery, congestion control and frame pacing — all of which would
 *    otherwise have to be rebuilt, worse, in JavaScript.
 *  - A <video> element is composited on the GPU. Decoding to a VideoFrame and
 *    painting it to a canvas adds a copy and a frame of latency for no benefit.
 *
 * The remaining downside of a media track — the jitter buffer — is handled
 * directly below, and adaptively rather than by forcing it to zero, because
 * measurements from projects that tried that show it trades smooth playback for
 * a latency win that a small buffer already delivers.
 *
 * Input and control travel on data channels alongside, exactly as cloud gaming
 * services do.
 */
(function (AS) {
  'use strict';

  const U = AS.Util;
  const Log = AS.Log;
  const P = AS.Protocol;

  // Jitter buffer targets in milliseconds.
  //
  // Zero is deliberately not offered. A target pinned at zero starves the
  // decoder on ordinary network jitter and shows up as stutter, most visibly at
  // high resolutions; a small buffer removes that for a couple of milliseconds
  // of added latency. "ultra" starts near zero and the controller below raises
  // it if the stream actually starts freezing.
  const JITTER_TARGETS = { ultra: 0, low: 25, balanced: 60, smooth: 120 };

  const STATS_INTERVAL_MS = 1000;
  const CONNECT_TIMEOUT_MS = 30000;
  const SIGNAL_SKEW_MS = 120000;

  function Session(options) {
    U.Emitter.call(this);
    this.rv = options.rendezvous;
    this.device = options.device;          // { deviceId, name, identityPub }
    this.settings = options.settings || {};
    this.videoEl = options.videoEl;

    this.sessionId = '';
    this.state = 'idle';
    this.hello = null;
    this.stats = emptyStats();

    this._pc = null;
    this._ctrl = null;
    this._input = null;
    this._peerKeyBytes = null;
    this._listeners = new U.Listeners();
    this._statsTimer = 0;
    this._connectTimer = 0;
    this._pendingCandidates = [];
    this._remoteSet = false;
    this._closed = false;
    this._seenNonces = new Set();

    this._frameCbHandle = 0;
    this._frameMarks = new Map();
    this._lastStatsReport = null;
    this._decodeEma = new U.Ema(0.2);
    this._e2eEma = new U.Ema(0.15);
    this._rttEma = new U.Ema(0.2);
    this._jitterTarget = JITTER_TARGETS[this.settings.latencyMode] ?? JITTER_TARGETS.low;
    this._freezeBaseline = null;
    this._jitterRaises = 0;
  }
  Session.prototype = Object.create(U.Emitter.prototype);
  Session.prototype.constructor = Session;

  function emptyStats() {
    return {
      fps: 0, width: 0, height: 0, codec: '', decoder: '', hardwareDecode: null,
      bitrateKbps: 0, packetsLost: 0, lossPercent: 0, nacks: 0, plis: 0,
      jitterMs: 0, jitterBufferMs: 0, decodeMs: 0, rttMs: 0, e2eMs: 0, inputMs: 0,
      candidateType: '', relayed: null, freezes: 0, framesDropped: 0,
      captureMs: 0, encodeMs: 0, agentFps: 0, targetKbps: 0, qp: 0, quality: 'unknown'
    };
  }

  Session.prototype._setState = function (state, detail) {
    if (this.state === state) return;
    this.state = state;
    Log.info('session state', state, detail || '');
    this.emit('state', { state: state, detail: detail || null });
  };

  /**
   * Describe what this browser can actually decode.
   *
   * Sent to the PC so it can pick a codec its hardware can encode *and* this
   * browser can decode, rather than either side assuming. This is why the same
   * client works on a Chromebook that has H.264 and on a Chromium build that
   * does not.
   */
  Session.prototype.codecCapabilities = function () {
    const out = [];
    const collect = function (kind) {
      let caps = null;
      try {
        caps = RTCRtpReceiver.getCapabilities(kind);
      } catch (err) {
        return;
      }
      if (!caps || !caps.codecs) return;
      for (const codec of caps.codecs) {
        const mime = (codec.mimeType || '').toLowerCase();
        // Repair and forward-error-correction payloads are not content codecs.
        if (/\/(rtx|red|ulpfec|flexfec-03|cn|telephone-event)$/.test(mime)) continue;
        out.push({
          mimeType: codec.mimeType,
          clockRate: codec.clockRate,
          channels: codec.channels || 0,
          sdpFmtpLine: codec.sdpFmtpLine || ''
        });
      }
    };
    collect('video');
    collect('audio');

    const preference = this.settings.codecPreference;
    if (preference && preference !== 'auto') {
      // Move the user's chosen family to the front; the PC still has the final
      // say, because it can only send what its encoder supports.
      const wanted = preference.toLowerCase();
      out.sort(function (a, b) {
        const aMatch = a.mimeType.toLowerCase().indexOf(wanted) >= 0 ? 0 : 1;
        const bMatch = b.mimeType.toLowerCase().indexOf(wanted) >= 0 ? 0 : 1;
        return aMatch - bMatch;
      });
    }
    return out;
  };

  /** Begin a session. Resolves once the peer connection is established. */
  Session.prototype.connect = async function (options) {
    const opts = options || {};
    this._closed = false;
    this._setState('requesting');

    try {
      this._peerKeyBytes = U.fromB64(this.device.identityPub);
    } catch (err) {
      throw fail('pairing_broken', 'This PC needs to be paired again.',
        'The stored identity key for this PC could not be read.');
    }

    const reply = await this.rv.request('connect', {
      target: this.device.deviceId,
      codecs: this.codecCapabilities(),
      label: this.settings.deviceLabel || U.guessDeviceLabel(),
      reconnect: !!opts.reconnect,
      priorSession: opts.priorSession || ''
    });
    this.sessionId = (reply.body && reply.body.sessionId) || '';
    if (!this.sessionId) {
      throw fail('no_session', 'We could not start a session with that PC.', 'The service returned no session id.');
    }
    Log.info('session requested', this.sessionId);
    this._setState('negotiating');
    this._bindSignalling();

    const self = this;
    clearTimeout(this._connectTimer);
    this._connectTimer = setTimeout(function () {
      if (self.state !== 'connected' && !self._closed) {
        self.emit('failed', fail('timeout',
          'We could not reach your PC.',
          'The connection was not established within ' + (CONNECT_TIMEOUT_MS / 1000) + ' seconds.'));
      }
    }, CONNECT_TIMEOUT_MS);
  };

  Session.prototype._bindSignalling = function () {
    const self = this;
    this._onSignal = function (body) { self._handleSignal(body); };
    this._onSessionEnd = function (body) {
      if (body.sessionId && body.sessionId !== self.sessionId) return;
      self.emit('ended', { reason: body.reason || 'The PC ended the session.' });
    };
    this.rv.on('signal', this._onSignal);
    this.rv.on('sessionEnd', this._onSessionEnd);
  };

  Session.prototype._unbindSignalling = function () {
    if (this._onSignal) this.rv.off('signal', this._onSignal);
    if (this._onSessionEnd) this.rv.off('sessionEnd', this._onSessionEnd);
    this._onSignal = null;
    this._onSessionEnd = null;
  };

  Session.prototype._handleSignal = async function (body) {
    if (this._closed) return;
    if (body.sessionId && body.sessionId !== this.sessionId) return;

    let payload;
    try {
      payload = await this.rv.verifyPayload(this._peerKeyBytes, this.sessionId,
        this.device.deviceId, body, SIGNAL_SKEW_MS);
    } catch (err) {
      // A signature that does not check out is the one failure that must never
      // be retried or worked around: it is what a rendezvous impersonating the
      // PC would look like.
      Log.error('rejected an unsigned or mis-signed signalling message', err);
      this.emit('failed', fail('identity_mismatch',
        'We could not verify that this is your PC.',
        (err && err.message) || 'The signalling message failed signature verification.'));
      this.close();
      return;
    }

    // Each payload carries a unique nonce; replaying one within a session is
    // refused rather than acted on twice.
    if (payload.nonce) {
      if (this._seenNonces.has(payload.nonce)) {
        Log.warn('ignoring a repeated signalling message');
        return;
      }
      this._seenNonces.add(payload.nonce);
      if (this._seenNonces.size > 512) this._seenNonces.clear();
    }

    try {
      if (payload.kind === 'offer') await this._onOffer(payload);
      else if (payload.kind === 'candidate') await this._onCandidate(payload);
      else if (payload.kind === 'bye') this.emit('ended', { reason: payload.reason || 'The PC ended the session.' });
    } catch (err) {
      Log.error('signalling failed', err);
      this.emit('failed', fail('negotiation_failed',
        'We could not set up the connection to your PC.', String(err && err.message || err)));
    }
  };

  Session.prototype._onOffer = async function (payload) {
    const pc = this._createPeerConnection();
    await pc.setRemoteDescription({ type: 'offer', sdp: payload.sdp });
    this._remoteSet = true;
    for (const candidate of this._pendingCandidates.splice(0)) {
      try { await pc.addIceCandidate(candidate); } catch (err) { Log.debug('late candidate rejected', err); }
    }

    // The stream is receive-only. Saying so explicitly avoids Chrome offering
    // to send anything and keeps the SDP small.
    for (const transceiver of pc.getTransceivers()) {
      if (transceiver.direction === 'sendrecv') transceiver.direction = 'recvonly';
    }

    const answer = await pc.createAnswer();
    await pc.setLocalDescription(answer);
    await this._sendSignal({ kind: 'answer', sdp: pc.localDescription.sdp });
    this._setState('connecting');
  };

  Session.prototype._onCandidate = async function (payload) {
    const candidate = {
      candidate: payload.candidate,
      sdpMid: payload.sdpMid,
      sdpMLineIndex: payload.sdpMLineIndex
    };
    if (!this._pc || !this._remoteSet) {
      this._pendingCandidates.push(candidate);
      return;
    }
    try {
      await this._pc.addIceCandidate(candidate);
    } catch (err) {
      Log.debug('candidate rejected', err);
    }
  };

  Session.prototype._sendSignal = async function (payload) {
    payload.nonce = U.randomId();
    payload.ts = Date.now();
    const signed = await this.rv.signPayload(this.sessionId, this.device.deviceId, payload);
    this.rv.send('signal', signed);
  };

  Session.prototype._createPeerConnection = function () {
    const self = this;
    const config = {
      iceServers: this.rv.iceServers,
      // A pool warms candidates before they are needed, shaving a round trip
      // off connection setup.
      iceCandidatePoolSize: 2,
      bundlePolicy: 'max-bundle',
      rtcpMuxPolicy: 'require'
    };
    if (this.settings.relayPreference === 'relay') config.iceTransportPolicy = 'relay';

    const pc = new RTCPeerConnection(config);
    this._pc = pc;

    pc.onicecandidate = function (event) {
      if (!event.candidate) return;
      self._sendSignal({
        kind: 'candidate',
        candidate: event.candidate.candidate,
        sdpMid: event.candidate.sdpMid,
        sdpMLineIndex: event.candidate.sdpMLineIndex
      }).catch(function (err) { Log.debug('could not send a candidate', err); });
    };

    pc.oniceconnectionstatechange = function () {
      Log.debug('ice state', pc.iceConnectionState);
      if (pc.iceConnectionState === 'failed') {
        self.emit('failed', fail('ice_failed',
          'We could not find a way to reach your PC.',
          'ICE negotiation failed. This usually means both networks block direct connections and no relay was available.'));
      }
    };

    pc.onconnectionstatechange = function () {
      const state = pc.connectionState;
      Log.info('peer connection', state);
      if (state === 'connected') {
        clearTimeout(self._connectTimer);
        self._setState('connected');
        self._startStats();
      } else if (state === 'failed') {
        self.emit('failed', fail('connection_failed',
          'The connection to your PC failed.', 'The peer connection entered the failed state.'));
      } else if (state === 'disconnected') {
        self._setState('interrupted');
      } else if (state === 'closed' && !self._closed) {
        self.emit('ended', { reason: 'The connection closed.' });
      }
    };

    pc.ontrack = function (event) {
      if (event.track.kind === 'video') self._attachVideo(event);
      else if (event.track.kind === 'audio') self._attachAudio(event);
      self._tuneReceiver(event.receiver);
    };

    pc.ondatachannel = function (event) {
      const channel = event.channel;
      if (channel.label === 'ctrl') self._attachCtrl(channel);
      else if (channel.label === 'input') self._attachInput(channel);
      else channel.close();
    };

    return pc;
  };

  /**
   * Set the receiver's jitter buffer target.
   *
   * jitterBufferTarget is the modern property; playoutDelayHint is kept as a
   * fallback for older Chromium. Both are hints — the browser will not starve
   * itself — which is exactly why a small non-zero target is the right default.
   */
  Session.prototype._tuneReceiver = function (receiver) {
    if (!receiver) return;
    const target = this._jitterTarget;
    try {
      if ('jitterBufferTarget' in receiver) receiver.jitterBufferTarget = target;
      else if ('playoutDelayHint' in receiver) receiver.playoutDelayHint = target / 1000;
      Log.debug('jitter buffer target set to ' + target + 'ms');
    } catch (err) {
      Log.debug('could not set the jitter buffer target', err);
    }
  };

  Session.prototype.setLatencyMode = function (mode) {
    this._jitterTarget = JITTER_TARGETS[mode] ?? JITTER_TARGETS.low;
    this._jitterRaises = 0;
    this._freezeBaseline = null;
    if (!this._pc) return;
    for (const receiver of this._pc.getReceivers()) {
      if (receiver.track && receiver.track.kind === 'video') this._tuneReceiver(receiver);
    }
  };

  Session.prototype._attachVideo = function (event) {
    const video = this.videoEl;
    if (!video) return;
    video.srcObject = event.streams[0] || new MediaStream([event.track]);
    const self = this;
    video.play().catch(function (err) {
      // Autoplay of a muted-by-default stream is normally allowed; if it is
      // blocked, the UI turns this into a "Tap to start" affordance.
      Log.warn('video playback was blocked', err);
      self.emit('playblocked', {});
    });
    this._startFrameCallbacks();
    this.emit('video', { track: event.track });
  };

  Session.prototype._attachAudio = function (event) {
    // The audio track rides in the same MediaStream as the video, so attaching
    // the stream to the video element plays both and keeps them in sync.
    this.emit('audio', { track: event.track });
  };

  Session.prototype._attachCtrl = function (channel) {
    const self = this;
    channel.binaryType = 'arraybuffer';
    this._ctrl = channel;
    channel.onopen = function () {
      Log.info('control channel open');
      self.emit('ctrlopen', {});
    };
    channel.onclose = function () { Log.info('control channel closed'); };
    channel.onmessage = function (event) { self._onCtrlMessage(event.data); };
  };

  Session.prototype._attachInput = function (channel) {
    channel.binaryType = 'arraybuffer';
    this._input = channel;
    const self = this;
    channel.onopen = function () {
      Log.info('input channel open', 'ordered=' + channel.ordered, 'maxRetransmits=' + channel.maxRetransmits);
      self.emit('inputopen', {});
    };
    channel.onmessage = function (event) { self._onCtrlMessage(event.data); };
  };

  Session.prototype._onCtrlMessage = function (data) {
    const message = P.decode(data instanceof ArrayBuffer ? new Uint8Array(data) : data);
    if (!message) {
      Log.warn('the PC sent a control message that could not be read');
      return;
    }
    switch (message.type) {
      case P.TYPE_HELLO:
        this.hello = message.body;
        Log.info('agent hello', message.body.deviceName, message.body.codec, message.body.encoder);
        this.emit('hello', message.body);
        break;
      case P.TYPE_STATS:
        this._lastStatsReport = message.body;
        break;
      case P.TYPE_FRAME_MARK:
        // Keep a bounded map so a stream of marks cannot grow without limit.
        this._frameMarks.set(message.rtpTimestamp, message);
        if (this._frameMarks.size > 300) {
          const oldest = this._frameMarks.keys().next().value;
          this._frameMarks.delete(oldest);
        }
        break;
      case P.TYPE_CURSOR_STATE:
        this.emit('cursorState', message);
        break;
      case P.TYPE_CURSOR_SHAPE:
        this.emit('cursorShape', message);
        break;
      case P.TYPE_CLIPBOARD_OUT:
        this.emit('clipboard', message.body);
        break;
      case P.TYPE_DISPLAY_CHANGED:
        this.emit('displayChanged', message.body);
        break;
      case P.TYPE_NOTICE:
        this.emit('notice', message.body);
        break;
      case P.TYPE_CONTROL_STATE:
        this.emit('controlState', message.body);
        break;
      case P.TYPE_PONG:
        this.emit('pong', message);
        break;
      default:
        Log.debug('ignoring control message type 0x' + message.type.toString(16));
    }
  };

  // --------------------------------------------------------- sending

  /** Send on the unreliable input channel. Drops silently when not open. */
  Session.prototype.sendInput = function (bytes) {
    const channel = this._input;
    if (!channel || channel.readyState !== 'open') return false;
    // A backlog on the input channel means old positions are queued ahead of
    // new ones, which feels worse than dropping. Skip rather than pile up.
    if (channel.bufferedAmount > 64 * 1024) return false;
    try {
      channel.send(bytes);
      return true;
    } catch (err) {
      return false;
    }
  };

  /** Send on the reliable control channel. */
  Session.prototype.sendCtrl = function (type, value) {
    const channel = this._ctrl;
    if (!channel || channel.readyState !== 'open') return false;
    try {
      channel.send(P.encodeJSON(type, value));
      return true;
    } catch (err) {
      Log.warn('could not send a control message', err);
      return false;
    }
  };

  Session.prototype.sendCtrlBytes = function (bytes) {
    const channel = this._ctrl;
    if (!channel || channel.readyState !== 'open') return false;
    try {
      channel.send(bytes);
      return true;
    } catch (err) {
      return false;
    }
  };

  Session.prototype.requestKeyframe = function () {
    return this.sendCtrl(P.TYPE_REQUEST_KEYFRAME, {});
  };

  // ----------------------------------------------------------- stats

  Session.prototype._startFrameCallbacks = function () {
    const video = this.videoEl;
    if (!video || !('requestVideoFrameCallback' in video)) return;
    const self = this;

    const onFrame = function (now, metadata) {
      if (self._closed) return;
      // metadata.rtpTimestamp ties this displayed frame back to the exact
      // frame the PC captured, which is what makes a true end-to-end
      // "capture to photons" measurement possible inside a browser.
      if (metadata && metadata.rtpTimestamp !== undefined) {
        const mark = self._frameMarks.get(metadata.rtpTimestamp >>> 0);
        if (mark) {
          self._frameMarks.delete(metadata.rtpTimestamp >>> 0);
          self.emit('frameMark', { mark: mark, metadata: metadata, displayedAt: now });
        }
      }
      if (metadata && metadata.processingDuration) {
        self._decodeEma.push(metadata.processingDuration * 1000);
      }
      if (metadata && metadata.captureTime && metadata.expectedDisplayTime) {
        // captureTime arrives only when the sender includes the absolute
        // capture time header extension; when present it is the most honest
        // latency figure available.
        const e2e = metadata.expectedDisplayTime - metadata.captureTime;
        if (e2e > 0 && e2e < 2000) self._e2eEma.push(e2e);
      }
      self._frameCbHandle = video.requestVideoFrameCallback(onFrame);
    };
    this._frameCbHandle = video.requestVideoFrameCallback(onFrame);
  };

  Session.prototype._startStats = function () {
    const self = this;
    clearInterval(this._statsTimer);
    this._statsTimer = setInterval(function () { self._collectStats(); }, STATS_INTERVAL_MS);
    this._collectStats();
  };

  Session.prototype._collectStats = async function () {
    if (!this._pc || this._closed) return;
    let report;
    try {
      report = await this._pc.getStats();
    } catch (err) {
      return;
    }

    const stats = emptyStats();
    let inbound = null;
    let audioInbound = null;
    const byId = new Map();
    report.forEach(function (s) { byId.set(s.id, s); });

    report.forEach(function (s) {
      if (s.type === 'inbound-rtp' && s.kind === 'video') inbound = s;
      else if (s.type === 'inbound-rtp' && s.kind === 'audio') audioInbound = s;
      else if (s.type === 'candidate-pair' && (s.nominated || s.state === 'succeeded')) {
        if (s.currentRoundTripTime !== undefined) stats.rttMs = s.currentRoundTripTime * 1000;
        const local = byId.get(s.localCandidateId);
        const remote = byId.get(s.remoteCandidateId);
        if (local) {
          stats.candidateType = local.candidateType || '';
          stats.relayed = local.candidateType === 'relay' ||
            (remote && remote.candidateType === 'relay') || false;
        }
      }
    });

    if (inbound) {
      stats.fps = inbound.framesPerSecond || 0;
      stats.width = inbound.frameWidth || 0;
      stats.height = inbound.frameHeight || 0;
      stats.decoder = inbound.decoderImplementation || '';
      // Chromium prefixes a hardware decoder's implementation string; it is the
      // only reliable signal that the GPU, not the CPU, is doing the work.
      stats.hardwareDecode = stats.decoder
        ? /hardware|ExternalDecoder|VDAVideo|Vaapi|D3D|MediaFoundation/i.test(stats.decoder)
        : null;
      stats.packetsLost = inbound.packetsLost || 0;
      stats.nacks = inbound.nackCount || 0;
      stats.plis = inbound.pliCount || 0;
      stats.jitterMs = (inbound.jitter || 0) * 1000;
      stats.freezes = inbound.freezeCount || 0;
      stats.framesDropped = inbound.framesDropped || 0;

      if (inbound.jitterBufferDelay !== undefined && inbound.jitterBufferEmittedCount) {
        stats.jitterBufferMs = (inbound.jitterBufferDelay / inbound.jitterBufferEmittedCount) * 1000;
      }
      if (inbound.totalDecodeTime !== undefined && inbound.framesDecoded) {
        stats.decodeMs = (inbound.totalDecodeTime / inbound.framesDecoded) * 1000;
      }
      if (inbound.codecId) {
        const codec = byId.get(inbound.codecId);
        if (codec) stats.codec = (codec.mimeType || '').replace(/^video\//, '');
      }

      const previous = this._prevInbound;
      if (previous && inbound.timestamp > previous.timestamp) {
        const seconds = (inbound.timestamp - previous.timestamp) / 1000;
        const bytes = (inbound.bytesReceived || 0) - (previous.bytesReceived || 0);
        stats.bitrateKbps = seconds > 0 ? (bytes * 8) / seconds / 1000 : 0;

        const lostDelta = (inbound.packetsLost || 0) - (previous.packetsLost || 0);
        const recvDelta = (inbound.packetsReceived || 0) - (previous.packetsReceived || 0);
        const total = lostDelta + recvDelta;
        stats.lossPercent = total > 0 ? Math.max(0, (lostDelta / total) * 100) : 0;
      }
      this._prevInbound = inbound;
      this._adaptJitterBuffer(inbound);
    }

    if (audioInbound) stats.hasAudio = true;

    stats.rttMs = this._rttEma.push(stats.rttMs) || stats.rttMs;
    stats.decodeMs = this._decodeEma.get(stats.decodeMs);
    stats.e2eMs = this._e2eEma.get(0);

    const agent = this._lastStatsReport;
    if (agent) {
      stats.captureMs = agent.capMs || 0;
      stats.encodeMs = agent.encMs || 0;
      stats.agentFps = agent.encFps || 0;
      stats.targetKbps = agent.targetKbps || 0;
      stats.qp = agent.qp || 0;
    }

    stats.quality = gradeQuality(stats);
    this.stats = stats;
    this.emit('stats', stats);
  };

  /**
   * Nudge the jitter buffer when reality disagrees with the chosen target.
   *
   * The ultra-low setting is where this matters: pinning the buffer near zero
   * is only free on a very stable link. If freezes start accumulating, the
   * target is raised in small steps — three times at most, so a genuinely bad
   * network does not slowly become a high-latency one.
   */
  Session.prototype._adaptJitterBuffer = function (inbound) {
    if (this._jitterTarget >= JITTER_TARGETS.balanced) return;
    const freezes = inbound.freezeCount || 0;
    if (this._freezeBaseline === null) { this._freezeBaseline = freezes; return; }
    if (freezes - this._freezeBaseline < 2) return;

    this._freezeBaseline = freezes;
    if (this._jitterRaises >= 3) return;
    this._jitterRaises++;
    this._jitterTarget = Math.min(JITTER_TARGETS.balanced, this._jitterTarget + 20);
    Log.info('raising the jitter buffer to ' + this._jitterTarget + 'ms after repeated freezes');
    for (const receiver of this._pc.getReceivers()) {
      if (receiver.track && receiver.track.kind === 'video') this._tuneReceiver(receiver);
    }
    this.emit('jitterAdjusted', { targetMs: this._jitterTarget });
  };

  function gradeQuality(stats) {
    if (stats.lossPercent > 5 || stats.rttMs > 250) return 'poor';
    if (stats.lossPercent > 1.5 || stats.rttMs > 120 || stats.freezes > 0) return 'fair';
    if (stats.rttMs > 0 && stats.rttMs < 40 && stats.lossPercent < 0.3) return 'excellent';
    return 'good';
  }

  // ----------------------------------------------------------- close

  Session.prototype.close = function (reason) {
    if (this._closed) return;
    this._closed = true;
    clearInterval(this._statsTimer);
    clearTimeout(this._connectTimer);
    this._listeners.removeAll();
    this._unbindSignalling();
    this._frameMarks.clear();

    if (this.videoEl) {
      try {
        if (this._frameCbHandle && this.videoEl.cancelVideoFrameCallback) {
          this.videoEl.cancelVideoFrameCallback(this._frameCbHandle);
        }
      } catch (err) { /* the element may already be detached */ }
      this.videoEl.srcObject = null;
    }
    for (const channel of [this._ctrl, this._input]) {
      if (channel) { try { channel.close(); } catch (err) { /* ignore */ } }
    }
    if (this._pc) {
      try { this._pc.close(); } catch (err) { /* ignore */ }
      this._pc = null;
    }
    if (this.sessionId && this.rv.isConnected()) {
      this.rv.send('sessionEnd', { sessionId: this.sessionId, reason: reason || 'client closed' });
    }
    this._setState('closed', reason);
  };

  function fail(code, message, detail) {
    const err = new Error(message);
    err.code = code;
    err.userMessage = message;
    err.detail = detail || '';
    return err;
  }

  Session.JITTER_TARGETS = JITTER_TARGETS;
  AS.Session = Session;
  AS.sessionFailure = fail;
})(window.AllShare);
