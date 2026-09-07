/*
 * ALL SHARE — settings.
 *
 * Organised by what the user is trying to achieve, not by which subsystem owns
 * the value. Anything that could make the product behave surprisingly lives
 * under Advanced, and anything genuinely dangerous is not exposed at all.
 */
(function (AS) {
  'use strict';

  const U = AS.Util;
  const UI = AS.UI;
  const Store = AS.Store;

  const SettingsUI = {};

  SettingsUI.open = function (app, initialTab) {
    const tabs = [
      { id: 'general', label: 'General', build: buildGeneral },
      { id: 'streaming', label: 'Streaming', build: buildStreaming },
      { id: 'input', label: 'Input', build: buildInput },
      { id: 'sound', label: 'Sound & clipboard', build: buildSoundClipboard },
      { id: 'advanced', label: 'Advanced', build: buildAdvanced }
    ];
    // The developer tab is hidden until someone deliberately reveals it (tap the
    // version line seven times, in General). Everything on it is diagnostic
    // detail that would only confuse the people this product is built for, but
    // that is genuinely needed when something is going wrong.
    if (Store.get('developerMode')) {
      tabs.push({ id: 'developer', label: 'Developer', build: buildDeveloper });
    }

    const tabBar = U.h('div', { class: 'modal__tabs', role: 'tablist' });
    const panels = U.h('div');
    const buttons = [];

    tabs.forEach(function (tab, index) {
      const panel = U.h('div', { class: 'modal__panel', role: 'tabpanel', hidden: index !== 0 });
      panel.appendChild(tab.build(app));
      panels.appendChild(panel);

      const button = U.h('button', {
        class: 'modal__tab', role: 'tab', text: tab.label,
        'aria-selected': index === 0 ? 'true' : 'false'
      });
      button.addEventListener('click', function () {
        buttons.forEach(function (b, i) {
          b.setAttribute('aria-selected', b === button ? 'true' : 'false');
          panels.children[i].hidden = b !== button;
        });
      });
      buttons.push(button);
      tabBar.appendChild(button);
    });

    if (initialTab) {
      const index = tabs.findIndex(function (t) { return t.id === initialTab; });
      if (index > 0) buttons[index].click();
    }

    UI.modal({
      title: 'Settings',
      wide: true,
      body: [tabBar, panels],
      actions: [{ label: 'Done', variant: 'primary' }]
    });
  };

  function section(title) {
    return U.h('div', {
      class: 'popover__title',
      text: title,
      style: { marginTop: '10px', marginBottom: '2px' }
    });
  }

  function buildGeneral(app) {
    const wrap = U.h('div');

    const labelInput = U.h('input', {
      class: 'field__input', type: 'text',
      value: Store.get('deviceLabel') || U.guessDeviceLabel(),
      placeholder: U.guessDeviceLabel(), maxlength: '48'
    });
    labelInput.addEventListener('change', function () {
      Store.set('deviceLabel', labelInput.value.trim());
    });
    wrap.appendChild(U.h('label', { class: 'field' }, [
      U.h('span', { class: 'field__label', text: 'Name for this device' }),
      labelInput,
      U.h('p', { class: 'field__hint', text: 'Shown on your PC so you can tell your devices apart.' })
    ]));

    const urlInput = U.h('input', {
      class: 'field__input', type: 'text', spellcheck: 'false',
      value: Store.get('rendezvous'), placeholder: 'wss://allshare.example.com/rv'
    });
    const urlError = U.h('p', { class: 'field__error', hidden: true });
    urlInput.addEventListener('change', function () {
      const problem = AS.HomeScreen.validateServiceAddress(urlInput.value);
      if (problem) {
        urlError.textContent = problem;
        urlError.hidden = false;
        return;
      }
      urlError.hidden = true;
      const normalized = AS.HomeScreen.normalizeServiceAddress(urlInput.value);
      urlInput.value = normalized;
      Store.set('rendezvous', normalized);
      app.reconnectService();
    });
    wrap.appendChild(U.h('label', { class: 'field' }, [
      U.h('span', { class: 'field__label', text: 'ALL SHARE service address' }),
      urlInput, urlError,
      U.h('p', { class: 'field__hint', text: 'Your PC shows this on its Add a device screen.' })
    ]));

    wrap.appendChild(UI.selectRow({
      title: 'Appearance',
      description: 'Match your Chromebook, or pick one.',
      value: Store.get('theme'),
      options: [
        { value: 'system', label: 'Match system' },
        { value: 'dark', label: 'Dark' },
        { value: 'light', label: 'Light' }
      ],
      onChange: function (value) { Store.set('theme', value); app.applyTheme(); }
    }));

    wrap.appendChild(section('Paired PCs'));
    const list = U.h('div', { class: 'rowlist' });
    const devices = Store.devices();
    if (!devices.length) {
      list.appendChild(U.h('p', { class: 'modal__note', text: 'No PCs paired yet.' }));
    } else {
      devices.forEach(function (device) {
        list.appendChild(U.h('div', { class: 'rowlist__item' }, [
          U.h('div', { class: 'rowlist__main' }, [
            U.h('div', { class: 'rowlist__title', text: device.name }),
            U.h('div', { class: 'rowlist__sub', text: 'Paired ' + U.formatAge(device.pairedAt) })
          ]),
          U.h('button', {
            class: 'btn btn--tiny', text: 'Remove',
            onclick: async function () {
              const yes = await UI.confirm({
                title: 'Remove ' + device.name + '?',
                body: 'You can pair again at any time with a new code from the PC.',
                confirmLabel: 'Remove', danger: true
              });
              if (yes) { app.forgetDevice(device.deviceId); SettingsUI.open(app, 'general'); }
            }
          })
        ]));
      });
    }
    wrap.appendChild(list);

    wrap.appendChild(buildAboutRow(app));
    return wrap;
  }

  /**
   * The about line, and the way in to the developer tab.
   *
   * Seven taps is the long-established convention for this (Android's build
   * number, and most things that copied it). It is deliberately something
   * nobody does by accident and everybody can be talked through over the phone.
   */
  function buildAboutRow(app) {
    let taps = 0;
    let lastTap = 0;
    // One toast, replaced as the count changes. Stacking a new one per tap
    // buries whatever else the app was trying to say.
    let countdownToast = null;
    const dismissCountdown = function () {
      if (countdownToast) { countdownToast(); countdownToast = null; }
    };

    const line = U.h('button', {
      class: 'about',
      type: 'button',
      title: 'ALL SHARE',
      text: 'ALL SHARE ' + AS.VERSION + ' — Powered by MMC'
    });

    line.addEventListener('click', function () {
      const now = Date.now();
      // Taps must be in one run. A tap minutes after the last one starts over,
      // so a curious click today plus another next week never adds up.
      if (now - lastTap > 3000) taps = 0;
      lastTap = now;
      taps++;

      if (Store.get('developerMode')) {
        if (taps >= 7) {
          taps = 0;
          dismissCountdown();
          Store.set('developerMode', false);
          UI.toast({ kind: 'ok', title: 'Developer tools hidden' });
          SettingsUI.open(app, 'general');
        }
        return;
      }
      if (taps >= 7) {
        taps = 0;
        dismissCountdown();
        Store.set('developerMode', true);
        UI.toast({
          kind: 'ok',
          title: 'Developer tools are on',
          text: 'A Developer tab has been added to Settings. Tap the version line seven times again to hide it.'
        });
        SettingsUI.open(app, 'developer');
      } else if (taps >= 4) {
        const remaining = 7 - taps;
        dismissCountdown();
        countdownToast = UI.toast({
          title: remaining === 1
            ? 'One more to turn on developer tools'
            : remaining + ' more to turn on developer tools',
          timeout: 3000
        });
      }
    });

    return U.h('div', { class: 'about__wrap' }, [line]);
  }

  function buildStreaming(app) {
    const wrap = U.h('div');

    wrap.appendChild(UI.settingRow(
      'Mode',
      'Gaming keeps latency lowest. Desktop keeps text sharpest.',
      UI.segmented({
        label: 'Streaming mode',
        value: Store.get('preset'),
        options: [
          { value: 'gaming', label: 'Gaming', title: 'High frame rate, minimum buffering' },
          { value: 'balanced', label: 'Balanced' },
          { value: 'desktop', label: 'Desktop', title: 'Sharper text, fewer frames when nothing moves' }
        ],
        onChange: function (value) { Store.set('preset', value); app.pushQuality(); }
      })
    ));

    wrap.appendChild(UI.selectRow({
      title: 'Picture size',
      description: 'Original is sharpest because nothing is resized.',
      value: Store.get('resolution'),
      options: [
        { value: 'auto', label: 'Automatic' },
        { value: 'native', label: 'Original (sharpest)' },
        { value: 'fit', label: 'Fit my screen' },
        { value: '1080', label: '1080p' },
        { value: '720', label: '720p' }
      ],
      onChange: function (value) { Store.set('resolution', value); app.pushQuality(); }
    }));

    wrap.appendChild(UI.selectRow({
      title: 'Frame rate limit',
      description: 'Higher is smoother; lower saves data.',
      value: String(Store.get('maxFps')),
      options: [
        { value: '30', label: '30 fps' },
        { value: '60', label: '60 fps' },
        { value: '90', label: '90 fps' },
        { value: '120', label: '120 fps' }
      ],
      onChange: function (value) { Store.set('maxFps', parseInt(value, 10)); app.pushQuality(); }
    }));

    wrap.appendChild(UI.selectRow({
      title: 'Data limit',
      description: 'The most ALL SHARE will use when your connection allows it.',
      value: String(Store.get('maxKbps')),
      options: [
        { value: '5000', label: '5 Mbps' },
        { value: '10000', label: '10 Mbps' },
        { value: '25000', label: '25 Mbps' },
        { value: '50000', label: '50 Mbps' },
        { value: '100000', label: '100 Mbps' }
      ],
      onChange: function (value) { Store.set('maxKbps', parseInt(value, 10)); app.pushQuality(); }
    }));

    wrap.appendChild(UI.toggleRow({
      title: 'Adjust automatically',
      description: 'Lower the quality when your connection struggles, and raise it again when it recovers.',
      value: Store.get('adaptive'),
      onChange: function (value) { Store.set('adaptive', value); app.pushQuality(); }
    }));

    wrap.appendChild(UI.selectRow({
      title: 'Responsiveness',
      description: 'Ultra is best on a fast, stable connection. Low is the safe choice.',
      value: Store.get('latencyMode'),
      options: [
        { value: 'ultra', label: 'Ultra low' },
        { value: 'low', label: 'Low (recommended)' },
        { value: 'balanced', label: 'Balanced' },
        { value: 'smooth', label: 'Smoothest' }
      ],
      onChange: function (value) { Store.set('latencyMode', value); app.applyLatencyMode(); }
    }));

    return wrap;
  }

  function buildInput() {
    const wrap = U.h('div');

    wrap.appendChild(UI.toggleRow({
      title: 'Draw the mouse pointer locally',
      description: 'Makes the pointer feel instant instead of lagging behind your hand.',
      value: Store.get('localCursor'),
      onChange: function (value) { Store.set('localCursor', value); }
    }));

    wrap.appendChild(UI.toggleRow({
      title: 'Send every key',
      description: 'Includes Alt+Tab and the Windows key. Needs fullscreen.',
      value: Store.get('captureAllKeys'),
      onChange: function (value) { Store.set('captureAllKeys', value); }
    }));

    if (AS.Keymap.isChromeOS()) {
      wrap.appendChild(UI.toggleRow({
        title: 'Top row sends F1–F12',
        description: 'Send the Chromebook top row as function keys, which is what Windows apps expect.',
        value: Store.get('chromebookTopRow') !== false,
        onChange: function (value) { Store.set('chromebookTopRow', value); }
      }));
    }

    wrap.appendChild(UI.selectRow({
      title: 'Keyboard behaviour',
      description: 'Physical keys work in games. Matching characters is better when your layouts differ.',
      value: Store.get('layoutMode'),
      options: [
        { value: 'scancode', label: 'Physical keys (best for games)' },
        { value: 'unicode', label: 'Match the characters I type' }
      ],
      onChange: function (value) { Store.set('layoutMode', value); }
    }));

    wrap.appendChild(UI.selectRow({
      title: 'Mouse sensitivity',
      description: 'Only applies while the mouse is locked.',
      value: String(Store.get('mouseSensitivity')),
      options: [
        { value: '0.5', label: '0.5×' }, { value: '0.75', label: '0.75×' },
        { value: '1', label: '1× (normal)' }, { value: '1.5', label: '1.5×' },
        { value: '2', label: '2×' }
      ],
      onChange: function (value) { Store.set('mouseSensitivity', parseFloat(value)); }
    }));

    wrap.appendChild(UI.toggleRow({
      title: 'Reverse scrolling',
      description: 'Scroll the other way.',
      value: Store.get('invertScroll'),
      onChange: function (value) { Store.set('invertScroll', value); }
    }));

    wrap.appendChild(U.h('p', {
      class: 'modal__note faint', style: { marginTop: '14px' },
      html: 'To release a locked mouse, hold <span class="kbd">Esc</span>. ' +
            '<span class="kbd">Ctrl</span>+<span class="kbd">Alt</span>+<span class="kbd">Shift</span>+<span class="kbd">Q</span> ' +
            'releases everything at once.'
    }));
    return wrap;
  }

  function buildSoundClipboard(app) {
    const wrap = U.h('div');

    wrap.appendChild(UI.toggleRow({
      title: 'Play sound from the PC',
      description: 'Streams your PC’s audio alongside the picture.',
      value: Store.get('audioEnabled'),
      onChange: function (value) { Store.set('audioEnabled', value); app.pushQuality(); }
    }));

    wrap.appendChild(section('Clipboard'));
    wrap.appendChild(U.h('p', {
      class: 'modal__note',
      text: 'Clipboard sharing only moves text, and only when you copy or paste yourself. Nothing is sent in the background.'
    }));

    wrap.appendChild(UI.toggleRow({
      title: 'Send what I copy here to the PC',
      description: 'When you press Ctrl+V, the text you copied on this device is pasted on the PC.',
      value: Store.get('clipboardToRemote'),
      onChange: function (value) { Store.set('clipboardToRemote', value); }
    }));

    wrap.appendChild(UI.toggleRow({
      title: 'Bring what I copy on the PC back here',
      description: 'Text you copy on the PC becomes available on this device.',
      value: Store.get('clipboardFromRemote'),
      onChange: function (value) { Store.set('clipboardFromRemote', value); }
    }));

    return wrap;
  }

  function buildAdvanced(app) {
    const wrap = U.h('div');

    wrap.appendChild(UI.selectRow({
      title: 'Preferred codec',
      description: 'Automatic picks the best your PC can send and this device can decode.',
      value: Store.get('codecPreference'),
      options: [
        { value: 'auto', label: 'Automatic (recommended)' },
        { value: 'h264', label: 'H.264' },
        { value: 'h265', label: 'HEVC / H.265' },
        { value: 'av1', label: 'AV1' },
        { value: 'vp9', label: 'VP9' }
      ],
      onChange: function (value) { Store.set('codecPreference', value); }
    }));

    wrap.appendChild(UI.selectRow({
      title: 'Connection route',
      description: 'Direct is fastest. Relay only is a diagnostic for restrictive networks.',
      value: Store.get('relayPreference'),
      options: [
        { value: 'auto', label: 'Automatic (recommended)' },
        { value: 'relay', label: 'Always use a relay' }
      ],
      onChange: function (value) { Store.set('relayPreference', value); }
    }));

    wrap.appendChild(UI.toggleRow({
      title: 'Show performance details',
      description: 'Display the live performance panel during a session.',
      value: Store.get('showHud'),
      onChange: function (value) { Store.set('showHud', value); }
    }));

    wrap.appendChild(UI.toggleRow({
      title: 'Detailed logging',
      description: 'Record extra detail for troubleshooting. Never includes what you type or copy.',
      value: Store.get('debugLogging'),
      onChange: function (value) { Store.set('debugLogging', value); }
    }));

    wrap.appendChild(section('Diagnostics'));
    const info = U.h('div', { class: 'rowlist' }, [
      infoRow('This device', AS.Identity.fingerprint || '—'),
      infoRow('Service', AS.Store.get('rendezvous') || 'not set'),
      infoRow('Service status', app.rv.isConnected() ? 'Connected (' + (app.rv.serverVersion || 'unknown version') + ')' : app.rv.state),
      infoRow('Browser', navigator.userAgent),
      infoRow('Video decoders', describeDecoders())
    ]);
    wrap.appendChild(info);

    wrap.appendChild(U.h('div', { style: { display: 'flex', gap: '10px', marginTop: '14px' } }, [
      U.h('button', {
        class: 'btn btn--ghost', text: 'Copy diagnostics',
        onclick: async function () {
          const text = buildDiagnostics(app);
          try {
            await navigator.clipboard.writeText(text);
            UI.toast({ kind: 'ok', title: 'Diagnostics copied' });
          } catch (err) {
            UI.modal({
              title: 'Diagnostics',
              wide: true,
              body: U.h('pre', {
                text: text,
                style: { maxHeight: '50vh', overflow: 'auto', fontSize: '11px', whiteSpace: 'pre-wrap' }
              }),
              actions: [{ label: 'Close', variant: 'primary' }]
            });
          }
        }
      }),
      U.h('button', {
        class: 'btn btn--danger', text: 'Reset settings',
        onclick: async function () {
          const yes = await UI.confirm({
            title: 'Reset all settings?',
            body: 'Your paired PCs are kept. Everything else goes back to its default.',
            confirmLabel: 'Reset', danger: true
          });
          if (yes) { Store.reset(); app.applyTheme(); SettingsUI.open(app, 'advanced'); }
        }
      })
    ]));

    return wrap;
  }

  function infoRow(title, value) {
    return U.h('div', { class: 'rowlist__item' }, [
      U.h('div', { class: 'rowlist__main' }, [
        U.h('div', { class: 'rowlist__title', text: title }),
        U.h('div', { class: 'rowlist__sub mono', text: value })
      ])
    ]);
  }

  function describeDecoders() {
    try {
      const caps = RTCRtpReceiver.getCapabilities('video');
      if (!caps || !caps.codecs) return 'unknown';
      const names = [];
      for (const codec of caps.codecs) {
        const name = (codec.mimeType || '').replace(/^video\//, '');
        if (/^(rtx|red|ulpfec|flexfec-03)$/i.test(name)) continue;
        if (names.indexOf(name) < 0) names.push(name);
      }
      return names.join(', ') || 'none';
    } catch (err) {
      return 'unknown';
    }
  }

  /**
   * The developer tab: a live log, the raw WebRTC report, and the internals.
   *
   * Nothing here is required to use ALL SHARE. It exists so that when something
   * does go wrong, the answer is available without asking anyone to open a
   * browser console — which on a Chromebook is not always something a user can
   * be talked through.
   */
  function buildDeveloper(app) {
    const wrap = U.h('div');

    wrap.appendChild(U.h('p', {
      class: 'modal__note',
      text: 'These tools are for troubleshooting. Nothing here needs changing for normal use.'
    }));

    // --- Live log -----------------------------------------------------------
    wrap.appendChild(section('Log'));
    const logBox = U.h('pre', { class: 'devlog' });
    const renderLog = function () { logBox.textContent = AS.Log.dump() || '(nothing logged yet)'; };
    renderLog();

    // The log refreshes on a timer while this panel is on screen. The interval
    // is cleared when the modal closes, so a settings screen opened and shut
    // fifty times does not leave fifty timers running.
    const logTimer = setInterval(function () {
      if (!logBox.isConnected) { clearInterval(logTimer); return; }
      const atBottom = logBox.scrollTop + logBox.clientHeight >= logBox.scrollHeight - 8;
      renderLog();
      if (atBottom) logBox.scrollTop = logBox.scrollHeight;
    }, 1000);

    wrap.appendChild(logBox);
    wrap.appendChild(U.h('div', { class: 'devrow' }, [
      U.h('button', {
        class: 'btn btn--tiny', text: 'Copy log',
        onclick: function () { copyOrShow(AS.Log.dump(), 'Log'); }
      }),
      U.h('button', {
        class: 'btn btn--tiny', text: 'Clear',
        onclick: function () { AS.Log.clear(); renderLog(); }
      })
    ]));

    // --- Raw WebRTC report --------------------------------------------------
    wrap.appendChild(section('Connection report'));
    const statsBox = U.h('pre', { class: 'devlog' , text: 'No session is running.' });
    const refreshStats = async function () {
      if (!app.session) {
        statsBox.textContent = 'No session is running.';
        return;
      }
      const raw = await app.session.rawStats();
      statsBox.textContent = raw.length
        ? JSON.stringify(raw, null, 2)
        : 'The session reported no statistics.';
    };
    refreshStats();
    wrap.appendChild(statsBox);
    wrap.appendChild(U.h('div', { class: 'devrow' }, [
      U.h('button', { class: 'btn btn--tiny', text: 'Refresh', onclick: refreshStats }),
      U.h('button', {
        class: 'btn btn--tiny', text: 'Copy report',
        onclick: function () { copyOrShow(statsBox.textContent, 'Connection report'); }
      }),
      U.h('button', {
        class: 'btn btn--tiny', text: 'Request a keyframe',
        onclick: function () {
          if (!app.session) { UI.toast({ kind: 'warn', title: 'No session is running' }); return; }
          app.session.requestKeyframe();
          UI.toast({ title: 'Keyframe requested' });
        }
      })
    ]));

    // --- Internals ----------------------------------------------------------
    wrap.appendChild(section('Internals'));
    wrap.appendChild(U.h('div', { class: 'rowlist' }, [
      infoRow('Client version', AS.VERSION),
      infoRow('Signalling version', String(AS.Rendezvous.SIGNAL_VERSION)),
      infoRow('Wire protocol', AS.Protocol.VERSION_MAJOR + '.' + AS.Protocol.VERSION_MINOR),
      infoRow('Device public key', AS.Identity.deviceId || '—'),
      infoRow('Session state', app.session ? app.session.state : 'no session'),
      infoRow('Secure context', String(self.isSecureContext)),
      infoRow('Page origin', location.protocol === 'file:' ? 'file:// (local file)' : location.origin),
      infoRow('Pointer Lock', 'requestPointerLock' in Element.prototype ? 'available' : 'missing'),
      infoRow('Keyboard Lock', navigator.keyboard && navigator.keyboard.lock ? 'available' : 'missing'),
      infoRow('Video decoders', describeDecoders())
    ]));

    wrap.appendChild(U.h('div', { class: 'devrow' }, [
      U.h('button', {
        class: 'btn btn--tiny', text: 'Hide developer tools',
        onclick: function () {
          Store.set('developerMode', false);
          UI.toast({ kind: 'ok', title: 'Developer tools hidden' });
          SettingsUI.open(app, 'general');
        }
      })
    ]));

    return wrap;
  }

  /**
   * Put text on the clipboard, falling back to showing it.
   *
   * The clipboard write can be refused — a Chromebook in a restricted profile
   * does refuse it — and silently doing nothing is the worst outcome for a
   * button whose entire purpose is handing information to somebody else.
   */
  async function copyOrShow(text, title) {
    try {
      await navigator.clipboard.writeText(text);
      UI.toast({ kind: 'ok', title: title + ' copied' });
    } catch (err) {
      UI.modal({
        title: title,
        wide: true,
        body: U.h('pre', {
          text: text,
          style: { maxHeight: '50vh', overflow: 'auto', fontSize: '11px', whiteSpace: 'pre-wrap' }
        }),
        actions: [{ label: 'Close' }]
      });
    }
  }

  function buildDiagnostics(app) {
    const lines = [
      'ALL SHARE diagnostics',
      'Generated: ' + new Date().toISOString(),
      'Device: ' + (AS.Identity.fingerprint || 'unknown'),
      'Service: ' + (Store.get('rendezvous') || 'not set'),
      'Service state: ' + app.rv.state,
      'Server version: ' + (app.rv.serverVersion || 'unknown'),
      'Browser: ' + navigator.userAgent,
      'Secure context: ' + self.isSecureContext,
      'Video decoders: ' + describeDecoders(),
      'Paired PCs: ' + Store.devices().length,
      '',
      'Settings:',
      JSON.stringify(redactSettings(Store.all()), null, 2),
      '',
      'Recent log:',
      AS.Log.dump()
    ];
    return lines.join('\n');
  }

  /** Strip anything from the settings dump that identifies the user's network. */
  function redactSettings(settings) {
    const copy = Object.assign({}, settings);
    if (copy.rendezvous) {
      try {
        copy.rendezvous = new URL(copy.rendezvous).protocol + '//<hidden>';
      } catch (err) {
        copy.rendezvous = '<hidden>';
      }
    }
    delete copy.deviceLabel;
    return copy;
  }

  AS.SettingsUI = SettingsUI;
})(window.AllShare);
