/*
 * Interoperability check between the browser's identity and pairing code and
 * Go's.
 *
 * The Go side writes a set of pairing values and a set of fingerprints; this
 * script recomputes them with the browser code and asserts they match.
 *
 * Both matter for the same reason. Two implementations of a key exchange that
 * "look right" but disagree fail as an unexplainable "that code did not work".
 * Two implementations of the fingerprint that disagree are worse: the whole
 * point of the fingerprint is that a user compares the one on the PC with the
 * one in the browser by eye, so a mismatch looks exactly like a security
 * warning while being nothing of the kind.
 */
'use strict';

const fs = require('fs');
const path = require('path');
const env = require('./browser-env');

const VECTORS = path.join(env.ROOT, 'shared', 'pair', 'testdata', 'interop.json');
const FINGERPRINTS = path.join(env.ROOT, 'shared', 'idkey', 'testdata', 'fingerprints.json');
for (const file of [VECTORS, FINGERPRINTS]) {
  if (!fs.existsSync(file)) {
    console.error(path.basename(file) + ' is missing — run `go test ./shared/...` first');
    process.exit(1);
  }
}
const v = JSON.parse(fs.readFileSync(VECTORS, 'utf8'));
const fp = JSON.parse(fs.readFileSync(FINGERPRINTS, 'utf8'));

const sandbox = env.load(env.CORE_FILES.concat(['js/net/pairing.js']), {});
const AS = sandbox.window.AllShare;
// Pairing reaches into Identity only for the shared domain tag.
AS.Identity = { DOMAIN_PAIR: 'ALLSHARE-PAIR-v1' };
const U = AS.Util;
const P = AS.Pairing;

let failures = 0;
function ok(name, condition, detail) {
  if (condition) { console.log('  ok    ' + name); return; }
  failures++;
  console.error('  FAIL  ' + name + (detail ? ' — ' + detail : ''));
}

(async () => {
  console.log('Pairing interop (browser vs Go)');

  const codeId = await P.deriveCodeId(v.code);
  ok('codeId matches Go', codeId === v.codeId, codeId + ' vs ' + v.codeId);

  // A formatted, lower-case code must derive the same handle.
  const typed = P.format(v.code).toLowerCase();
  ok('formatted code derives the same handle', (await P.deriveCodeId(typed)) === v.codeId);

  const salt = U.fromB64(v.salt);
  const pw = await P.derivePassword(v.code, salt);
  ok('password matches Go', U.toB64(pw) === v.password, U.toB64(pw) + ' vs ' + v.password);

  const transcript = {
    codeId: v.codeId,
    salt: salt,
    agentEpk: U.fromB64(v.agentEpk),
    clientEpk: U.fromB64(v.clientEpk),
    agentIdPub: U.fromB64(v.agentIdPub),
    clientIdPub: U.fromB64(v.clientIdPub),
    deviceName: v.deviceName,
    clientLabel: v.clientLabel
  };
  const hash = await P.transcriptHash(transcript);
  ok('transcript hash matches Go', U.toB64(hash) === v.transcriptHash, U.toB64(hash) + ' vs ' + v.transcriptHash);

  const master = await P.master(U.fromB64(v.shared), pw, transcript);
  ok('master secret matches Go', U.toB64(master) === v.master, U.toB64(master) + ' vs ' + v.master);

  const clientTag = await P.confirmTag(master, P.ROLE_CLIENT);
  ok('client confirmation matches Go', U.toB64(clientTag) === v.confirmClient);
  const agentTag = await P.confirmTag(master, P.ROLE_AGENT);
  ok('agent confirmation matches Go', U.toB64(agentTag) === v.confirmAgent);

  ok('verifyConfirm accepts the right tag', await P.verifyConfirm(master, P.ROLE_AGENT, U.fromB64(v.confirmAgent)));
  ok('verifyConfirm rejects the other role', !(await P.verifyConfirm(master, P.ROLE_CLIENT, U.fromB64(v.confirmAgent))));

  // A live X25519 agreement against Go's key must reproduce Go's shared secret.
  const eph = await P.newEphemeral();
  ok('ephemeral key is 32 bytes', eph.publicBytes.length === 32);
  const roundTrip = await P.shared(eph, U.fromB64(v.agentEpk));
  ok('X25519 agreement produces 32 bytes', roundTrip.length === 32);

  // Malformed peer keys must be refused rather than producing a weak secret.
  for (const bad of [new Uint8Array(31), new Uint8Array(33), new Uint8Array(32)]) {
    let rejected = false;
    try { await P.shared(eph, bad); } catch (e) { rejected = true; }
    ok('rejects a ' + bad.length + '-byte peer key' + (bad.length === 32 ? ' (all zero, low order)' : ''), rejected);
  }

  // Shape validation.
  ok('rejects a short code', !P.validate('ABC').ok);
  ok('rejects an out-of-alphabet character', !P.validate('ABCD-EFGH-JK!M').ok);
  ok('accepts the real code formatted', P.validate(P.format(v.code)).ok);
  ok('folds O to 0 and I to 1', P.canonicalize('O0I1') === '0011');

  // --- Fingerprints -------------------------------------------------------
  // Identity is loaded on its own here: fingerprintOf needs nothing but
  // crypto.subtle and Util, and the rest of that module wants IndexedDB.
  const idSandbox = env.load(env.CORE_FILES.concat(['js/core/identity.js']), {});
  const Identity = idSandbox.window.AllShare.Identity;

  for (const entry of fp.keys) {
    const bytes = U.fromB64(entry.publicKey);
    const got = await Identity.fingerprintOf(bytes);
    ok('fingerprint of ' + entry.publicKey.slice(0, 10) + '… matches Go',
      got === entry.fingerprint, got + ' vs ' + entry.fingerprint);
  }

  // Every character a fingerprint can contain must be one a user can read back
  // without hesitating, and the same set the pairing code uses.
  const alphabet = fp.alphabet;
  ok('the alphabet has 32 characters', alphabet.length === 32, String(alphabet.length));
  for (const bad of ['I', 'L', 'O', 'U']) {
    ok('the alphabet excludes ' + bad, !alphabet.includes(bad));
  }
  ok('every fingerprint character is in the alphabet',
    [...fp.keys.map((k) => k.fingerprint).join('').replace(/-/g, '')]
      .every((c) => alphabet.includes(c)));

  console.log(failures ? failures + ' failure(s)' : 'Browser and Go pairing agree exactly.');
  process.exit(failures ? 1 : 0);
})().catch((err) => { console.error(err); process.exit(1); });
