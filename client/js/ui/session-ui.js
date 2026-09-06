/*
 * ALL SHARE — the session view.
 *
 * The most important thing here is the locally drawn cursor. The PC excludes
 * the pointer from the video and sends its shape and position separately; this
 * file paints it at the position the browser already knows about, so pointer
 * motion is instant no matter what the round-trip time is. That single decision
 * is the difference between a remote desktop that feels laggy and one that
 * feels responsive, and it costs almost nothing.
 *
 * The second is that nothing permanently covers the desktop: the toolbar and
 * the status pill fade out when the user stops moving, and come back the moment
 * they do.
 */
(function (AS) {
  'use strict';

  const U = AS.Util;
  const UI = AS.UI;
  const P = AS.Protocol;
  const Log = AS.Log;

  const TOOLBAR_IDLE_MS = 2600;
  const HINT_MS = 4000;

  function SessionUI(app) {
    this.app = app;
    this.root = document.querySelector('[data-screen="session"]');
    this.stage = U.el('stage');
    this.video = U.el('video');
    this.cursorCanvas = U.el('cursor-canvas');
    this.toolbar = U.el('toolbar');
    this.statusPill = U.el('statuspill');
    this.statusText = U.el('statuspill-text');
    this.hud = U.el('hud');
    this.hudGrid = U.el('hud-grid');
    this.hudGraph = U.el('hud-graph');
    this.hudFoot = U.el('hud-foot');
    this.overlay = U.el('overlay');
    this.hintEl = U.el('hint');

    this.session = null;
    this.input = null;
    this.device = null;

    this._listeners = new U.Listeners();
    this._idleTimer = 0;
    this._hintTimer = 0;
    this._popover = null;
    this._cursorShapes = new Map();
    this._cursorState = { x: 0, y: 0, visible: true, shapeId: 0, relative: false };
    this._localPointer = { x: 0, y: 0, valid: false };
    this._cursorRaf = 0;
    this._latencyHistory = new U.Ring(120);
    this._e2eHistory = new U.Ring(120);
    this._inputLatency = new U.Ema(0.15);
    this._lastFrameLatency = 0;
    this._latencyMeasuredAt = 0;
    this._lastMeasuredSeq = 0;
    this._audioMuted = false;
  }

  SessionUI.prototype.init = function () {
    const self = this;
    this.toolbar.addEventListener('click', function (event) {
      const button = event.target.closest('[data-action]');
      if (button) self._onToolbarAction(button.getAttribute('data-action'));
    });
    const hudClose = this.hud.querySelector('[data-action="toggle-hud"]');
    if (hudClose) hudClose.addEventListener('click', function () { self.toggleHud(); });
  };

  // ------------------------------------------------------- lifecycle

  SessionUI.prototype.enter = function (device) {
    this.device = device;
    this.root.hidden = false;
    this._resetOverlay();
    this.showOverlay({
      title: 'Connecting to ' + device.name,
      body: '',
      spinner: true,
      steps: [
        { id: 'reach', label: 'Reaching your PC' },
        { id: 'negotiate', label: 'Setting up a secure connection' },
        { id: 'stream', label: 'Starting the picture' }
      ]
    });
    this.setStep('reach', 'active');
    this._bindStageEvents();
    this._startCursorLoop();
    U.show(this.hud, !!AS.Store.get('showHud'));
    this._nudgeToolbar();
  };

  SessionUI.prototype.leave = function () {
    this._listeners.removeAll();
    cancelAnimationFrame(this._cursorRaf);
    this._cursorRaf = 0;
    clearTimeout(this._idleTimer);
    clearTimeout(this._hintTimer);
    this._closePopover();
    this._cursorShapes.clear();
    this._latencyHistory = new U.Ring(120);
    this._e2eHistory = new U.Ring(120);
    this.root.hidden = true;
    this.session = null;
    this.input = null;
    if (document.fullscreenElement) {
      document.exitFullscreen().catch(function () { /* already leaving */ });
    }
  };

  /** Wire a live session and its input controller into the view. */
  SessionUI.prototype.bind = function (session, input) {
    this.session = session;
    this.input = input;
    const self = this;

    session.on('hello', function (hello) { self._onHello(hello); });
    session.on('stats', function (stats) { self._onStats(stats); });
    session.on('cursorShape', function (message) { self._onCursorShape(message); });
    session.on('cursorState', function (message) { self._onCursorState(message); });
    session.on('displayChanged', function (body) { self._onDisplayChanged(body); });
    session.on('notice', function (notice) { self._onNotice(notice); });
    session.on('frameMark', function (event) { self._onFrameMark(event); });
    session.on('pong', function (message) { self._onPong(message); });
    session.on('jitterAdjusted', function (event) {
      Log.info('jitter buffer raised to ' + event.targetMs + 'ms');
    });
    session.on('playblocked', function () {
      self.showOverlay({
        title: 'Ready when you are',
        body: 'Your browser needs a tap before it will start playing.',
        icon: 'monitor',
        actions: [{ label: 'Start', variant: 'primary', onClick: function () {
          self.video.play().catch(function () {});
          self.hideOverlay();
        } }]
      });
    });

    input.on('lockChange', function (event) { self._onLockChange(event); });
    input.on('activity', function () { self._nudgeToolbar(); });
    input.on('emergencyRelease', function () {
      self.hint('Everything released. Press Mouse Lock to capture again.');
    });
    input.on('lockError', function (event) {
      UI.toast({ kind: 'warn', title: 'Mouse Lock unavailable', text: event.message });
    });
    input.on('clipboardSent', function (event) {
      self.hint('Sent ' + event.bytes + ' characters to your PC');
    });
  };

  SessionUI.prototype._bindStageEvents = function () {
    const self = this;
    const L = this._listeners;

    L.add(this.stage, 'pointermove', function (event) {
      self._nudgeToolbar();
      if (!self.input) return;
      if (self.input.pointerLocked) return;
      const rect = self.input.videoRect();
      if (!rect.width) return;
      self._localPointer.x = event.clientX;
      self._localPointer.y = event.clientY;
      self._localPointer.valid = true;
    }, { passive: true });

    L.add(this.stage, 'pointerleave', function () { self._localPointer.valid = false; });
    L.add(window, 'resize', U.debounce(function () { self._onResize(); }, 150));
    L.add(document, 'fullscreenchange', function () { self._syncFullscreenButton(); });

    // A single click on the stage focuses the session, which is what the
    // clipboard and keyboard APIs require before they will deliver anything.
    L.add(this.stage, 'click', function () {
      if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
    });
  };

  // ------------------------------------------------------------ overlay

  SessionUI.prototype._resetOverlay = function () {
    U.clear(U.el('overlay-steps'));
    U.clear(U.el('overlay-actions'));
    U.el('overlay-details').hidden = true;
    U.el('overlay-icon').hidden = true;
    U.el('overlay-spinner').hidden = false;
  };

  /**
   * Show the overlay. `steps` renders the connection progress list; `actions`
   * renders buttons; `detail` goes behind "Advanced details" and is the only
   * place technical text is ever shown.
   */
  SessionUI.prototype.showOverlay = function (options) {
    this._resetOverlay();
    this.overlay.hidden = false;
    this.overlay.classList.remove('is-fading');

    U.el('overlay-title').textContent = options.title || '';
    U.el('overlay-body').textContent = options.body || '';
    U.el('overlay-spinner').hidden = !options.spinner;

    const iconEl = U.el('overlay-icon');
    if (options.icon) {
      iconEl.className = 'overlay-card__icon' + (options.iconKind ? ' is-' + options.iconKind : '');
      iconEl.innerHTML = AS.Icons.get(options.icon);
      iconEl.hidden = false;
    }

    const stepsEl = U.el('overlay-steps');
    if (options.steps) {
      options.steps.forEach(function (step) {
        stepsEl.appendChild(U.h('div', { class: 'ostep', 'data-step': step.id }, [
          U.h('span', { class: 'ostep__dot' }),
          U.h('span', { text: step.label })
        ]));
      });
    }

    const actionsEl = U.el('overlay-actions');
    if (options.actions) {
      options.actions.forEach(function (action) {
        actionsEl.appendChild(U.h('button', {
          class: 'btn ' + (action.variant ? 'btn--' + action.variant : 'btn--ghost'),
          text: action.label,
          onclick: action.onClick
        }));
      });
    }

    if (options.detail) {
      U.el('overlay-details').hidden = false;
      U.el('overlay-detail-text').textContent = options.detail;
    }
  };

  SessionUI.prototype.hideOverlay = function () {
    const overlay = this.overlay;
    overlay.classList.add('is-fading');
    setTimeout(function () {
      if (overlay.classList.contains('is-fading')) overlay.hidden = true;
    }, 240);
  };

  SessionUI.prototype.setStep = function (id, state) {
    const node = this.overlay.querySelector('[data-step="' + id + '"]');
    if (!node) return;
    node.classList.remove('is-active', 'is-done');
    if (state === 'active') node.classList.add('is-active');
    else if (state === 'done') {
      node.classList.add('is-done');
      const dot = node.querySelector('.ostep__dot');
      if (dot) dot.innerHTML = AS.Icons.get('check');
    }
  };

  // -------------------------------------------------------- agent data

  SessionUI.prototype._onHello = function (hello) {
    this.setStep('reach', 'done');
    this.setStep('negotiate', 'done');
    this.setStep('stream', 'active');

    if (hello.sessionKind === 'lockscreen' || hello.sessionKind === 'login') {
      this.hint('This PC is locked. Sign in as you normally would.');
    }
    const displaysBtn = U.el('displays-btn');
    U.show(displaysBtn, !!(hello.monitors && hello.monitors.length > 1));

    // Tell the PC what we can show, so it can size the stream to avoid any
    // resampling — the single biggest factor in how sharp text looks.
    this._sendViewport();
    this.app.pushQuality();

    setTimeout(() => {
      if (this.session && this.session.state === 'connected') this.hideOverlay();
    }, 250);
  };

  SessionUI.prototype._onDisplayChanged = function (body) {
    if (this.session) this.session.hello = Object.assign(this.session.hello || {}, body);
    const displaysBtn = U.el('displays-btn');
    U.show(displaysBtn, !!(body.monitors && body.monitors.length > 1));
    this.hint('Display layout changed');
  };

  SessionUI.prototype._onNotice = function (notice) {
    UI.toast({
      kind: notice.severity === 'error' ? 'error' : notice.severity === 'warning' ? 'warn' : 'info',
      title: notice.message,
      text: notice.detail || ''
    });
  };

  SessionUI.prototype._onPong = function (message) {
    const sentAt = this.input && this.input.lastInputSentAt.get(message.seq);
    if (sentAt) this._inputLatency.push(U.now() - sentAt);
  };

  SessionUI.prototype._onFrameMark = function (event) {
    // The PC tags each encoded frame with the input sequence it reflects. When
    // the browser tells us that exact frame reached the screen, the difference
    // is a genuine end-to-end "key press to pixels" number rather than a
    // network round trip.
    //
    // Each input is measured exactly once, on the first frame that reflects it.
    // Without that, an idle moment re-reports the same stale input against
    // every later frame and the figure climbs for no reason — which is a
    // measurement bug, not latency.
    const seq = event.mark.inputSeq;
    if (!seq || seq === this._lastMeasuredSeq) return;
    const sentAt = this.input && this.input.lastInputSentAt.get(seq);
    if (!sentAt) return;
    this._lastMeasuredSeq = seq;

    const latency = event.displayedAt - sentAt;
    if (latency > 0 && latency < 3000) {
      this._lastFrameLatency = latency;
      this._latencyMeasuredAt = event.displayedAt;
      this._e2eHistory.push(latency);
    }
  };

  /**
   * The most recent end-to-end measurement, or 0 if it is too old to show.
   *
   * A number from several seconds ago describes a connection that may no longer
   * exist, so the status pill falls back to the network estimate rather than
   * displaying something stale.
   */
  SessionUI.prototype.freshLatency = function () {
    if (!this._lastFrameLatency) return 0;
    if (U.now() - this._latencyMeasuredAt > 4000) return 0;
    return this._lastFrameLatency;
  };

  // ---------------------------------------------------------- cursor

  SessionUI.prototype._onCursorShape = function (message) {
    const meta = message.meta || {};
    if (!message.image || !message.image.length) return;
    // A Blob URL keeps the decode off the main thread and avoids the 33% cost
    // of base64. Old shapes are revoked so a long session cannot leak them.
    const blob = new Blob([message.image], { type: 'image/png' });
    const url = URL.createObjectURL(blob);
    const image = new Image();
    const self = this;
    image.onload = function () {
      const previous = self._cursorShapes.get(meta.id);
      if (previous && previous.url) URL.revokeObjectURL(previous.url);
      self._cursorShapes.set(meta.id, {
        image: image, url: url,
        hotX: meta.hx || 0, hotY: meta.hy || 0,
        width: meta.w || image.width, height: meta.h || image.height
      });
      if (self._cursorShapes.size > 64) {
        const oldestKey = self._cursorShapes.keys().next().value;
        const oldest = self._cursorShapes.get(oldestKey);
        if (oldest && oldest.url) URL.revokeObjectURL(oldest.url);
        self._cursorShapes.delete(oldestKey);
      }
    };
    image.onerror = function () { URL.revokeObjectURL(url); };
    image.src = url;
  };

  SessionUI.prototype._onCursorState = function (message) {
    this._cursorState = message;
    // While the remote application has captured the pointer, its position is
    // authoritative and the local prediction has to stop.
    this.stage.classList.toggle('is-hiding-cursor', !!AS.Store.get('localCursor'));
  };

  SessionUI.prototype._startCursorLoop = function () {
    const self = this;
    const draw = function () {
      self._drawCursor();
      self._cursorRaf = requestAnimationFrame(draw);
    };
    this._cursorRaf = requestAnimationFrame(draw);
  };

  SessionUI.prototype._drawCursor = function () {
    const canvas = this.cursorCanvas;
    if (!canvas || !this.session) return;

    const rect = this.stage.getBoundingClientRect();
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const width = Math.round(rect.width * dpr);
    const height = Math.round(rect.height * dpr);
    if (canvas.width !== width || canvas.height !== height) {
      canvas.width = width;
      canvas.height = height;
    }
    const ctx = canvas.getContext('2d');
    ctx.clearRect(0, 0, canvas.width, canvas.height);

    if (!AS.Store.get('localCursor')) return;
    if (!this._cursorState.visible) return;

    const shape = this._cursorShapes.get(this._cursorState.shapeId);
    if (!shape) return;

    const videoRect = this.input ? this.input.videoRect() : null;
    if (!videoRect || !videoRect.width) return;

    let screenX, screenY;
    if (this.input && this.input.pointerLocked) {
      // Locked: there is no local pointer to follow, so the PC's reported
      // position is the only truth available.
      screenX = videoRect.left + (this._cursorState.x / 65535) * videoRect.width;
      screenY = videoRect.top + (this._cursorState.y / 65535) * videoRect.height;
    } else if (this._localPointer.valid && !this._cursorState.relative) {
      // Unlocked: draw where the user's hand actually is. This is what removes
      // the round trip from perceived pointer latency.
      screenX = this._localPointer.x;
      screenY = this._localPointer.y;
    } else {
      screenX = videoRect.left + (this._cursorState.x / 65535) * videoRect.width;
      screenY = videoRect.top + (this._cursorState.y / 65535) * videoRect.height;
    }

    // Scale the cursor by the same factor the video is scaled by, so it matches
    // what is on screen rather than floating at native size over a resized image.
    const scale = videoRect.width / videoRect.videoWidth;
    const drawW = shape.width * scale;
    const drawH = shape.height * scale;
    const x = (screenX - rect.left - shape.hotX * scale) * dpr;
    const y = (screenY - rect.top - shape.hotY * scale) * dpr;

    ctx.imageSmoothingEnabled = scale < 1;
    try {
      ctx.drawImage(shape.image, x, y, drawW * dpr, drawH * dpr);
    } catch (err) {
      /* an image that failed to decode is simply not drawn */
    }
  };

  // ----------------------------------------------------------- stats

  SessionUI.prototype._onStats = function (stats) {
    this._latencyHistory.push(stats.rttMs);

    const inputMs = this._inputLatency.get(0);
    const shown = this.freshLatency() || stats.e2eMs ||
      (stats.rttMs + stats.decodeMs + stats.jitterBufferMs);

    this.statusPill.classList.toggle('is-poor', stats.quality === 'fair');
    this.statusPill.classList.toggle('is-bad', stats.quality === 'poor');
    this.statusText.textContent = [
      stats.height ? stats.height + 'p' : '',
      stats.fps ? Math.round(stats.fps) + ' fps' : '',
      shown ? Math.round(shown) + ' ms' : ''
    ].filter(Boolean).join(' · ');

    if (!this.hud.hidden) this._renderHud(stats, inputMs, shown);
  };

  SessionUI.prototype._renderHud = function (stats, inputMs, e2eMs) {
    const cells = [
      ['Frame rate', Math.round(stats.fps) + ' fps', stats.fps < 24 ? 'warn' : ''],
      ['Resolution', stats.width ? stats.width + '×' + stats.height : '—', ''],
      ['End to end', e2eMs ? Math.round(e2eMs) + ' ms' : '—', e2eMs > 120 ? 'warn' : e2eMs > 220 ? 'bad' : ''],
      ['Network', Math.round(stats.rttMs) + ' ms', stats.rttMs > 120 ? 'warn' : ''],
      ['Input', inputMs ? Math.round(inputMs) + ' ms' : '—', ''],
      ['Data rate', U.formatBitrate(stats.bitrateKbps * 1000), ''],
      ['Packet loss', stats.lossPercent.toFixed(1) + '%', stats.lossPercent > 2 ? 'bad' : stats.lossPercent > 0.5 ? 'warn' : ''],
      ['Buffer', Math.round(stats.jitterBufferMs) + ' ms', ''],
      ['Decode', stats.decodeMs.toFixed(1) + ' ms', stats.decodeMs > 12 ? 'warn' : ''],
      ['PC encode', stats.encodeMs ? stats.encodeMs.toFixed(1) + ' ms' : '—', ''],
      ['Freezes', String(stats.freezes), stats.freezes > 0 ? 'warn' : ''],
      ['Dropped', String(stats.framesDropped), stats.framesDropped > 0 ? 'warn' : '']
    ];

    U.clear(this.hudGrid);
    const self = this;
    cells.forEach(function (cell) {
      self.hudGrid.appendChild(U.h('div', { class: 'hud__cell' }, [
        U.h('div', { class: 'hud__k', text: cell[0] }),
        U.h('div', { class: 'hud__v' + (cell[2] ? ' is-' + cell[2] : ''), text: cell[1] })
      ]));
    });

    const route = stats.relayed === null ? 'connecting'
      : stats.relayed ? 'through a relay' : 'direct (' + (stats.candidateType || 'peer to peer') + ')';
    const decode = stats.hardwareDecode === null ? 'unknown'
      : stats.hardwareDecode ? 'hardware' : 'software';
    this.hudFoot.textContent = [
      (stats.codec || 'negotiating') + ' · ' + decode + ' decode',
      'route: ' + route,
      stats.decoder || '',
      'nack ' + stats.nacks + ' · pli ' + stats.plis + (stats.qp ? ' · qp ' + Math.round(stats.qp) : '')
    ].filter(Boolean).join('\n');

    this._drawGraph();
  };

  SessionUI.prototype._drawGraph = function () {
    const canvas = this.hudGraph;
    if (!canvas) return;
    const ctx = canvas.getContext('2d');
    const width = canvas.width;
    const height = canvas.height;
    ctx.clearRect(0, 0, width, height);

    const series = [
      { data: this._e2eHistory.items, color: 'rgba(79,125,255,.95)' },
      { data: this._latencyHistory.items, color: 'rgba(154,107,255,.7)' }
    ];
    const peak = Math.max(60, this._e2eHistory.max(), this._latencyHistory.max());

    // Reference lines at the thresholds where interaction starts to feel wrong.
    ctx.strokeStyle = 'rgba(255,255,255,.10)';
    ctx.lineWidth = 1;
    [50, 100, 200].forEach(function (ms) {
      if (ms > peak) return;
      const y = height - (ms / peak) * height;
      ctx.beginPath();
      ctx.moveTo(0, y);
      ctx.lineTo(width, y);
      ctx.stroke();
    });

    series.forEach(function (line) {
      if (line.data.length < 2) return;
      ctx.strokeStyle = line.color;
      ctx.lineWidth = 1.5;
      ctx.beginPath();
      line.data.forEach(function (value, index) {
        const x = (index / (line.data.length - 1)) * width;
        const y = height - Math.min(1, value / peak) * height;
        if (index === 0) ctx.moveTo(x, y);
        else ctx.lineTo(x, y);
      });
      ctx.stroke();
    });

    ctx.fillStyle = 'rgba(255,255,255,.4)';
    ctx.font = '9px ui-monospace, monospace';
    ctx.fillText(Math.round(peak) + ' ms', 2, 9);
  };

  // --------------------------------------------------------- toolbar

  SessionUI.prototype._onToolbarAction = function (action) {
    switch (action) {
      case 'toggle-lock': this.toggleLock(); break;
      case 'toggle-keyboard': this.toggleAllKeys(); break;
      case 'clipboard': this.showClipboardMenu(); break;
      case 'toggle-audio': this.toggleAudio(); break;
      case 'quality': this.showQualityMenu(); break;
      case 'displays': this.showDisplayMenu(); break;
      case 'toggle-hud': this.toggleHud(); break;
      case 'fullscreen': this.toggleFullscreen(); break;
      case 'disconnect': this.app.disconnect('user disconnected'); break;
      default: break;
    }
    this._nudgeToolbar();
  };

  SessionUI.prototype.toggleLock = async function () {
    if (!this.input) return;
    if (this.input.pointerLocked) {
      this.input.exitLock();
      return;
    }
    const result = await this.input.enterLock({});
    if (!result.pointer) {
      UI.toast({
        kind: 'warn',
        title: 'Mouse Lock did not start',
        text: 'Your browser refused to capture the mouse. Try clicking the picture first.'
      });
      return;
    }
    if (!result.keyboard) {
      this.hint('Mouse locked. Press Esc to release.');
    } else {
      this.hint('Mouse locked. Hold Esc to release.');
    }
    if (result.accelerated) {
      UI.toast({
        kind: 'info',
        title: 'Using smoothed mouse movement',
        text: 'This browser would not provide raw movement, so aiming may feel slightly different from sitting at the PC.'
      });
    }
  };

  SessionUI.prototype._onLockChange = function (event) {
    const button = this.toolbar.querySelector('[data-action="toggle-lock"]');
    if (button) button.classList.toggle('is-on', event.locked);
    this.stage.classList.toggle('is-locked', event.locked);
    if (!event.locked) this._nudgeToolbar();
  };

  SessionUI.prototype.toggleAllKeys = function () {
    const next = !AS.Store.get('captureAllKeys');
    AS.Store.set('captureAllKeys', next);
    if (this.input) this.input.settings.captureAllKeys = next;
    const button = this.toolbar.querySelector('[data-action="toggle-keyboard"]');
    if (button) button.classList.toggle('is-on', next);
    this.hint(next
      ? 'All keys go to your PC. Go fullscreen for Alt+Tab and the Windows key.'
      : 'Browser shortcuts work normally again.');
  };

  SessionUI.prototype.toggleAudio = function () {
    this._audioMuted = !this._audioMuted;
    this.video.muted = this._audioMuted;
    const button = this.toolbar.querySelector('[data-action="toggle-audio"]');
    if (button) {
      button.classList.toggle('is-muted', this._audioMuted);
      const icon = button.querySelector('[data-icon]');
      if (icon) icon.innerHTML = AS.Icons.get(this._audioMuted ? 'volume-off' : 'volume');
    }
    this.hint(this._audioMuted ? 'Sound off' : 'Sound on');
  };

  SessionUI.prototype.toggleHud = function () {
    const next = this.hud.hidden;
    this.hud.hidden = !next;
    AS.Store.set('showHud', next);
    const button = this.toolbar.querySelector('[data-action="toggle-hud"]');
    if (button) button.classList.toggle('is-on', next);
  };

  SessionUI.prototype.toggleFullscreen = function () {
    if (document.fullscreenElement) {
      document.exitFullscreen().catch(function () { /* already leaving */ });
    } else {
      this.stage.requestFullscreen({ navigationUI: 'hide' }).catch(function (err) {
        UI.toast({ kind: 'warn', title: 'Fullscreen was refused', text: String(err && err.message || err) });
      });
    }
  };

  SessionUI.prototype._syncFullscreenButton = function () {
    const button = this.toolbar.querySelector('[data-action="fullscreen"]');
    if (!button) return;
    const isFull = !!document.fullscreenElement;
    button.classList.toggle('is-on', isFull);
    const icon = button.querySelector('[data-icon]');
    if (icon) icon.innerHTML = AS.Icons.get(isFull ? 'collapse' : 'expand');
    this._sendViewport();
  };

  SessionUI.prototype._onResize = function () { this._sendViewport(); };

  SessionUI.prototype._sendViewport = function () {
    if (!this.session) return;
    const rect = this.stage.getBoundingClientRect();
    const dpr = window.devicePixelRatio || 1;
    this.session.sendCtrl(P.TYPE_VIEWPORT, {
      width: Math.round(rect.width * dpr),
      height: Math.round(rect.height * dpr),
      dpr: dpr,
      decodeBudgetMs: this.session.stats.decodeMs || 0
    });
  };

  // -------------------------------------------------------- popovers

  SessionUI.prototype._closePopover = function () {
    if (this._popover && this._popover.parentNode) this._popover.parentNode.removeChild(this._popover);
    this._popover = null;
  };

  SessionUI.prototype._openPopover = function (title, content) {
    this._closePopover();
    const node = U.h('div', { class: 'popover' }, [
      U.h('div', { class: 'popover__title', text: title }),
      content
    ]);
    this.stage.appendChild(node);
    this._popover = node;

    const self = this;
    const close = function (event) {
      if (node.contains(event.target)) return;
      document.removeEventListener('pointerdown', close, true);
      self._closePopover();
    };
    setTimeout(function () { document.addEventListener('pointerdown', close, true); }, 0);
  };

  SessionUI.prototype.showQualityMenu = function () {
    const self = this;
    const list = U.h('div', { class: 'choicelist' });
    const presets = [
      { value: 'gaming', icon: 'gamepad', title: 'Gaming', desc: 'Lowest latency, highest frame rate' },
      { value: 'balanced', icon: 'scale', title: 'Balanced', desc: 'A good default for most things' },
      { value: 'desktop', icon: 'desktop', title: 'Desktop', desc: 'Sharpest text for reading and writing' }
    ];
    const current = AS.Store.get('preset');
    presets.forEach(function (preset) {
      list.appendChild(U.h('button', {
        class: 'choice', 'aria-pressed': preset.value === current ? 'true' : 'false',
        onclick: function () {
          AS.Store.set('preset', preset.value);
          self.app.pushQuality();
          self._closePopover();
          self._updateQualityLabel();
          self.hint(preset.title + ' mode');
        }
      }, [
        U.h('span', { class: 'choice__icon', html: AS.Icons.get(preset.icon) }),
        U.h('span', { class: 'choice__main' }, [
          U.h('span', { class: 'choice__title', text: preset.title }),
          U.h('span', { class: 'choice__desc', text: preset.desc })
        ])
      ]));
    });

    const wrap = U.h('div', {}, [
      list,
      U.h('div', { class: 'popover__title', style: { marginTop: '14px', paddingTop: '12px', borderTop: '1px solid var(--border)' }, text: 'Picture size' }),
      UI.segmented({
        value: AS.Store.get('resolution'),
        options: [
          { value: 'auto', label: 'Auto' },
          { value: 'native', label: 'Original' },
          { value: 'fit', label: 'Fit' },
          { value: '1080', label: '1080p' },
          { value: '720', label: '720p' }
        ],
        onChange: function (value) { AS.Store.set('resolution', value); self.app.pushQuality(); }
      }),
      U.h('div', { class: 'popover__row', style: { marginTop: '10px' } }, [
        U.h('span', { class: 'popover__label', text: 'More settings' }),
        U.h('button', {
          class: 'btn btn--tiny', text: 'Open',
          onclick: function () { self._closePopover(); AS.SettingsUI.open(self.app, 'streaming'); }
        })
      ])
    ]);
    this._openPopover('Quality', wrap);
  };

  SessionUI.prototype._updateQualityLabel = function () {
    const label = U.el('quality-label');
    if (!label) return;
    const names = { gaming: 'Gaming', desktop: 'Desktop', balanced: 'Balanced', custom: 'Custom' };
    label.textContent = names[AS.Store.get('preset')] || 'Quality';
  };

  SessionUI.prototype.showDisplayMenu = function () {
    const hello = (this.session && this.session.hello) || {};
    const monitors = hello.monitors || [];
    if (!monitors.length) return;
    const self = this;

    const list = U.h('div', { class: 'choicelist' });
    monitors.forEach(function (monitor) {
      list.appendChild(U.h('button', {
        class: 'choice', 'aria-pressed': monitor.id === hello.activeMonitor ? 'true' : 'false',
        onclick: function () {
          self.session.sendCtrl(P.TYPE_SELECT_MONITOR, { monitorId: monitor.id });
          self._closePopover();
          self.hint('Switching to ' + (monitor.name || 'display ' + (monitor.id + 1)));
        }
      }, [
        U.h('span', { class: 'choice__icon', html: AS.Icons.get('monitor') }),
        U.h('span', { class: 'choice__main' }, [
          U.h('span', { class: 'choice__title', text: monitor.name || 'Display ' + (monitor.id + 1) }),
          U.h('span', {
            class: 'choice__desc',
            text: monitor.width + '×' + monitor.height +
                  (monitor.refreshHz ? ' · ' + monitor.refreshHz + ' Hz' : '') +
                  (monitor.primary ? ' · main display' : '')
          })
        ])
      ]));
    });
    this._openPopover('Displays', list);
  };

  SessionUI.prototype.showClipboardMenu = function () {
    const self = this;
    const wrap = U.h('div', {}, [
      U.h('p', { class: 'popover__sub', style: { marginBottom: '10px' },
        text: 'Copy and paste as you normally would. Text moves with you.' }),
      U.h('div', { class: 'choicelist' }, [
        U.h('button', {
          class: 'choice',
          onclick: async function () {
            self._closePopover();
            try {
              const text = await navigator.clipboard.readText();
              if (!text) { self.hint('Nothing to send — your clipboard is empty.'); return; }
              self.session.sendCtrl(P.TYPE_CLIPBOARD_IN, { text: text.slice(0, 65536) });
              self.hint('Clipboard sent to your PC');
            } catch (err) {
              UI.toast({
                kind: 'info',
                title: 'Press Ctrl+V instead',
                text: 'Your browser will only share the clipboard when you paste it yourself.'
              });
            }
          }
        }, [
          U.h('span', { class: 'choice__icon', html: AS.Icons.get('clipboard') }),
          U.h('span', { class: 'choice__main' }, [
            U.h('span', { class: 'choice__title', text: 'Send my clipboard to the PC' }),
            U.h('span', { class: 'choice__desc', text: 'Or just press Ctrl+V in the session' })
          ])
        ]),
        U.h('button', {
          class: 'choice',
          onclick: function () {
            self._closePopover();
            AS.SettingsUI.open(self.app, 'sound');
          }
        }, [
          U.h('span', { class: 'choice__icon', html: AS.Icons.get('gear') }),
          U.h('span', { class: 'choice__main' }, [
            U.h('span', { class: 'choice__title', text: 'Clipboard settings' })
          ])
        ])
      ])
    ]);
    this._openPopover('Clipboard', wrap);
  };

  // ------------------------------------------------------------ chrome

  SessionUI.prototype._nudgeToolbar = function () {
    const self = this;
    this.toolbar.classList.remove('is-hidden');
    this.statusPill.classList.remove('is-hidden');
    clearTimeout(this._idleTimer);
    this._idleTimer = setTimeout(function () {
      if (self._popover) return;
      self.toolbar.classList.add('is-hidden');
      if (self.hud.hidden) self.statusPill.classList.add('is-hidden');
    }, TOOLBAR_IDLE_MS);
  };

  SessionUI.prototype.hint = function (text) {
    const node = this.hintEl;
    node.textContent = text;
    node.hidden = false;
    node.classList.remove('is-out');
    clearTimeout(this._hintTimer);
    this._hintTimer = setTimeout(function () {
      node.classList.add('is-out');
      setTimeout(function () { node.hidden = true; }, 240);
    }, HINT_MS);
  };

  AS.SessionUI = SessionUI;
})(window.AllShare);
