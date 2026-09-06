/*
 * ALL SHARE — pairing, browser side.
 *
 * Mirrors shared/pair. The user reads a twelve-character code off their PC and
 * types it here; that code is the password input to an authenticated key
 * exchange, not a bearer token the rendezvous could steal and reuse.
 *
 *   codeID  = PBKDF2(code, fixed salt)        — a lookup handle
 *   pw      = PBKDF2(code, session salt)      — the authenticating secret
 *   Z       = X25519(our ephemeral, their ephemeral)
 *   master  = HKDF(Z ‖ pw, salt = transcript)
 *   confirm = HMAC(master, "client" | "agent")
 *
 * Both sides check the other's confirmation before trusting anything, and the
 * transcript covers both identity keys, so a rendezvous that substitutes a key
 * produces a mismatch and the pairing is refused.
 *
 * The KDF work is deliberately heavy: it is what makes guessing a 60-bit code
 * hopeless. On a mid-range Chromebook the two derivations take about a second,
 * which is why the UI shows progress rather than appearing to hang.
 */
(function (AS) {
  'use strict';

  const U = AS.Util;
  const Pairing = {};

  Pairing.CODE_LENGTH = 12;
  Pairing.KDF_ITERATIONS = 210000;
  Pairing.SALT_BYTES = 16;

  const ALPHABET = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
  const FIXED_ID_SALT = 'ALLSHARE-PAIR-ID-v1';
  const PW_SALT_PREFIX = 'ALLSHARE-PAIR-PW-v1';
  const HKDF_INFO = 'allshare-pair-master';
  const CONFIRM_LABEL = 'ALLSHARE-PAIR-CONFIRM-v1';

  Pairing.ROLE_CLIENT = 'client';
  Pairing.ROLE_AGENT = 'agent';

  /**
   * Normalise what the user typed.
   *
   * The alphabet has no I, L, O or U, so those characters can only be
   * mistakes — folded to the digits and letters they are usually mistaken for.
   * Separators and case are ignored so a code can be typed however it reads.
   */
  Pairing.canonicalize = function (input) {
    let out = '';
    for (const raw of String(input).toUpperCase()) {
      let ch = raw;
      if (' -_\t\n\r.'.indexOf(ch) >= 0) continue;
      if (ch === 'O') ch = '0';
      else if (ch === 'I' || ch === 'L') ch = '1';
      else if (ch === 'U') ch = 'V';
      out += ch;
    }
    return out;
  };

  Pairing.format = function (code) {
    const clean = Pairing.canonicalize(code);
    const groups = [];
    for (let i = 0; i < clean.length; i += 4) groups.push(clean.slice(i, i + 4));
    return groups.join('-');
  };

  /** Cheap shape check, so a typo fails instantly instead of after a second of KDF. */
  Pairing.validate = function (code) {
    const clean = Pairing.canonicalize(code);
    if (clean.length !== Pairing.CODE_LENGTH) {
      return { ok: false, reason: 'A pairing code has ' + Pairing.CODE_LENGTH + ' characters.' };
    }
    for (const ch of clean) {
      if (ALPHABET.indexOf(ch) < 0) {
        return { ok: false, reason: 'That code contains a character ALL SHARE does not use.' };
      }
    }
    return { ok: true, code: clean };
  };

  async function pbkdf2(password, saltBytes, lengthBytes) {
    const key = await crypto.subtle.importKey('raw', U.utf8(password), 'PBKDF2', false, ['deriveBits']);
    const bits = await crypto.subtle.deriveBits(
      { name: 'PBKDF2', hash: 'SHA-256', salt: saltBytes, iterations: Pairing.KDF_ITERATIONS },
      key, lengthBytes * 8);
    return new Uint8Array(bits);
  }

  /** The public lookup handle for a code. */
  Pairing.deriveCodeId = async function (code) {
    const check = Pairing.validate(code);
    if (!check.ok) throw new Error(check.reason);
    return U.toB64(await pbkdf2(check.code, U.utf8(FIXED_ID_SALT), 16));
  };

  /** The authenticating secret for one pairing session. */
  Pairing.derivePassword = async function (code, saltBytes) {
    const check = Pairing.validate(code);
    if (!check.ok) throw new Error(check.reason);
    if (saltBytes.length !== Pairing.SALT_BYTES) {
      throw new Error('The PC sent an unexpected pairing salt.');
    }
    const salt = U.concatBytes([U.utf8(PW_SALT_PREFIX), saltBytes]);
    return pbkdf2(check.code, salt, 32);
  };

  /** Generate this side's ephemeral X25519 keypair. */
  Pairing.newEphemeral = async function () {
    const keys = await crypto.subtle.generateKey({ name: 'X25519' }, true, ['deriveBits']);
    const raw = new Uint8Array(await crypto.subtle.exportKey('raw', keys.publicKey));
    return { keys: keys, publicBytes: raw };
  };

  /** X25519 agreement against the peer's ephemeral public key. */
  Pairing.shared = async function (ephemeral, peerPublicBytes) {
    if (!peerPublicBytes || peerPublicBytes.length !== 32) {
      throw new Error('The PC sent an unexpected pairing key.');
    }
    let peerKey;
    try {
      peerKey = await crypto.subtle.importKey('raw', peerPublicBytes, { name: 'X25519' }, false, []);
    } catch (err) {
      throw new Error('The PC sent an unexpected pairing key.');
    }
    const bits = await crypto.subtle.deriveBits(
      { name: 'X25519', public: peerKey }, ephemeral.keys.privateKey, 256);
    return new Uint8Array(bits);
  };

  /**
   * Hash the transcript: every public value in the exchange, in a fixed order,
   * each length-prefixed so no two different transcripts can collide.
   */
  Pairing.transcriptHash = async function (t) {
    const parts = [];
    const push = function (bytes) {
      const len = new Uint8Array(4);
      new DataView(len.buffer).setUint32(0, bytes.length, true);
      parts.push(len, bytes);
    };
    push(U.utf8(AS.Identity.DOMAIN_PAIR));
    push(U.utf8(t.codeId));
    push(t.salt);
    push(t.agentEpk);
    push(t.clientEpk);
    push(t.agentIdPub);
    push(t.clientIdPub);
    push(U.utf8(t.deviceName || ''));
    push(U.utf8(t.clientLabel || ''));
    return new Uint8Array(await crypto.subtle.digest('SHA-256', U.concatBytes(parts)));
  };

  /** Derive the pairing master secret. */
  Pairing.master = async function (sharedSecret, password, transcript) {
    const ikm = U.concatBytes([sharedSecret, password]);
    const salt = await Pairing.transcriptHash(transcript);
    const base = await crypto.subtle.importKey('raw', ikm, 'HKDF', false, ['deriveBits']);
    const bits = await crypto.subtle.deriveBits(
      { name: 'HKDF', hash: 'SHA-256', salt: salt, info: U.utf8(HKDF_INFO) }, base, 256);
    return new Uint8Array(bits);
  };

  /** One side's key-confirmation tag. The two roles produce different tags. */
  Pairing.confirmTag = async function (master, role) {
    const key = await crypto.subtle.importKey('raw', master, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
    const input = U.concatBytes([U.utf8(CONFIRM_LABEL), new Uint8Array([0]), U.utf8(role)]);
    return new Uint8Array(await crypto.subtle.sign('HMAC', key, input));
  };

  /** Constant-time confirmation check. */
  Pairing.verifyConfirm = async function (master, role, received) {
    const expected = await Pairing.confirmTag(master, role);
    return U.bytesEqual(expected, received);
  };

  AS.Pairing = Pairing;
})(window.AllShare);
