/*
 * ALL SHARE — application bootstrap.
 *
 * Owns the device model, the rendezvous connection and the session lifecycle,
 * and mediates between the two screens. Reconnection lives here because it has
 * to survive the session object being destroyed and recreated.
 */
(function (AS) {
  'use strict';

  const U = AS.Util;
  const UI = AS.UI;
  const Store = AS.Store;
  const Log = AS.Log;

  // Reconnect schedule, in milliseconds. Deliberately front-loaded: most
  // interruptions are a Wi-Fi hiccup that resolves in a second or two, and a
  // user watching a "Reconnecting…" message is counting.
  const RECONNECT_DELAYS = [800, 1500, 3000, 5000, 8000];

  function App() {
    this.rv = new AS.Rendezvous();
    this.home = new AS.HomeScreen(this);
    this.sessionUI = new AS.SessionUI(this);

    this.session = null;
    this.input = null;
    this.activeDevice = null;

    /** deviceId → merged view of stored + live device state. */
    this._devices = new Map();
    this._reconnectAttempt = 0;
    this._reconnectTimer = 0;
    this._wakePolls = new Map();
    this._deviceRefresh = 0;
  }

  App.prototype.start = async function () {
    Store.init();
    this.applyTheme();
    AS.Icons.hydrate(document);
    this.home.init();
    this.sessionUI.init();

    // Everything below needs the device identity, so it is loaded before the
    // first screen is shown.
    try {
      await AS.Identity.load();
    } catch (err) {
      Log.error('could not set up this device', err);
      UI.modal({
        title: 'ALL SHARE could not start',
        body: 'This browser would not let ALL SHARE create a secure identity for this device. ' +
              'That usually means storage is blocked. Try a normal browser window rather than a private one.',
        dismissable: false
      });
      return;
    }

    this._loadStoredDevices();
    this._wireRendezvous();
    this.home.render();
    this.home.setServiceState(this.rv.state);

    const url = Store.get('rendezvous');
    if (url) this.rv.start(url);

    const self = this;
    window.addEventListener('beforeunload', function () {
      if (self.session) self.session.close('page closing');
    });
    window.addEventListener('online', function () {
      Log.info('the network came back');
      if (Store.get('rendezvous')) self.rv.retryNow();
    });

    Log.info('ALL SHARE ready', 'device=' + AS.Identity.fingerprint);
  };

  // ---------------------------------------------------------- devices

  App.prototype._loadStoredDevices = function () {
    const self = this;
    // Stored PCs are shown immediately, before the service answers, so the app
    // is useful the instant it opens rather than after a network round trip.
    Store.devices().forEach(function (stored) {
      self._devices.set(stored.deviceId, {
        deviceId: stored.deviceId,
        name: stored.name,
        identityPub: stored.identityPub,
        pairedAt: stored.pairedAt,
        lastSeen: stored.lastConnected || 0,
        online: false,
        busy: false,
        known: true,
        wake: { method: 'none' }
      });
    });
    this._refreshFingerprints();
  };

  App.prototype._refreshFingerprints = async function () {
    for (const device of this._devices.values()) {
      if (device.fingerprint || !device.identityPub) continue;
      try {
        device.fingerprint = await AS.Identity.fingerprintOf(U.fromB64(device.identityPub));
      } catch (err) {
        device.fingerprint = '';
      }
    }
  };

  App.prototype.deviceList = function () {
    const list = Array.from(this._devices.values());
    // Online first, then most recently used: the PC the user wants is almost
    // always one of those two.
    list.sort(function (a, b) {
      if (a.online !== b.online) return a.online ? -1 : 1;
      return (b.lastSeen || 0) - (a.lastSeen || 0);
    });
    return list;
  };

  App.prototype.deviceById = function (deviceId) { return this._devices.get(deviceId) || null; };

  App.prototype.refreshDevices = async function () {
    this._loadStoredDevices();
    if (!this.rv.isConnected()) { this.home.render(); return; }
    try {
      const reply = await this.rv.request('listDevices', {});
      const devices = (reply.body && reply.body.devices) || [];
      this._mergeDevices(devices);
    } catch (err) {
      Log.warn('could not list PCs', err);
    }
    this.home.render();
  };

  App.prototype._mergeDevices = function (remote) {
    const self = this;
    remote.forEach(function (info) {
      const existing = self._devices.get(info.deviceId);
      if (!existing) {
        // The service knows about a PC this browser has no stored key for.
        // It is listed so the user understands what they are seeing, but it
        // cannot be connected to without pairing, because there is no pinned
        // identity to verify its signalling against.
        self._devices.set(info.deviceId, Object.assign({}, info, { known: false, identityPub: '' }));
        return;
      }
      Object.assign(existing, {
        name: info.name || existing.name,
        online: info.online,
        busy: info.busy,
        lastSeen: info.lastSeen || existing.lastSeen,
        agentVersion: info.agentVersion,
        os: info.os,
        wake: info.wake || existing.wake
      });
    });
    this._refreshFingerprints();
  };

  App.prototype.forgetDevice = function (deviceId) {
    Store.removeDevice(deviceId);
    this._devices.delete(deviceId);
    if (this.rv.isConnected()) {
      this.rv.request('forgetDevice', { target: deviceId }).catch(function (err) {
        Log.warn('the service could not be told about the removal', err);
      });
    }
    this.home.render();
    UI.toast({ kind: 'ok', title: 'PC removed' });
  };

  // ------------------------------------------------------- rendezvous

  App.prototype._wireRendezvous = function () {
    const self = this;

    this.rv.on('state', function (event) {
      self.home.setServiceState(event.state, event.detail);
      if (event.state === 'connected') self.refreshDevices();
      else self.home.render();
    });

    this.rv.on('deviceUpdate', function (info) {
      const existing = self._devices.get(info.deviceId);
      if (existing) {
        const cameOnline = !existing.online && info.online;
        Object.assign(existing, {
          online: info.online, busy: info.busy, name: info.name || existing.name,
          lastSeen: info.lastSeen || existing.lastSeen, wake: info.wake || existing.wake
        });
        self.home.render();
        if (cameOnline) self._onDeviceCameOnline(existing);
      } else {
        self._mergeDevices([info]);
        self.home.render();
      }
    });

    this.rv.on('wakeStatus', function (status) { self._onWakeStatus(status); });

    this.rv.on('error', function (err) {
      if (err.code === 'rate_limited') {
        UI.toast({ kind: 'warn', title: 'Too many attempts', text: 'Please wait a moment and try again.' });
      }
    });

    this.rv.on('disconnected', function () {
      // A dropped rendezvous does not end a running session: media flows
      // peer to peer and keeps working. Only signalling is unavailable.
      if (self.session && self.session.state === 'connected') {
        Log.info('the service went away but the session is still connected');
      }
    });
  };

  App.prototype.reconnectService = function () {
    const url = Store.get('rendezvous');
    if (!url) { this.home.render(); return; }
    this.rv.stop();
    this.rv.start(url);
  };

  // --------------------------------------------------------- sessions

  App.prototype.connectTo = async function (deviceId, options) {
    const device = this._devices.get(deviceId);
    if (!device) return;

    if (!device.known || !device.identityPub) {
      UI.modal({
        title: 'Pair with ' + device.name + ' first',
        body: 'This device has no saved details for that PC, so ALL SHARE cannot verify it. ' +
              'Open ALL SHARE on the PC, choose "Add a device", and enter the code.',
        actions: [
          { label: 'Not now' },
          { label: 'Add a PC', variant: 'primary', onClick: function () { AS.PairingUI.open(this); }.bind(this) }
        ]
      });
      return;
    }

    const opts = options || {};
    this.activeDevice = device;
    this._reconnectAttempt = opts.isReconnect ? this._reconnectAttempt : 0;

    this.home.hide();
    if (!opts.isReconnect) this.sessionUI.enter(device);

    const session = new AS.Session({
      rendezvous: this.rv,
      device: device,
      settings: Store.all(),
      videoEl: this.sessionUI.video
    });
    this.session = session;

    const input = new AS.InputController({
      session: session,
      stage: this.sessionUI.stage,
      video: this.sessionUI.video,
      settings: Store.all()
    });
    this.input = input;
    this.sessionUI.bind(session, input);
    this._wireSession(session, input);

    try {
      await session.connect({ reconnect: !!opts.isReconnect });
      this.sessionUI.setStep('reach', 'done');
      this.sessionUI.setStep('negotiate', 'active');
    } catch (err) {
      Log.warn('could not start the session', err);
      this._showSessionFailure(err);
    }
  };

  App.prototype._wireSession = function (session, input) {
    const self = this;

    session.on('state', function (event) {
      if (event.state === 'connected') {
        self._reconnectAttempt = 0;
        input.attach();
        Store.touchDevice(self.activeDevice.deviceId, {});
        self.sessionUI.setStep('stream', 'active');
      } else if (event.state === 'interrupted') {
        self.sessionUI.showOverlay({
          title: 'Connection interrupted',
          body: 'Trying to get back to your PC…',
          spinner: true
        });
      }
    });

    session.on('ctrlopen', function () { self.pushQuality(); });

    session.on('clipboard', function (body) {
      if (!body || !body.text) return;
      input.receiveClipboard(body.text).then(function (written) {
        if (written) self.sessionUI.hint('Copied from your PC');
        else {
          UI.toast({
            kind: 'info',
            title: 'Text copied on your PC',
            text: 'Click the picture, then press Ctrl+V here to paste it.',
            timeout: 6000
          });
        }
      });
    });

    session.on('failed', function (err) { self._onSessionDropped(err); });
    session.on('ended', function (event) { self._onSessionEnded(event); });
  };

  App.prototype._onSessionEnded = function (event) {
    if (!this.session) return;
    Log.info('session ended', event.reason);
    this._teardownSession();
    this.sessionUI.showOverlay({
      title: 'Session ended',
      body: event.reason || 'Your PC ended the session.',
      icon: 'info', iconKind: 'warn',
      actions: [
        { label: 'Back to my PCs', onClick: this.goHome.bind(this) },
        { label: 'Reconnect', variant: 'primary', onClick: this._retryNow.bind(this) }
      ]
    });
  };

  /**
   * A dropped session tries to come back on its own before bothering the user.
   *
   * Some failures are not worth retrying: a refused signature means something
   * is impersonating the PC, and a PC that is simply offline needs waking, not
   * another connection attempt.
   */
  App.prototype._onSessionDropped = function (err) {
    const fatal = err && (err.code === 'identity_mismatch' || err.code === 'pairing_broken');
    if (fatal || this._reconnectAttempt >= RECONNECT_DELAYS.length) {
      this._teardownSession();
      this._showSessionFailure(err);
      return;
    }

    const delay = RECONNECT_DELAYS[this._reconnectAttempt];
    this._reconnectAttempt++;
    const attempt = this._reconnectAttempt;
    Log.info('reconnect attempt ' + attempt + ' in ' + delay + 'ms');

    this._teardownSession({ keepScreen: true });
    this.sessionUI.showOverlay({
      title: 'Connection lost',
      body: 'Reconnecting… attempt ' + attempt + ' of ' + RECONNECT_DELAYS.length,
      spinner: true,
      actions: [{ label: 'Stop trying', onClick: this.goHome.bind(this) }]
    });

    const self = this;
    clearTimeout(this._reconnectTimer);
    this._reconnectTimer = setTimeout(function () {
      if (!self.activeDevice) return;
      if (!self.rv.isConnected()) {
        // Without signalling there is no way to renegotiate, so wait for the
        // service rather than burning an attempt that cannot succeed.
        self.rv.retryNow();
        self._reconnectTimer = setTimeout(function () { self._retryNow(); }, 1500);
        return;
      }
      self.connectTo(self.activeDevice.deviceId, { isReconnect: true });
    }, delay);
  };

  App.prototype._retryNow = function () {
    clearTimeout(this._reconnectTimer);
    if (!this.activeDevice) { this.goHome(); return; }
    this._reconnectAttempt = 0;
    this.connectTo(this.activeDevice.deviceId, { isReconnect: true });
  };

  App.prototype._showSessionFailure = function (err) {
    const friendly = UI.friendlyError(err);
    const self = this;
    const actions = [{ label: 'Back to my PCs', onClick: this.goHome.bind(this) }];
    if (!err || err.code !== 'identity_mismatch') {
      actions.push({ label: 'Try again', variant: 'primary', onClick: function () { self._retryNow(); } });
    }
    this.sessionUI.showOverlay({
      title: friendly.title,
      body: friendly.body,
      icon: 'alert',
      iconKind: err && err.code === 'identity_mismatch' ? 'error' : 'warn',
      actions: actions,
      detail: UI.errorDetail(err)
    });
  };

  App.prototype._teardownSession = function (options) {
    clearTimeout(this._reconnectTimer);
    if (this.input) { this.input.detach(); this.input = null; }
    if (this.session) { this.session.close((options && options.reason) || 'client'); this.session = null; }
  };

  App.prototype.disconnect = function (reason) {
    this._reconnectAttempt = RECONNECT_DELAYS.length;
    this._teardownSession({ reason: reason });
    this.goHome();
  };

  App.prototype.goHome = function () {
    clearTimeout(this._reconnectTimer);
    this._teardownSession();
    this.activeDevice = null;
    this.sessionUI.leave();
    this.home.show();
    this.refreshDevices();
  };

  // ------------------------------------------------------------- wake

  App.prototype.wake = async function (deviceId) {
    const device = this._devices.get(deviceId);
    if (!device) return;

    this.home.setBusy(deviceId, { kind: 'waking', label: 'Waking…', progress: true });
    try {
      const reply = await this.rv.request('wake', { target: deviceId });
      this._onWakeStatus(reply.body || {});
    } catch (err) {
      this.home.setBusy(deviceId, null);
      const friendly = UI.friendlyError(err);
      UI.toast({ kind: 'warn', title: friendly.title, text: friendly.body });
    }
  };

  App.prototype._onWakeStatus = function (status) {
    const deviceId = status.target;
    if (!deviceId) return;
    const device = this._devices.get(deviceId);
    if (!device) return;

    if (status.stage === 'online') {
      this.home.setBusy(deviceId, null);
      device.online = true;
      this.home.render();
      this._onDeviceCameOnline(device);
      return;
    }

    if (status.stage === 'unavailable') {
      this.home.setBusy(deviceId, null);
      UI.modal({
        title: "That PC can't be woken from here",
        body: status.message || UI.FRIENDLY_ERRORS.wake_unavailable.body,
        actions: [{ label: 'OK', variant: 'primary' }]
      });
      return;
    }

    // "sent" and "waiting" both mean the same thing to a user: it is coming.
    // The honest part is the estimate, which differs enormously between a LAN
    // peer sending a magic packet and a PC that checks in every fifteen minutes.
    const label = status.estimatedSeconds > 60
      ? 'Waking… up to ' + U.formatDuration(status.estimatedSeconds)
      : 'Waking…';
    this.home.setBusy(deviceId, { kind: 'waking', label: label, progress: true });

    if (status.method === 'checkin' && status.estimatedSeconds > 60) {
      UI.toast({
        kind: 'info',
        title: 'Your PC will wake shortly',
        text: 'It checks in every ' + U.formatDuration(status.estimatedSeconds) +
              '. ALL SHARE will connect automatically as soon as it does.',
        timeout: 9000
      });
    }
    this._startWakePoll(deviceId, status.estimatedSeconds || 60);
  };

  /**
   * Poll for a waking PC.
   *
   * Presence is normally pushed, but a push can be missed across a reconnect,
   * so a slow poll backs it up. It stops at the honest estimate rather than
   * spinning forever.
   */
  App.prototype._startWakePoll = function (deviceId, estimateSeconds) {
    const self = this;
    if (this._wakePolls.has(deviceId)) clearInterval(this._wakePolls.get(deviceId).timer);

    const deadline = Date.now() + Math.max(60, estimateSeconds + 90) * 1000;
    const timer = setInterval(async function () {
      if (Date.now() > deadline) {
        self._stopWakePoll(deviceId);
        self.home.setBusy(deviceId, null);
        UI.toast({
          kind: 'warn',
          title: 'Your PC did not come online',
          text: 'It may need waking at the machine itself.'
        });
        return;
      }
      if (!self.rv.isConnected()) return;
      try {
        const reply = await self.rv.request('listDevices', {});
        self._mergeDevices((reply.body && reply.body.devices) || []);
        const device = self._devices.get(deviceId);
        if (device && device.online) {
          self._stopWakePoll(deviceId);
          self.home.setBusy(deviceId, null);
          self._onDeviceCameOnline(device);
        }
      } catch (err) {
        /* the next tick will try again */
      }
    }, 4000);

    this._wakePolls.set(deviceId, { timer: timer, deadline: deadline });
  };

  App.prototype._stopWakePoll = function (deviceId) {
    const entry = this._wakePolls.get(deviceId);
    if (entry) { clearInterval(entry.timer); this._wakePolls.delete(deviceId); }
  };

  /**
   * A PC we asked to wake has come online: connect straight away, so the whole
   * flow is one button press rather than "wake, wait, notice, press connect".
   */
  App.prototype._onDeviceCameOnline = function (device) {
    if (!this._wakePolls.has(device.deviceId) && !this.home.busyStates.has(device.deviceId)) return;
    this._stopWakePoll(device.deviceId);
    this.home.setBusy(device.deviceId, null);
    if (this.session) return;
    UI.toast({ kind: 'ok', title: device.name + ' is awake', text: 'Connecting…' });
    this.connectTo(device.deviceId);
  };

  // ------------------------------------------------------------ misc

  App.prototype.applyTheme = function () {
    document.documentElement.setAttribute('data-theme', Store.get('theme') || 'system');
    const button = document.querySelector('[data-action="toggle-theme"]');
    if (button) {
      const theme = Store.get('theme');
      const dark = theme === 'dark' ||
        (theme === 'system' && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
      button.innerHTML = AS.Icons.get(dark ? 'sun' : 'moon');
      button.title = dark ? 'Switch to light' : 'Switch to dark';
    }
  };

  App.prototype.toggleTheme = function () {
    const order = ['system', 'dark', 'light'];
    const next = order[(order.indexOf(Store.get('theme')) + 1) % order.length];
    Store.set('theme', next);
    this.applyTheme();
  };

  App.prototype.applyLatencyMode = function () {
    if (this.session) this.session.setLatencyMode(Store.get('latencyMode'));
  };

  /** Push the current quality settings to the PC. */
  App.prototype.pushQuality = function () {
    if (!this.session) return;
    const settings = Store.all();
    const resolution = settings.resolution;

    const body = {
      preset: settings.preset,
      resolution: resolution === '1080' || resolution === '720' ? 'fixed' : resolution,
      maxFps: settings.maxFps,
      maxKbps: settings.maxKbps,
      adaptive: settings.adaptive,
      audioEnabled: settings.audioEnabled
    };
    if (resolution === '1080') { body.width = 1920; body.height = 1080; }
    if (resolution === '720') { body.width = 1280; body.height = 720; }

    this.session.sendCtrl(AS.Protocol.TYPE_SET_QUALITY, body);
    if (this.input) this.input.settings = settings;
    this.session.settings = settings;
    this.applyLatencyMode();
    this.sessionUI._updateQualityLabel();
  };

  // ------------------------------------------------------------ boot

  /** Replace the page with a legible explanation of why nothing will work. */
  function refuseToStart(what, advice) {
    document.body.innerHTML =
      '<div style="padding:3rem;max-width:36rem;margin:0 auto;font:15px/1.6 system-ui">' +
      '<h1 style="font-size:20px">ALL SHARE cannot run in this browser</h1>' +
      '<p>It needs ' + what + '.</p><p>' + advice + '</p></div>';
  }

  async function boot() {
    // Fail early and legibly if the browser is missing something essential,
    // rather than throwing halfway through a connection attempt.
    const missing = [];
    if (typeof RTCPeerConnection === 'undefined') missing.push('WebRTC');
    if (!self.isSecureContext) missing.push('a secure context');
    if (!(self.crypto && self.crypto.subtle)) missing.push('Web Crypto');
    if (typeof WebSocket === 'undefined') missing.push('WebSockets');

    if (missing.length) {
      refuseToStart(missing.join(', '),
        'Please update your browser, or use Chrome.');
      return;
    }

    // Ed25519 is the one capability that cannot be detected by looking: the
    // Web Crypto object exists on every browser here, and only the attempt
    // reveals whether this build knows the algorithm. Chrome shipped it
    // unflagged in version 137 (May 2025), so an older Chromebook reaches this
    // point and would otherwise fail with a bare NotSupportedError at the
    // moment it generated its identity.
    try {
      await crypto.subtle.generateKey({ name: 'Ed25519' }, false, ['sign', 'verify']);
    } catch (err) {
      AS.Log.error('this browser cannot generate an Ed25519 key', err);
      refuseToStart('Ed25519 signatures, which this browser does not support',
        'Chrome and Chromebooks have supported this since Chrome 137, released in ' +
        'May 2025. Updating your browser will fix it. On a Chromebook, open ' +
        'Settings and choose "About ChromeOS", then "Check for updates".');
      return;
    }

    const app = new AS.App();
    AS.app = app;
    app.start().catch(function (err) {
      AS.Log.error('startup failed', err);
    });
  }

  AS.App = App;
  AS.boot = boot;

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () { boot(); });
  } else {
    boot();
  }
})(window.AllShare);
