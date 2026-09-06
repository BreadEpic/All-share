/*
 * ALL SHARE — reusable interface pieces: toasts, modals, confirmations.
 *
 * Every message shown to a user is written in plain language. Anything
 * technical — an ICE state, a protocol code — belongs behind "Advanced
 * details", never in the sentence the user reads first.
 */
(function (AS) {
  'use strict';

  const U = AS.Util;
  const UI = {};

  // ------------------------------------------------------------ toasts

  const TOAST_MS = 5000;

  /**
   * Show a transient message.
   * @param {{kind?:string, title:string, text?:string, actionLabel?:string,
   *          onAction?:Function, timeout?:number}} options
   */
  UI.toast = function (options) {
    const host = U.el('toasts');
    if (!host) return function () {};

    const kind = options.kind || 'info';
    const iconName = kind === 'ok' ? 'check' : kind === 'error' ? 'alert' : kind === 'warn' ? 'alert' : 'info';

    const body = U.h('div', { class: 'toast__body' }, [
      U.h('div', { class: 'toast__title', text: options.title })
    ]);
    if (options.text) body.appendChild(U.h('div', { class: 'toast__text', text: options.text }));

    let dismiss;
    if (options.actionLabel && options.onAction) {
      body.appendChild(U.h('button', {
        class: 'toast__action', text: options.actionLabel,
        onclick: function () { options.onAction(); dismiss(); }
      }));
    }

    const node = U.h('div', { class: 'toast toast--' + kind, role: 'status' }, [
      U.h('span', { class: 'toast__icon', html: AS.Icons.get(iconName) }),
      body,
      U.h('button', { class: 'toast__close', 'aria-label': 'Dismiss', text: '×', onclick: function () { dismiss(); } })
    ]);

    host.appendChild(node);
    let removed = false;
    dismiss = function () {
      if (removed) return;
      removed = true;
      clearTimeout(timer);
      node.classList.add('is-out');
      setTimeout(function () { if (node.parentNode) node.parentNode.removeChild(node); }, 220);
    };
    const timeout = options.timeout === undefined ? TOAST_MS : options.timeout;
    const timer = timeout > 0 ? setTimeout(dismiss, timeout) : 0;
    return dismiss;
  };

  // ------------------------------------------------------------ modals

  let openModal = null;

  /**
   * Show a modal dialog.
   *
   * Focus is moved into the dialog and Escape closes it, so a keyboard user is
   * never trapped. Only one modal exists at a time by construction.
   */
  UI.modal = function (options) {
    UI.closeModal();
    const root = U.el('modal-root');
    if (!root) return null;
    U.clear(root);

    const dialog = U.h('div', {
      class: 'modal' + (options.wide ? ' modal--wide' : ''),
      role: 'dialog', 'aria-modal': 'true', 'aria-label': options.title || 'Dialog'
    });

    if (options.title) {
      dialog.appendChild(U.h('div', { class: 'modal__head' }, [
        U.h('h2', { class: 'modal__title', text: options.title }),
        options.dismissable === false ? null :
          U.h('button', { class: 'iconbtn', 'aria-label': 'Close', html: AS.Icons.get('x'), onclick: close })
      ]));
    }

    const bodyNode = U.h('div', { class: 'modal__body' });
    if (options.body) {
      (Array.isArray(options.body) ? options.body : [options.body]).forEach(function (child) {
        bodyNode.appendChild(typeof child === 'string' ? U.h('p', { class: 'modal__note', text: child }) : child);
      });
    }
    dialog.appendChild(bodyNode);

    if (options.actions && options.actions.length) {
      const foot = U.h('div', { class: 'modal__foot' });
      options.actions.forEach(function (action) {
        foot.appendChild(U.h('button', {
          class: 'btn ' + (action.variant ? 'btn--' + action.variant : 'btn--ghost'),
          text: action.label,
          disabled: action.disabled || false,
          onclick: function () {
            const keepOpen = action.onClick && action.onClick() === false;
            if (!keepOpen && action.keepOpen !== true) close();
          }
        }));
      });
      dialog.appendChild(foot);
    }

    root.appendChild(dialog);
    root.hidden = false;

    const listeners = new U.Listeners();
    if (options.dismissable !== false) {
      listeners.add(root, 'mousedown', function (event) { if (event.target === root) close(); });
      listeners.add(document, 'keydown', function (event) {
        if (event.key === 'Escape') { event.stopPropagation(); close(); }
      }, true);
    }

    function close() {
      if (openModal !== handle) return;
      listeners.removeAll();
      root.hidden = true;
      U.clear(root);
      openModal = null;
      if (options.onClose) options.onClose();
    }

    const handle = { close: close, dialog: dialog, body: bodyNode };
    openModal = handle;

    // Focus the first control so the dialog is immediately usable by keyboard.
    const focusable = dialog.querySelector('input, button, select, textarea');
    if (focusable) setTimeout(function () { focusable.focus(); }, 40);
    return handle;
  };

  UI.closeModal = function () { if (openModal) openModal.close(); };
  UI.hasModal = function () { return !!openModal; };

  /** A yes/no dialog. Resolves true when confirmed. */
  UI.confirm = function (options) {
    return new Promise(function (resolve) {
      let answered = false;
      UI.modal({
        title: options.title,
        body: options.body,
        actions: [
          { label: options.cancelLabel || 'Cancel', onClick: function () { answered = true; resolve(false); } },
          {
            label: options.confirmLabel || 'Confirm',
            variant: options.danger ? 'danger' : 'primary',
            onClick: function () { answered = true; resolve(true); }
          }
        ],
        onClose: function () { if (!answered) resolve(false); }
      });
    });
  };

  // ------------------------------------------------------- form pieces

  UI.toggleRow = function (options) {
    const button = U.h('button', {
      class: 'toggle', type: 'button', role: 'switch',
      'aria-checked': options.value ? 'true' : 'false',
      'aria-label': options.title
    });
    button.addEventListener('click', function () {
      const next = button.getAttribute('aria-checked') !== 'true';
      button.setAttribute('aria-checked', next ? 'true' : 'false');
      options.onChange(next);
    });
    const row = U.h('div', { class: 'switch' }, [
      U.h('div', { class: 'switch__text' }, [
        U.h('div', { class: 'switch__title', text: options.title }),
        options.description ? U.h('div', { class: 'switch__desc', text: options.description }) : null
      ]),
      U.h('div', { class: 'switch__control' }, [button])
    ]);
    row.setValue = function (value) { button.setAttribute('aria-checked', value ? 'true' : 'false'); };
    return row;
  };

  UI.segmented = function (options) {
    const group = U.h('div', { class: 'seg', role: 'group', 'aria-label': options.label || '' });
    const buttons = [];
    options.options.forEach(function (option) {
      const button = U.h('button', {
        type: 'button', text: option.label,
        'aria-pressed': option.value === options.value ? 'true' : 'false',
        title: option.title || ''
      });
      button.addEventListener('click', function () {
        buttons.forEach(function (b) { b.setAttribute('aria-pressed', 'false'); });
        button.setAttribute('aria-pressed', 'true');
        options.onChange(option.value);
      });
      buttons.push(button);
      group.appendChild(button);
    });
    return group;
  };

  UI.selectRow = function (options) {
    const select = U.h('select', { class: 'select', 'aria-label': options.title });
    options.options.forEach(function (option) {
      select.appendChild(U.h('option', {
        value: option.value, text: option.label,
        selected: String(option.value) === String(options.value)
      }));
    });
    select.addEventListener('change', function () { options.onChange(select.value); });
    return U.h('div', { class: 'switch' }, [
      U.h('div', { class: 'switch__text' }, [
        U.h('div', { class: 'switch__title', text: options.title }),
        options.description ? U.h('div', { class: 'switch__desc', text: options.description }) : null
      ]),
      U.h('div', { class: 'switch__control' }, [select])
    ]);
  };

  UI.settingRow = function (title, description, control) {
    return U.h('div', { class: 'switch' }, [
      U.h('div', { class: 'switch__text' }, [
        U.h('div', { class: 'switch__title', text: title }),
        description ? U.h('div', { class: 'switch__desc', text: description }) : null
      ]),
      U.h('div', { class: 'switch__control' }, [control])
    ]);
  };

  // ------------------------------------------------------------ errors

  /**
   * Turn any failure into a sentence a non-technical user can act on.
   *
   * The mapping is intentionally exhaustive rather than falling through to a
   * generic message: "Connection failed" tells the user nothing about whether
   * to check their Wi-Fi, wake their PC, or pair again.
   */
  const FRIENDLY = {
    device_offline: {
      title: 'That PC is offline',
      body: 'It may be asleep or turned off. Try waking it, or check that it is switched on and connected to the internet.'
    },
    device_unknown: {
      title: 'That PC is no longer set up',
      body: 'ALL SHARE may have been removed from it. Install it again and pair this device.'
    },
    not_paired: {
      title: 'This device is not paired with that PC',
      body: 'Open ALL SHARE on the PC, choose "Add a device", and enter the code it shows.'
    },
    pair_bad_code: {
      title: 'That code did not work',
      body: 'Check the code on your PC. Codes expire after a few minutes, and each one can be used only once.'
    },
    pair_expired: {
      title: 'That code has expired',
      body: 'Ask your PC for a new code and try again.'
    },
    rate_limited: {
      title: 'Too many attempts',
      body: 'Please wait a minute and try again.'
    },
    unauthorized: {
      title: 'This device was not recognised',
      body: 'Try pairing with your PC again.'
    },
    busy: {
      title: 'That PC is already in use',
      body: 'Someone else is connected to it right now.'
    },
    version_mismatch: {
      title: 'ALL SHARE needs updating',
      body: 'This client is older than the service it is talking to. Download the latest version.'
    },
    ice_failed: {
      title: "We couldn't reach your PC",
      body: 'Both networks are blocking a direct connection and no relay was available. A different Wi-Fi network usually fixes this.'
    },
    connection_failed: {
      title: 'The connection dropped',
      body: 'This is usually a network problem. ALL SHARE will keep trying.'
    },
    timeout: {
      title: "We couldn't reach your PC",
      body: 'Your PC did not answer. Check that it is awake and connected to the internet.'
    },
    identity_mismatch: {
      title: 'We could not verify that PC',
      body: 'The PC did not prove it is the one you paired with. For your safety the connection was refused. If you reinstalled ALL SHARE on that PC, pair it again.'
    },
    pairing_broken: {
      title: 'This PC needs pairing again',
      body: 'The saved details for it are no longer usable.'
    },
    wake_unavailable: {
      title: "That PC can't be woken from here",
      body: 'Waking needs either another ALL SHARE PC on the same home network, or scheduled check-ins turned on in ALL SHARE on the PC.'
    },
    bad_address: {
      title: 'That service address is not valid',
      body: 'It should look like wss://allshare.example.com/rv'
    },
    internal: {
      title: 'Something went wrong',
      body: 'Please try again. If it keeps happening, check that your ALL SHARE service is running.'
    }
  };

  UI.friendlyError = function (err) {
    if (!err) return FRIENDLY.internal;
    const code = err.code || '';
    if (FRIENDLY[code]) return FRIENDLY[code];
    if (err.userMessage) {
      return { title: err.userMessage, body: '' };
    }
    if (err.message && !/^[A-Z_]+$/.test(err.message)) {
      return { title: 'Something went wrong', body: err.message };
    }
    return FRIENDLY.internal;
  };

  UI.errorDetail = function (err) {
    if (!err) return '';
    const parts = [];
    if (err.code) parts.push('code: ' + err.code);
    if (err.detail) parts.push(err.detail);
    if (err.message && err.message !== err.userMessage) parts.push(err.message);
    if (err.stack && AS.Log.isDebug()) parts.push(err.stack);
    return parts.join('\n');
  };

  UI.FRIENDLY_ERRORS = FRIENDLY;
  AS.UI = UI;
})(window.AllShare);
