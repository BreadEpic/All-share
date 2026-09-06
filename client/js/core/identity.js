/*
 * ALL SHARE — this device's cryptographic identity.
 *
 * The client owns an Ed25519 keypair generated on first run. Its public half is
 * the device ID the rendezvous knows it by; its private half signs the
 * authentication challenge and every signalling payload.
 *
 * The private key is generated as non-extractable and stored in IndexedDB as a
 * live CryptoKey. It therefore never exists as bytes in JavaScript and cannot
 * be read out of storage even by code running on this page — a meaningfully
 * stronger position than keeping a base64 secret in localStorage.
 *
 * Verified on Chromium from file://: Ed25519, X25519, PBKDF2, HKDF and
 * IndexedDB are all available, because Chromium treats file:// as a secure
 * context.
 */
(function (AS) {
  'use strict';

  const DB_NAME = 'allshare';
  const DB_VERSION = 1;
  const STORE = 'identity';
  const RECORD_KEY = 'device-identity-v1';

  const U = AS.Util;

  function openDatabase() {
    return new Promise(function (resolve, reject) {
      let request;
      try {
        request = indexedDB.open(DB_NAME, DB_VERSION);
      } catch (err) {
        reject(new Error('This browser will not let ALL SHARE store its device key.'));
        return;
      }
      request.onupgradeneeded = function () {
        const db = request.result;
        if (!db.objectStoreNames.contains(STORE)) db.createObjectStore(STORE);
      };
      request.onsuccess = function () { resolve(request.result); };
      request.onerror = function () { reject(request.error || new Error('could not open local storage')); };
      request.onblocked = function () { reject(new Error('another ALL SHARE tab is blocking storage')); };
    });
  }

  function transact(db, mode, fn) {
    return new Promise(function (resolve, reject) {
      const tx = db.transaction(STORE, mode);
      const store = tx.objectStore(STORE);
      let result;
      try {
        result = fn(store);
      } catch (err) {
        reject(err);
        return;
      }
      tx.oncomplete = function () {
        resolve(result && result.result !== undefined ? result.result : result);
      };
      tx.onerror = function () { reject(tx.error); };
      tx.onabort = function () { reject(tx.error || new Error('storage transaction aborted')); };
    });
  }

  const Identity = {
    /** @type {CryptoKeyPair|null} */
    keys: null,
    /** @type {string} base64url of the raw public key */
    deviceId: '',
    fingerprint: ''
  };

  /**
   * Load the device identity, generating one on first run.
   *
   * If IndexedDB is unavailable the app still works for the current session
   * with an in-memory identity; it simply has to be paired again next time.
   * Refusing to start would be a worse outcome than a degraded one.
   */
  Identity.load = async function () {
    let db = null;
    try {
      db = await openDatabase();
    } catch (err) {
      AS.Log.warn('persistent storage unavailable, using a session-only identity', err);
    }

    if (db) {
      try {
        const saved = await transact(db, 'readonly', function (store) { return store.get(RECORD_KEY); });
        if (saved && saved.privateKey && saved.publicKey) {
          Identity.keys = { privateKey: saved.privateKey, publicKey: saved.publicKey };
          await Identity._deriveIds();
          AS.Log.info('device identity loaded', Identity.fingerprint);
          return Identity;
        }
      } catch (err) {
        AS.Log.warn('stored device identity could not be read; generating a new one', err);
      }
    }

    Identity.keys = await crypto.subtle.generateKey({ name: 'Ed25519' }, false, ['sign', 'verify']);
    await Identity._deriveIds();

    if (db) {
      try {
        await transact(db, 'readwrite', function (store) {
          store.put({ privateKey: Identity.keys.privateKey, publicKey: Identity.keys.publicKey }, RECORD_KEY);
        });
        AS.Log.info('generated a new device identity', Identity.fingerprint);
      } catch (err) {
        // Chromium can only structured-clone a non-extractable key into
        // IndexedDB; if that ever fails we keep going with a session identity
        // rather than losing the ability to connect at all.
        AS.Log.warn('device identity could not be saved; it will not survive a reload', err);
      }
    }
    return Identity;
  };

  Identity._deriveIds = async function () {
    const raw = await crypto.subtle.exportKey('raw', Identity.keys.publicKey);
    Identity.deviceId = U.toB64(new Uint8Array(raw));
    Identity.fingerprint = await Identity.fingerprintOf(new Uint8Array(raw));
  };

  /**
   * Render a short, human-comparable fingerprint of a public key.
   * Matches the Go implementation exactly so both ends show the same string.
   */
  Identity.fingerprintOf = async function (publicKeyBytes) {
    const input = U.concatBytes([U.utf8('ALLSHARE-FP-v1'), publicKeyBytes]);
    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', input));
    const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789';
    let bits = 0;
    let acc = 0;
    let out = '';
    for (let i = 0; i < 8 && out.length < 12; i++) {
      acc = (acc << 8) | digest[i];
      bits += 8;
      while (bits >= 5 && out.length < 12) {
        bits -= 5;
        out += alphabet[(acc >> bits) & 31];
      }
    }
    return out.slice(0, 4) + '-' + out.slice(4, 8) + '-' + out.slice(8, 12);
  };

  /**
   * Sign domain-tagged content.
   *
   * The layout matches shared/idkey exactly: the domain tag, a zero byte, then
   * each part with a little-endian 32-bit length prefix. Length-prefixing is
   * what stops ("ab","c") and ("a","bc") producing the same signed bytes.
   */
  Identity.sign = async function (domain, parts) {
    const input = buildSigningInput(domain, parts);
    const signature = await crypto.subtle.sign({ name: 'Ed25519' }, Identity.keys.privateKey, input);
    return new Uint8Array(signature);
  };

  /** Verify a signature made by a peer's public key. */
  Identity.verify = async function (publicKeyBytes, domain, signature, parts) {
    let key;
    try {
      key = await crypto.subtle.importKey('raw', publicKeyBytes, { name: 'Ed25519' }, false, ['verify']);
    } catch (err) {
      return false;
    }
    if (!signature || signature.length !== 64) return false;
    try {
      return await crypto.subtle.verify({ name: 'Ed25519' }, key, signature, buildSigningInput(domain, parts));
    } catch (err) {
      return false;
    }
  };

  function buildSigningInput(domain, parts) {
    const chunks = [U.utf8(domain), new Uint8Array([0])];
    for (const part of parts) {
      const bytes = part instanceof Uint8Array ? part : U.utf8(String(part));
      const length = new Uint8Array(4);
      new DataView(length.buffer).setUint32(0, bytes.length, true);
      chunks.push(length, bytes);
    }
    return U.concatBytes(chunks);
  }

  Identity.DOMAIN_AUTH = 'ALLSHARE-AUTH-v1';
  Identity.DOMAIN_SIGNAL = 'ALLSHARE-SIGNAL-v1';
  Identity.DOMAIN_PAIR = 'ALLSHARE-PAIR-v1';

  AS.Identity = Identity;
})(window.AllShare);
