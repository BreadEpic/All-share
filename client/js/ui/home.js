/*
 * ALL SHARE — the home screen: the list of the user's PCs.
 *
 * Every card answers three questions at a glance: which PC is this, can I use
 * it right now, and what happens if I press the button. A card never shows a
 * control that cannot work — an offline PC with no wake path gets an
 * explanation instead of a Wake button that would fail.
 */
(function (AS) {
  'use strict';

  const U = AS.Util;
  const UI = AS.UI;

  function HomeScreen(app) {
    this.app = app;
    this.root = document.querySelector('[data-screen="home"]');
    this.devicesEl = U.el('devices');
    this.emptyEl = U.el('empty');
    this.setupEl = U.el('setup');
    this.bannerEl = U.el('conn-banner');
    this.bannerText = U.el('conn-banner-text');
    this.footerStatus = U.el('footer-status');

    /** deviceId → transient UI state (waking, connecting). */
    this.busyStates = new Map();
    this._cards = new Map();
  }

  HomeScreen.prototype.init = function () {
    const self = this;
    this.root.addEventListener('click', function (event) {
      const target = event.target.closest('[data-action]');
      if (!target) return;
      const action = target.getAttribute('data-action');
      const deviceId = target.getAttribute('data-device') || '';
      self._onAction(action, deviceId, target);
    });

    const input = U.el('setup-url');
    if (input) {
      input.addEventListener('keydown', function (event) {
        if (event.key === 'Enter') self._saveSetup();
      });
    }
  };

  HomeScreen.prototype._onAction = function (action, deviceId, target) {
    switch (action) {
      case 'toggle-theme': this.app.toggleTheme(); break;
      case 'open-settings': AS.SettingsUI.open(this.app); break;
      case 'add-pc': AS.PairingUI.open(this.app); break;
      case 'retry-rendezvous': this.app.reconnectService(); break;
      case 'save-setup': this._saveSetup(); break;
      case 'connect': this.app.connectTo(deviceId); break;
      case 'wake': this.app.wake(deviceId); break;
      case 'device-menu': this._openDeviceMenu(deviceId, target); break;
      default: break;
    }
  };

  HomeScreen.prototype._saveSetup = function () {
    const input = U.el('setup-url');
    const errorEl = U.el('setup-error');
    const value = (input.value || '').trim();
    const problem = validateServiceAddress(value);
    if (problem) {
      errorEl.textContent = problem;
      errorEl.hidden = false;
      input.focus();
      return;
    }
    errorEl.hidden = true;
    AS.Store.set('rendezvous', normalizeServiceAddress(value));
    this.app.reconnectService();
    this.render();
  };

  HomeScreen.prototype.show = function () { this.root.hidden = false; };
  HomeScreen.prototype.hide = function () { this.root.hidden = true; };

  /** Repaint the whole screen from current state. */
  HomeScreen.prototype.render = function () {
    const configured = !!AS.Store.get('rendezvous');
    const devices = this.app.deviceList();

    U.show(this.setupEl, !configured);
    U.show(this.emptyEl, configured && devices.length === 0);
    U.show(this.devicesEl, configured && devices.length > 0);

    if (!configured || devices.length === 0) {
      U.clear(this.devicesEl);
      this._cards.clear();
      if (!configured) {
        const input = U.el('setup-url');
        if (input && !input.value) input.value = AS.Store.get('rendezvous') || '';
      }
      return;
    }

    // Rebuilding the list wholesale is fine at this scale (a handful of PCs)
    // and keeps ordering and state strictly consistent with the model.
    U.clear(this.devicesEl);
    this._cards.clear();
    const self = this;
    devices.forEach(function (device) {
      const card = self._renderCard(device);
      self._cards.set(device.deviceId, card);
      self.devicesEl.appendChild(card);
    });
  };

  HomeScreen.prototype._renderCard = function (device) {
    const busy = this.busyStates.get(device.deviceId) || null;
    const online = !!device.online;
    const inUse = !!device.busy;

    const classes = ['device'];
    if (online) classes.push('is-online');
    if (inUse) classes.push('is-busy');
    if (busy && busy.kind === 'waking') classes.push('is-waking');

    const statusText = busy ? busy.label
      : !device.known ? 'Not paired with this device'
      : inUse ? 'In use by another device'
      : online ? 'Online'
      : 'Offline';

    // A slow wake path is disclosed next to the status rather than crammed into
    // the button, so the user knows what to expect before pressing it and the
    // button itself stays a short, plain verb.
    const wakeNote = !online && !busy && device.wake && device.wake.method === 'checkin' &&
      device.wake.estimatedSeconds > 60
      ? 'Checks in every ' + U.formatDuration(device.wake.estimatedSeconds).replace(/^about /, '')
      : '';

    const card = U.h('div', { class: classes.join(' '), 'data-device-card': device.deviceId }, [
      U.h('div', { class: 'device__top' }, [
        U.h('div', { class: 'device__glyph', html: AS.Icons.get('monitor') }),
        U.h('div', { class: 'device__id' }, [
          U.h('h3', { class: 'device__name', text: device.name, title: device.name }),
          U.h('div', { class: 'device__status' }, [
            U.h('span', { class: 'device__dot' }),
            U.h('span', { text: statusText })
          ]),
          wakeNote ? U.h('div', { class: 'device__note', text: wakeNote }) : null
        ]),
        U.h('button', {
          class: 'iconbtn device__menu', 'data-action': 'device-menu',
          'data-device': device.deviceId, 'aria-label': 'More options for ' + device.name,
          html: AS.Icons.get('dots')
        })
      ]),
      this._metaRow(device, online),
      busy && busy.progress ? U.h('div', { class: 'device__progress' }, [U.h('i')]) : null,
      this._actionRow(device, online, inUse, busy)
    ]);
    return card;
  };

  HomeScreen.prototype._metaRow = function (device, online) {
    const chips = [];
    if (online && device.os) chips.push(device.os);
    if (!online && device.lastSeen) chips.push('Last seen ' + U.formatAge(device.lastSeen));
    if (!online && !device.lastSeen && device.pairedAt) chips.push('Paired ' + U.formatAge(device.pairedAt));
    if (online && device.agentVersion) chips.push('v' + device.agentVersion);
    if (!chips.length) return null;
    return U.h('div', { class: 'device__meta' }, chips.map(function (text) {
      return U.h('span', { class: 'chip', text: text });
    }));
  };

  HomeScreen.prototype._actionRow = function (device, online, inUse, busy) {
    if (busy) {
      return U.h('div', { class: 'device__actions' }, [
        U.h('button', { class: 'btn', disabled: true, text: busy.label })
      ]);
    }
    if (online) {
      return U.h('div', { class: 'device__actions' }, [
        U.h('button', {
          class: 'btn btn--primary', 'data-action': 'connect', 'data-device': device.deviceId,
          text: inUse ? 'Take over' : 'Connect'
        })
      ]);
    }

    const wake = device.wake || {};
    const canWake = wake.method && wake.method !== 'none';
    if (!canWake) {
      return U.h('div', { class: 'device__actions' }, [
        U.h('button', { class: 'btn', disabled: true, text: 'Offline', title: wake.reason || '' })
      ]);
    }
    return U.h('div', { class: 'device__actions' }, [
      U.h('button', {
        class: 'btn btn--primary', 'data-action': 'wake', 'data-device': device.deviceId,
        text: 'Wake PC',
        title: wake.method === 'checkin'
          ? 'This PC wakes on a schedule to check whether it is wanted.'
          : 'A magic packet will be sent by another PC on the same network.'
      })
    ]);
  };

  HomeScreen.prototype._openDeviceMenu = function (deviceId) {
    const device = this.app.deviceById(deviceId);
    if (!device) return;
    const self = this;

    const rows = U.h('div', { class: 'rowlist' }, [
      U.h('div', { class: 'rowlist__item' }, [
        U.h('div', { class: 'rowlist__main' }, [
          U.h('div', { class: 'rowlist__title', text: 'Identity' }),
          U.h('div', { class: 'rowlist__sub mono', text: device.fingerprint || '—' })
        ])
      ]),
      U.h('div', { class: 'rowlist__item' }, [
        U.h('div', { class: 'rowlist__main' }, [
          U.h('div', { class: 'rowlist__title', text: 'Status' }),
          U.h('div', { class: 'rowlist__sub', text: device.online ? 'Online' : 'Offline · last seen ' + U.formatAge(device.lastSeen) })
        ])
      ]),
      U.h('div', { class: 'rowlist__item' }, [
        U.h('div', { class: 'rowlist__main' }, [
          U.h('div', { class: 'rowlist__title', text: 'Waking' }),
          U.h('div', { class: 'rowlist__sub', text: describeWake(device.wake) })
        ])
      ])
    ]);

    UI.modal({
      title: device.name,
      body: [
        rows,
        U.h('p', { class: 'modal__note faint', style: { marginTop: '14px' },
          text: 'The identity above is what ALL SHARE checks on every connection. It never changes unless ALL SHARE is reinstalled on that PC.' })
      ],
      actions: [
        { label: 'Close' },
        {
          label: 'Remove this PC', variant: 'danger',
          onClick: function () {
            setTimeout(function () { self._confirmRemove(device); }, 60);
          }
        }
      ]
    });
  };

  HomeScreen.prototype._confirmRemove = async function (device) {
    const confirmed = await UI.confirm({
      title: 'Remove ' + device.name + '?',
      body: 'This device will no longer be able to connect to it. You can pair again at any time with a new code from the PC.',
      confirmLabel: 'Remove',
      danger: true
    });
    if (confirmed) this.app.forgetDevice(device.deviceId);
  };

  /** Mark a device as doing something, or clear it with null. */
  HomeScreen.prototype.setBusy = function (deviceId, state) {
    if (state) this.busyStates.set(deviceId, state);
    else this.busyStates.delete(deviceId);
    this.render();
  };

  /** Update the service-connection banner and footer. */
  HomeScreen.prototype.setServiceState = function (state, detail) {
    const messages = {
      idle: '',
      connecting: 'Connecting to the ALL SHARE service…',
      connected: '',
      retrying: 'Not connected to the ALL SHARE service. Your PCs will appear when it comes back.'
    };
    const message = messages[state] || '';
    U.show(this.bannerEl, !!message && state === 'retrying');
    if (this.bannerText) this.bannerText.textContent = detail && detail.message ? detail.message : message;

    if (this.footerStatus) {
      const label = state === 'connected'
        ? 'Connected to the ALL SHARE service'
        : state === 'connecting' ? 'Connecting…'
        : state === 'retrying' ? 'Reconnecting…' : 'Not connected';
      this.footerStatus.textContent = label +
        (AS.Identity.fingerprint ? ' · this device: ' + AS.Identity.fingerprint : '');
    }
  };

  function describeWake(wake) {
    if (!wake || !wake.method || wake.method === 'none') {
      return wake && wake.reason ? wake.reason : 'This PC cannot be woken remotely.';
    }
    if (wake.method === 'modernStandby') return 'Stays reachable while asleep — waking is instant.';
    if (wake.method === 'lanPeer') return 'Another PC on the same network can wake it, usually within seconds.';
    if (wake.method === 'checkin') {
      return 'Wakes itself every ' + U.formatDuration(wake.estimatedSeconds) + ' to check whether it is wanted.';
    }
    if (wake.method === 'wolDirect') return 'The ALL SHARE service can wake it directly.';
    return 'Can be woken remotely.';
  }

  function validateServiceAddress(value) {
    if (!value) return 'Enter the address your PC showed you.';
    const normalized = normalizeServiceAddress(value);
    let url;
    try {
      url = new URL(normalized);
    } catch (err) {
      return 'That does not look like an address. It should look like wss://allshare.example.com/rv';
    }
    if (url.protocol !== 'wss:' && url.protocol !== 'ws:') {
      return 'The address should start with wss://';
    }
    if (!url.hostname) return 'That address is missing a server name.';
    return '';
  }

  /**
   * Accept what a user is likely to type and turn it into a usable address.
   * A bare hostname, an https:// URL and a missing path all become the right
   * WebSocket URL rather than an error.
   */
  function normalizeServiceAddress(value) {
    let text = String(value).trim();
    if (!/^[a-z]+:\/\//i.test(text)) text = 'wss://' + text;
    text = text.replace(/^https:\/\//i, 'wss://').replace(/^http:\/\//i, 'ws://');
    try {
      const url = new URL(text);
      if (url.pathname === '/' || url.pathname === '') url.pathname = '/rv';
      return url.toString().replace(/\/$/, '');
    } catch (err) {
      return text;
    }
  }

  HomeScreen.validateServiceAddress = validateServiceAddress;
  HomeScreen.normalizeServiceAddress = normalizeServiceAddress;
  HomeScreen.describeWake = describeWake;
  AS.HomeScreen = HomeScreen;
})(window.AllShare);
