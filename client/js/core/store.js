/*
 * ALL SHARE — settings and paired-device storage.
 *
 * Settings live in localStorage, which Chromium does grant to pages loaded from
 * file:// (verified, not assumed). Every read is defensive: a user can clear
 * site data at any moment, and a browser in a restricted profile can throw on
 * access rather than returning null, so a storage failure must degrade to
 * defaults instead of breaking the app.
 *
 * The paired-device list stored here holds each PC's *public* identity key.
 * That key is a trust anchor: it is what the client checks every SDP signature
 * against, so a change to it is treated the way SSH treats a changed host key.
 */
(function (AS) {
  'use strict';

  const KEY_SETTINGS = 'allshare.settings.v1';
  const KEY_DEVICES = 'allshare.devices.v1';

  const DEFAULTS = {
    rendezvous: '',
    deviceLabel: '',
    theme: 'system',

    // Streaming
    preset: 'balanced',
    resolution: 'auto',
    maxFps: 60,
    maxKbps: 25000,
    adaptive: true,
    codecPreference: 'auto',

    // Latency. "low" keeps a small jitter buffer, which is the safe default:
    // forcing the buffer to zero is measurably worse on anything but a LAN
    // because the decoder starts stalling on ordinary network jitter.
    latencyMode: 'low',

    // Input
    localCursor: true,
    relativeOnLock: true,
    captureAllKeys: true,
    layoutMode: 'scancode',
    mouseSensitivity: 1.0,
    invertScroll: false,
    naturalScroll: false,

    // Audio
    audioEnabled: true,
    audioVolume: 1.0,

    // Clipboard
    clipboardToRemote: true,
    clipboardFromRemote: true,

    // Diagnostics
    showHud: false,
    debugLogging: false,
    // Hidden until deliberately revealed: seven taps on the version line in
    // Settings → General. Off is the right default — the developer tab shows
    // raw connection reports that would only worry the people this is for.
    developerMode: false,
    relayPreference: 'auto'
  };

  function safeGet(key) {
    try {
      return window.localStorage.getItem(key);
    } catch (err) {
      AS.Log.warn('local storage is unavailable; settings will not persist');
      return null;
    }
  }

  function safeSet(key, value) {
    try {
      window.localStorage.setItem(key, value);
      return true;
    } catch (err) {
      AS.Log.warn('could not save settings', err);
      return false;
    }
  }

  const Store = new AS.Util.Emitter();
  let settings = null;
  let devices = null;

  function loadSettings() {
    const config = window.ALLSHARE_CONFIG || {};
    const base = Object.assign({}, DEFAULTS);
    // A shipped config.js provides defaults; anything the user has changed in
    // the app wins over it.
    if (config.rendezvous) base.rendezvous = String(config.rendezvous);
    if (config.deviceLabel) base.deviceLabel = String(config.deviceLabel);

    const raw = safeGet(KEY_SETTINGS);
    if (!raw) return base;
    try {
      const saved = JSON.parse(raw);
      for (const key in DEFAULTS) {
        if (Object.prototype.hasOwnProperty.call(saved, key)) base[key] = saved[key];
      }
    } catch (err) {
      AS.Log.warn('saved settings were unreadable; starting from defaults');
    }
    return base;
  }

  function loadDevices() {
    const raw = safeGet(KEY_DEVICES);
    if (!raw) return [];
    try {
      const parsed = JSON.parse(raw);
      return Array.isArray(parsed) ? parsed.filter(function (d) {
        return d && typeof d.deviceId === 'string' && typeof d.identityPub === 'string';
      }) : [];
    } catch (err) {
      AS.Log.warn('saved PC list was unreadable');
      return [];
    }
  }

  Store.init = function () {
    settings = loadSettings();
    devices = loadDevices();
    AS.Log.setDebug(!!settings.debugLogging);
  };

  Store.get = function (key) {
    if (!settings) Store.init();
    return settings[key];
  };

  Store.all = function () {
    if (!settings) Store.init();
    return Object.assign({}, settings);
  };

  Store.set = function (key, value) {
    if (!settings) Store.init();
    if (settings[key] === value) return;
    settings[key] = value;
    safeSet(KEY_SETTINGS, JSON.stringify(settings));
    if (key === 'debugLogging') AS.Log.setDebug(!!value);
    Store.emit('change', { key: key, value: value });
  };

  Store.setMany = function (patch) {
    if (!settings) Store.init();
    let changed = false;
    for (const key in patch) {
      if (settings[key] !== patch[key]) { settings[key] = patch[key]; changed = true; }
    }
    if (!changed) return;
    safeSet(KEY_SETTINGS, JSON.stringify(settings));
    AS.Log.setDebug(!!settings.debugLogging);
    Store.emit('change', { key: null, value: null });
  };

  Store.reset = function () {
    settings = Object.assign({}, DEFAULTS);
    safeSet(KEY_SETTINGS, JSON.stringify(settings));
    Store.emit('change', { key: null, value: null });
  };

  // ------------------------------------------------------ paired PCs

  Store.devices = function () {
    if (!devices) devices = loadDevices();
    return devices.slice();
  };

  Store.device = function (deviceId) {
    return Store.devices().find(function (d) { return d.deviceId === deviceId; }) || null;
  };

  /**
   * Record a newly paired PC.
   *
   * Re-pairing an existing PC deliberately overwrites the stored identity key:
   * that only happens after a successful mutual key confirmation with a code
   * the user just read off that machine, which is exactly the "I know this
   * changed and I approve" gesture SSH asks for.
   */
  Store.addDevice = function (record) {
    if (!devices) devices = loadDevices();
    const existing = devices.findIndex(function (d) { return d.deviceId === record.deviceId; });
    const entry = {
      deviceId: record.deviceId,
      name: record.name || 'Windows PC',
      identityPub: record.identityPub,
      pairedAt: Date.now(),
      lastConnected: existing >= 0 ? devices[existing].lastConnected : 0
    };
    if (existing >= 0) devices[existing] = entry;
    else devices.push(entry);
    safeSet(KEY_DEVICES, JSON.stringify(devices));
    Store.emit('devices', devices.slice());
    return entry;
  };

  Store.touchDevice = function (deviceId, patch) {
    if (!devices) devices = loadDevices();
    const found = devices.find(function (d) { return d.deviceId === deviceId; });
    if (!found) return;
    Object.assign(found, patch || {}, { lastConnected: Date.now() });
    safeSet(KEY_DEVICES, JSON.stringify(devices));
  };

  Store.removeDevice = function (deviceId) {
    if (!devices) devices = loadDevices();
    devices = devices.filter(function (d) { return d.deviceId !== deviceId; });
    safeSet(KEY_DEVICES, JSON.stringify(devices));
    Store.emit('devices', devices.slice());
  };

  Store.DEFAULTS = DEFAULTS;
  AS.Store = Store;
})(window.AllShare);
