/*
 * ALL SHARE — the pairing flow.
 *
 * Twelve characters, entered once, and the two devices trust each other
 * forever. The code boxes accept a paste of the whole code, advance and retreat
 * as the user types, and fold the characters people habitually mistype.
 *
 * The key derivation genuinely takes about a second on a Chromebook — that cost
 * is what makes guessing the code hopeless — so the UI says what it is doing
 * rather than appearing to freeze.
 */
(function (AS) {
  'use strict';

  const U = AS.Util;
  const UI = AS.UI;
  const Pair = AS.Pairing;

  const PairingUI = {};

  PairingUI.open = function (app) {
    const boxes = [];
    const groups = U.h('div', { class: 'codeboxes' });

    for (let g = 0; g < 3; g++) {
      const group = U.h('div', { class: 'codegroup' });
      for (let i = 0; i < 4; i++) {
        const index = g * 4 + i;
        const box = U.h('input', {
          class: 'codebox', type: 'text', maxlength: '1',
          inputmode: 'latin', autocapitalize: 'characters',
          autocomplete: 'off', autocorrect: 'off', spellcheck: 'false',
          'aria-label': 'Pairing code character ' + (index + 1)
        });
        boxes.push(box);
        group.appendChild(box);
      }
      groups.appendChild(group);
    }

    const errorEl = U.h('p', { class: 'field__error', hidden: true });
    const statusEl = U.h('div', { class: 'pairwait', hidden: true }, [
      U.h('div', { class: 'spinner spinner--sm' }),
      U.h('span', { class: 'dim', text: 'Checking the code…' })
    ]);

    const form = U.h('div', { class: 'codeform' }, [
      U.h('ol', { class: 'pairsteps' }, [
        U.h('li', { html: 'Open <strong>ALL SHARE</strong> on your Windows PC.' }),
        U.h('li', { html: 'Choose <strong>Add a device</strong>.' }),
        U.h('li', { text: 'Type the code it shows below.' })
      ]),
      groups,
      errorEl,
      statusEl
    ]);

    let submitting = false;
    const modal = UI.modal({
      title: 'Add a PC',
      body: form,
      actions: [
        { label: 'Cancel' },
        {
          label: 'Pair', variant: 'primary', keepOpen: true,
          onClick: function () { submit(); return false; }
        }
      ]
    });

    wireBoxes(boxes, function () { submit(); });
    setTimeout(function () { boxes[0].focus(); }, 60);

    function readCode() {
      return boxes.map(function (b) { return b.value; }).join('');
    }

    function setBusy(busy, label) {
      submitting = busy;
      statusEl.hidden = !busy;
      if (busy && label) statusEl.lastChild.textContent = label;
      boxes.forEach(function (b) { b.disabled = busy; });
      const pairButton = modal.dialog.querySelector('.modal__foot .btn--primary');
      if (pairButton) pairButton.disabled = busy;
    }

    function showError(message) {
      errorEl.textContent = message;
      errorEl.hidden = false;
      setBusy(false);
    }

    async function submit() {
      if (submitting) return;
      errorEl.hidden = true;

      const raw = readCode();
      const check = Pair.validate(raw);
      if (!check.ok) {
        showError(check.reason);
        const firstEmpty = boxes.find(function (b) { return !b.value; });
        (firstEmpty || boxes[0]).focus();
        return;
      }

      setBusy(true, 'Checking the code…');
      try {
        const result = await PairingUI.run(app, check.code, function (stage) { setBusy(true, stage); });
        modal.close();
        UI.toast({
          kind: 'ok',
          title: 'Paired with ' + result.name,
          text: 'You can connect to it whenever it is online.'
        });
        app.refreshDevices();
      } catch (err) {
        AS.Log.warn('pairing failed', err);
        const friendly = UI.friendlyError(err);
        showError(friendly.body ? friendly.title + ' ' + friendly.body : friendly.title);
        boxes.forEach(function (b) { b.value = ''; b.classList.remove('is-filled'); });
        boxes[0].focus();
      }
    }
  };

  /**
   * Run the pairing exchange.
   *
   * The client never trusts the service's word about which PC this is: the
   * device name, both identity keys and the session salt are all bound into the
   * transcript, and the PC's confirmation tag is verified before its identity
   * key is stored. A service that substituted a key would fail this check.
   */
  PairingUI.run = async function (app, code, onStage) {
    const rv = app.rv;
    if (!rv.isConnected()) {
      throw AS.sessionFailure('internal', 'ALL SHARE is not connected to your service yet.',
        'The rendezvous connection is not established.');
    }

    onStage('Checking the code…');
    const codeId = await Pair.deriveCodeId(code);

    const lookup = await rv.request('pairLookup', { codeId: codeId });
    const offer = lookup.body || {};
    if (!offer.epk || !offer.salt || !offer.idPub) {
      throw AS.sessionFailure('pair_bad_code', 'That code did not work.', 'The service returned an incomplete pairing offer.');
    }

    onStage('Setting up a secure link…');
    const salt = U.fromB64(offer.salt);
    const agentEpk = U.fromB64(offer.epk);
    const agentIdPub = U.fromB64(offer.idPub);
    if (agentIdPub.length !== 32) {
      throw AS.sessionFailure('pair_bad_code', 'That PC sent an identity we could not read.', 'Identity key was not 32 bytes.');
    }

    const password = await Pair.derivePassword(code, salt);
    const ephemeral = await Pair.newEphemeral();
    const shared = await Pair.shared(ephemeral, agentEpk);

    const clientLabel = AS.Store.get('deviceLabel') || U.guessDeviceLabel();
    const clientIdPub = U.fromB64(AS.Identity.deviceId);

    const transcript = {
      codeId: codeId,
      salt: salt,
      agentEpk: agentEpk,
      clientEpk: ephemeral.publicBytes,
      agentIdPub: agentIdPub,
      clientIdPub: clientIdPub,
      deviceName: offer.name || '',
      clientLabel: clientLabel
    };
    const master = await Pair.master(shared, password, transcript);
    const confirm = await Pair.confirmTag(master, Pair.ROLE_CLIENT);

    onStage('Waiting for your PC…');
    const resultPromise = waitForPairResult(rv, 30000);
    rv.send('pairSubmit', {
      codeId: codeId,
      epk: U.toB64(ephemeral.publicBytes),
      idPub: AS.Identity.deviceId,
      label: clientLabel,
      confirm: U.toB64(confirm)
    });

    const result = await resultPromise;
    if (!result.ok) {
      throw AS.sessionFailure(result.code || 'pair_bad_code',
        'That code did not work.', 'The PC refused the pairing attempt.');
    }

    // The PC's own confirmation proves it derived the same secret from the same
    // code — which means nothing sat in the middle and altered the exchange.
    const agentConfirm = U.fromB64(result.confirm || '');
    if (!(await Pair.verifyConfirm(master, Pair.ROLE_AGENT, agentConfirm))) {
      throw AS.sessionFailure('identity_mismatch',
        'We could not verify that PC.',
        'The PC returned a key confirmation that did not match. Something may be interfering with the connection.');
    }
    if (result.idPub && result.idPub !== offer.idPub) {
      throw AS.sessionFailure('identity_mismatch',
        'We could not verify that PC.',
        'The identity key changed between the offer and the result.');
    }

    const record = AS.Store.addDevice({
      deviceId: result.deviceId || offer.deviceId,
      name: result.name || offer.name || 'Windows PC',
      identityPub: offer.idPub
    });
    AS.Log.info('paired with', record.name, await AS.Identity.fingerprintOf(agentIdPub));
    return record;
  };

  function waitForPairResult(rv, timeoutMs) {
    return new Promise(function (resolve, reject) {
      const timer = setTimeout(function () {
        rv.off('pairResult', onResult);
        reject(AS.sessionFailure('timeout', 'Your PC did not answer.',
          'No pairing result arrived within ' + (timeoutMs / 1000) + ' seconds.'));
      }, timeoutMs);
      function onResult(body) {
        clearTimeout(timer);
        rv.off('pairResult', onResult);
        resolve(body || {});
      }
      rv.on('pairResult', onResult);
    });
  }

  /**
   * Make the code boxes behave the way people expect: type to advance,
   * backspace to retreat, paste anywhere to fill everything, arrows to move.
   */
  function wireBoxes(boxes, onComplete) {
    function setValue(index, char) {
      boxes[index].value = char;
      boxes[index].classList.toggle('is-filled', !!char);
    }

    boxes.forEach(function (box, index) {
      box.addEventListener('input', function () {
        const cleaned = Pair.canonicalize(box.value);
        if (!cleaned) { setValue(index, ''); return; }
        // Typing fast, or an autocomplete, can deliver several characters at
        // once; spread them across the following boxes.
        let cursor = index;
        for (const ch of cleaned) {
          if (cursor >= boxes.length) break;
          setValue(cursor, ch);
          cursor++;
        }
        if (cursor < boxes.length) boxes[cursor].focus();
        else { boxes[boxes.length - 1].focus(); maybeComplete(); }
      });

      box.addEventListener('keydown', function (event) {
        if (event.key === 'Backspace' && !box.value && index > 0) {
          event.preventDefault();
          setValue(index - 1, '');
          boxes[index - 1].focus();
        } else if (event.key === 'ArrowLeft' && index > 0) {
          event.preventDefault();
          boxes[index - 1].focus();
        } else if (event.key === 'ArrowRight' && index < boxes.length - 1) {
          event.preventDefault();
          boxes[index + 1].focus();
        } else if (event.key === 'Enter') {
          event.preventDefault();
          onComplete();
        }
      });

      box.addEventListener('paste', function (event) {
        event.preventDefault();
        const text = (event.clipboardData || window.clipboardData).getData('text');
        const cleaned = Pair.canonicalize(text);
        for (let i = 0; i < boxes.length; i++) setValue(i, cleaned[i] || '');
        boxes[Math.min(cleaned.length, boxes.length - 1)].focus();
        maybeComplete();
      });

      box.addEventListener('focus', function () { box.select(); });
    });

    function maybeComplete() {
      const full = boxes.every(function (b) { return !!b.value; });
      if (full) onComplete();
    }
  }

  AS.PairingUI = PairingUI;
})(window.AllShare);
