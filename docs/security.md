# Security

This document explains what ALL SHARE protects, what it does not protect, and
exactly how each protection works. The first section is written for anyone; the
sections after it are for people who want to check the claims.

If you find a problem, the fastest thing you can do is unpair the device
(**Settings → Devices → Forget**) and stop the ALL SHARE service on the PC
(`sc stop AllShareAgent` from an administrator command prompt). Both take
effect immediately.

---

## 1. In plain language

**What ALL SHARE gives away if it is working correctly:** nothing. Video,
audio, keystrokes, mouse movement and clipboard text travel encrypted from
your PC to your browser and back. The middle server that introduces the two
sides cannot read any of it.

**What ALL SHARE gives away if someone steals your Chromebook while it is
unlocked:** everything, immediately. A paired device *is* the key. There is no
second password on top of pairing. That is a deliberate trade — see
[Section 8](#8-deliberate-trade-offs) — and it means the single most important
thing you can do is put a screen lock on any device you pair.

**Who can connect to your PC:** only a device you paired by typing a 12-character
code that your PC displayed. Nobody else, including the server operator,
including us, including someone who has completely taken over the middle server.

**What the middle server can see:** that a device with a certain public key
connected, from a certain IP address, at a certain time, and asked to reach a
certain PC. It cannot see your screen, your keys, your clipboard, or your
audio. It cannot inject input. It cannot join the session.

**What it cannot protect against:** anything already running on your PC. If
your PC has malware, ALL SHARE will faithfully stream the malware to you.
Remote access software cannot be more trustworthy than the machine it runs on.

---

## 2. Threat model

### 2.1 Assets

| Asset | Where it lives | Worst case if lost |
|---|---|---|
| Screen contents | Video stream, PC memory | Attacker sees everything you do |
| Keystrokes | Input data channel | Attacker sees passwords you type |
| Clipboard text | Control data channel | Attacker sees copied secrets |
| Client identity private key | Browser IndexedDB, non-extractable | Attacker can impersonate your Chromebook |
| Agent identity private key | `C:\ProgramData\ALL SHARE\identity.key`, 0600 | Attacker can impersonate your PC |
| Pairing code | Shown on the PC screen for 3 minutes | Attacker can pair a device of their own |
| TURN credential | Issued per session, hours of life | Attacker can relay their own traffic at your cost |

### 2.2 Adversaries we defend against

**A1 — Network attacker on the client's network.** Reads and modifies every
packet between the browser and the server, and between the browser and the PC.
Includes a hostile Wi-Fi access point and anyone who can see traffic on a
shared network.

**A2 — Fully compromised rendezvous server.** The introduction server is
malicious: it lies about which device is which, substitutes its own SDP, tries
to sit in the middle of the media path, and logs everything.

**A3 — Online attacker with no prior access.** Can reach the rendezvous server
over the internet, knows your PC exists, and wants in. Can guess, enumerate,
flood, and replay.

**A4 — Another user on the same LAN as the PC.** Can send packets directly to
the agent's listening ports and can see LAN broadcast traffic.

**A5 — Malicious paired peer.** A device you *did* pair, or a PC you *did*
connect to, now behaving badly and trying to attack the other side through the
protocol.

### 2.3 Adversaries we explicitly do not defend against

**N1 — Malware already running on the Windows PC.** It can read the agent's
memory, the identity key, and the framebuffer directly. Out of scope for any
remote-access tool.

**N2 — Physical access to an unlocked, paired client device.** The device is
the credential. A screen lock is your control here, not ours.

**N3 — A malicious browser or a malicious browser extension.** Extensions with
host permissions can read page memory. Nothing an in-page application can do
prevents that.

**N4 — Traffic analysis.** An observer learns *that* you are streaming, roughly
how much, and when. Video bitrate correlates with screen activity. We do not
pad traffic; padding a 15 Mbit/s video stream to hide activity is not
affordable.

**N5 — Rubber-hose, coercion, legal compulsion.** Out of scope.

---

## 3. Cryptography inventory

Every primitive in the system, why it was chosen, and where it runs.

| Purpose | Primitive | Where | Why this one |
|---|---|---|---|
| Device identity | Ed25519 | `shared/idkey`, `client/js/core/identity.js` | Deterministic, no nonce-reuse footgun, available in WebCrypto from a `file://` page, small keys |
| Signalling authenticity | Ed25519 over a length-prefixed transcript | same | Domain-separated so a signature for one purpose cannot be replayed for another |
| Pairing key agreement | X25519 | `shared/pair`, `client/js/net/pairing.js` | Standard, constant-time in both Go and WebCrypto |
| Pairing code stretch | PBKDF2-HMAC-SHA-256, 210 000 iterations | same | The only stretcher WebCrypto exposes; iteration count follows OWASP's 2023 guidance |
| Key derivation | HKDF-SHA-256 | same | Separates one shared secret into independent keys with clean domain labels |
| Pairing confirmation | HMAC-SHA-256 | same | Mutual proof-of-knowledge before either side commits |
| Transport (signalling) | TLS 1.2+ (WSS) | `coder/websocket` over Go's `crypto/tls` | Standard |
| Transport (media + data) | DTLS 1.2 with SRTP (AES-128-GCM or AES-CM-HMAC-SHA1) | Pion | The only encryption a browser will do for WebRTC; keys are negotiated end-to-end |
| TURN credentials | HMAC-SHA-1 over `expiry:username` | `internal/rendezvous/turnsvc` | The long-standing ephemeral-credential scheme every TURN server implements; SHA-1 here is a MAC with a short-lived key, not a collision-resistant hash |
| LAN peer grouping | HMAC-SHA-256 of the server-observed address | `internal/rendezvous/signal/hub.go` | Lets the server group devices without storing their IP addresses in the clear |

No primitive in this list is home-grown. There is no custom cipher, no custom
mode, and no place where the code decides whether a MAC matched using a
non-constant-time comparison — `crypto/hmac.Equal` and `crypto/subtle` are used
throughout, and WebCrypto's `verify` is constant-time by contract.

---

## 4. How each defence works

### 4.1 Device identity (defends A1, A2, A3, A5)

Every participant — each client, each PC agent, and the server itself — has an
Ed25519 keypair generated on first run. The public key *is* the device ID; there
is no registration, no account, and no username.

The client's private key is generated with `extractable: false` and stored as a
`CryptoKey` object in IndexedDB. It is never serialised, so there is no base64
blob for a script to read out of `localStorage`, and even a script running in
the page can only ask the browser to *use* the key, not to export it. The agent's
key is written to a 0600 file in a 0700 directory under `ProgramData`, which is
readable by SYSTEM and administrators only.

Every signature in the system goes through one function, which length-prefixes
each component before hashing:

```
sign(domain, part1, part2, ...) = Ed25519(domain || LE64(len(part1)) || part1 || LE64(len(part2)) || part2 || ...)
```

The length prefixes matter: without them, `("ab", "c")` and `("a", "bc")` hash
identically, and an attacker who controls where a boundary falls can make a
signature over one message verify as a signature over another. The domain
strings (`ALLSHARE-AUTH-v1`, `ALLSHARE-SIGNAL-v1`, `ALLSHARE-PAIR-v1`) keep the
three uses of the same key from ever overlapping.

### 4.2 Server sign-in (defends A3)

The server issues a fresh 32-byte random nonce and the peer signs
`(nonce, serverID, role)` under the auth domain. Binding the server's own
identity in means a signature captured by one server cannot be replayed against
another; binding the role in means a client's signature cannot be replayed to
claim an agent's slot.

The rate limiter runs **before** the signature verification and is keyed on both
the source address and the claimed device ID, so neither a single host nor a
single stolen ID can be used alone to force expensive verifications. Defaults:
10 attempts with a 0.5/s refill for sign-in, and 20 new sockets with a 1/s
refill per source address.

### 4.3 Pairing (defends A2, A3)

Pairing is the moment a device becomes trusted, so it gets the most attention.

The PC generates a 12-character code from a 32-character alphabet with `I`, `L`,
`O` and `U` removed — 60 bits of entropy, in five groups of readable characters.
The code is displayed on the PC's own screen. It is never sent anywhere: what
travels is a *handle* derived from it.

```
codeID   = PBKDF2(code, "ALLSHARE-PAIR-ID-v1", 210000)[0:16]      # the lookup handle
password = PBKDF2(code, sessionSalt, 210000)[0:32]                # the authenticator
```

Both sides then do an X25519 exchange and mix in the password, binding the whole
transcript:

```
transcript = SHA-256(codeID || agentEphemeralPub || clientEphemeralPub || agentIdentityPub || clientIdentityPub)
master     = HKDF-SHA-256(Z || password, salt = transcript, info = "ALLSHARE-PAIR-MASTER-v1")
tagAgent   = HMAC(master, "agent")
tagClient  = HMAC(master, "client")
```

Each side sends its tag and checks the other's before storing anything. This is
what makes the exchange **code-authenticated**: a man in the middle who does not
know the code produces a different `Z`, therefore a different `master`,
therefore a tag that does not verify — and the honest sides abandon the pairing
having stored nothing. A compromised server (A2) cannot pair itself to your PC.

Three limits contain online guessing:

- **One attempt per code.** `MaxAttempts = 1`. A wrong tag burns the code
  permanently; the PC must show a new one. An attacker gets one guess against
  2^60, not unlimited guesses.
- **Three-minute window.** The code expires whether or not it is used.
- **The tightest rate limit in the server** applies to pairing lookups: 5 with a
  0.1/s refill, keyed on source address *and* device ID.

Guessing a 60-bit code at 0.1 attempts per second, against a code that dies
after one wrong guess and expires in three minutes, is not an attack.

### 4.4 End-to-end signed signalling (defends A2 — this is the important one)

The rendezvous server relays SDP offers and answers. A malicious server would
love to substitute its own SDP, because SDP carries the **DTLS certificate
fingerprint** — the thing that decides who the media session is actually
encrypted to. Substituting it is a textbook man-in-the-middle.

So the payload is signed end-to-end, by the device identity keys, before the
server ever sees it:

```
sig = Ed25519_sender(DomainSignal, sessionID, senderID, recipientID, payloadBytes)
```

The receiver verifies with the *paired* peer's public key — the one it stored at
pairing time, not one the server supplied in this message. Recipient ID is
covered, so the server cannot take a payload addressed to one device and forward
it to another. Session ID is covered, so a payload from an old session cannot be
replayed into a new one.

The chain is: **pairing establishes the identity key → the identity key signs the
SDP → the SDP carries the DTLS fingerprint → DTLS derives the SRTP keys.** Every
link is verified by the endpoints. The server is reduced to an untrusted
mailbox, which is exactly what we want from a component that has to be reachable
from the internet.

Both sides refuse to proceed if verification fails, and the client shows
"Could not verify the PC's identity" rather than a stack trace.

### 4.5 Authorisation for a session (defends A3, A5)

Pairing is checked **twice, independently**:

1. The server checks its registry and refuses to introduce an unpaired client.
2. The agent checks its own config when the introduction arrives, and hangs up
   with "This device is not paired with this PC." if it does not recognise the
   caller.

Check 2 is the one that counts. A compromised server can skip check 1; it cannot
skip check 2, because that decision is made on your PC against a list only your
PC maintains. Unpairing on the PC is therefore final: no server-side state can
re-authorise a forgotten device.

The reverse direction is constrained too. A client can only remove *itself* from
a PC's paired list — the `forget` handler ignores any device ID in the request
body and uses the authenticated connection's own ID.

### 4.6 Input authority (defends A5)

Input arrives on a data channel inside the DTLS session, so it comes from the
paired peer or from nobody. Beyond that, the agent constrains what input can
express:

- Key events are carried as **HID usage IDs** and translated by a fixed table to
  PS/2 set-1 scancodes. The table has 256 entries; an out-of-range usage is
  dropped. There is no path by which a peer supplies a raw scancode or a raw
  Windows message.
- Mouse coordinates are normalised 0–65535 and clamped to the selected monitor's
  rectangle.
- Every input packet carries the client's full 256-bit held-key bitmap, and the
  agent diffs it against its own view. This is a robustness feature (see
  [performance.md](performance.md)) but it is also a containment feature: the
  agent's key state converges on the client's *declared* state, so a malformed
  or lost packet cannot leave a key held forever.
- Disconnect, for any reason, releases every held key and mouse button.

The agent runs input injection in the interactive desktop session, not as
SYSTEM — see [Section 6](#6-windows-privilege-boundaries).

### 4.7 Clipboard (defends N2 partially; user-controlled by design)

Clipboard sharing is off unless you turn it on, has a separate switch for each
direction, and only ever moves `text/plain`.

Crucially, it is driven by the browser's `copy` and `paste` **events**, not by
the Clipboard Read API. That means the browser only hands us clipboard content
at the moment you press Ctrl+C or Ctrl+V. There is no background polling and no
persistent read permission to grant — the page physically cannot read your
clipboard when you are not actively copying. The toolbar shows a brief
"Clipboard sent" indicator each time text moves, so a transfer is never silent.

Content larger than 256 KB is refused rather than truncated.

### 4.8 Transport (defends A1)

Two encrypted transports, no plaintext anywhere:

- **Signalling:** WSS (TLS 1.2+). The rendezvous process itself serves plain
  HTTP and expects TLS to be terminated by a reverse proxy in front of it —
  Caddy or nginx, which handle certificate issuance and renewal far better than
  a bespoke implementation would. [deployment.md](deployment.md) gives working
  configurations for both, and this is the only supported way to expose the
  service to the internet.

  Because the operator, not the code, terminates TLS, the *client* is where the
  requirement is enforced: both the browser client and the Windows agent refuse
  a `ws://` service address unless the host is on the local network (loopback,
  `10/8`, `172.16/12`, `192.168/16`, link-local, IPv6 loopback/ULA, or a
  `.local`/`localhost` name). The carve-out exists because a rendezvous
  self-hosted on your own LAN is a legitimate deployment and nobody can get a
  publicly trusted certificate for `192.168.1.10`; demanding one would only
  teach people to disable the check. Everything reachable from the internet must
  be `wss://`. The rule lives in one function per language —
  `rvclient.ValidateEndpoint` and `AS.Util.serviceAddressProblem` — and the two
  are tested against the same case list so they cannot drift apart.

- **Media and data:** DTLS 1.2 with SRTP, keys negotiated directly between the
  browser and your PC. The server never holds them.

`X-Forwarded-For` is honoured only when the operator passes `-trust-proxy`,
because the rate limiter keys on the client address: honouring the header by
default would let anyone spoof it and slip every per-address limit.

A note on `InsecureSkipVerify` in `internal/rendezvous/signal/conn.go`: this is
**not** TLS verification being disabled. It is `coder/websocket`'s
*origin* check, which compares the browser's `Origin` header against the `Host`
header. A page loaded from `file://` sends `Origin: null`, so the check would
reject the exact client this product is built around. Turning it off is safe
here because the origin check protects cookie-authenticated endpoints from
cross-site WebSocket hijacking, and this server has no cookies and no ambient
authority: every connection must sign a fresh challenge with a private key that
a hostile web page cannot obtain. The comment at that line says the same thing,
so the next person to read it does not "fix" it.

### 4.9 Wake (defends A3, A4)

Wake-on-LAN magic packets are unauthenticated by design — the network card is
asleep and cannot verify anything. We do not pretend otherwise:

- The server never sends a magic packet to a user-supplied address. Wake is
  relayed to an *already authenticated agent* on the same LAN, identified by
  `LANKeyFor(addr)` — an HMAC of the address the **server observed**, never an
  address the agent claimed. An agent cannot talk its way into a LAN group it is
  not actually on.
- The MAC address to wake comes from the target device's own registry entry,
  written when that device authenticated. A client cannot ask the server to
  spray magic packets at arbitrary MACs, which would make the server a
  reflector.
- Waking a PC only powers it on. It does not authorise anything: the ensuing
  connection goes through pairing checks like any other.

### 4.10 Availability (defends A3)

Five separate token buckets (connections, sign-in, pairing, session starts,
per-peer message rate), each keyed on both source address and identity, with
lazy refill and periodic GC so an attacker cycling source addresses cannot grow
the map without bound. Connections have a handshake deadline; frames have a size
cap; JSON bodies are decoded into fixed structs, and the codec list a client may
advertise is truncated to 64 entries before it is forwarded.

---

## 5. Security review

Performed against the checklist requested for this project. Each item states
what was looked for, what was found, and where to verify it.

### 5.1 Unauthenticated endpoints

**Looked for:** any handler reachable before the signature challenge completes.

**Found:** three. `/rv` is the WebSocket upgrade, which yields an
unauthenticated connection that is closed within fifteen seconds if it does not
complete the signature challenge. `/healthz` returns liveness, the version, the
server's public fingerprint and aggregate counts — no per-device data.
`/config` returns the version, server ID and realm, all of which a client needs
before it can authenticate and none of which is a secret. Every message handler in the hub is dispatched
only after `c.role` and `c.deviceID` are set, which happens only after
`pub.Verify` returns true. Handlers additionally check role: `connect`, `pair`
and `forget` reject anything that is not a client; registration rejects anything
that is not an agent.

**Status:** OK. `internal/rendezvous/signal/conn.go:205-280`, dispatch at
`hub.go`.

### 5.2 Command injection

**Looked for:** any construction of a shell command, `cmd.exe` invocation, or
`ShellExecute` from remote-controlled data.

**Found:** none. The agent runs exactly one external process — itself, in the
user session, via `CreateProcessAsUser` with an argv array (no shell, no string
parsing) and a fixed executable path taken from `os.Executable()`. The
installer script is not reachable from the network.

**Status:** OK.

### 5.3 Path traversal

**Looked for:** any file path derived from network input.

**Found:** none. Every path in the system is a constant joined to a
platform-standard base directory (`ProgramData`, `os.UserConfigDir`). Device
names, which *are* attacker-influenced, are passed through
`registry.SanitizeName` — which strips control characters and path separators
and truncates — and are used only as display strings, never as filenames. The
registry file is a single JSON blob keyed by public key, not a directory of
per-device files, so there is no filename to traverse.

**Status:** OK. `internal/rendezvous/registry/registry.go:352`.

### 5.4 Arbitrary file execution

**Looked for:** any code path that runs a binary chosen at runtime.

**Found:** one — the service supervisor relaunching the agent in the user
session. It uses `os.Executable()`, not a configured or received path.

**Status:** OK.

### 5.5 Unsafe WebSocket handling

**Looked for:** unbounded reads, blocking writes that could stall the reader,
missing deadlines, and the origin check.

**Found:** read limit set on every socket; a handshake deadline; writes go
through a buffered channel with a non-blocking send so a slow peer is
disconnected rather than allowed to back up the hub. The origin check is
disabled deliberately for `file://` clients, analysed in
[Section 4.8](#48-transport-defends-a1). During review this area produced one
real bug — a peer that failed the handshake had its socket closed before the
explanatory frame flushed, so users saw a generic disconnect instead of the
reason. Fixed by `drain()` with a two-second grace period.

**Status:** OK, with the origin exception documented at the call site.

### 5.6 Token and key leakage

**Looked for:** secrets in logs, in error strings, in URLs, or in messages sent
to the wrong party.

**Found:** logging uses `Fingerprint()` (a 12-character truncated hash) and
`shortID()` for identities. Nothing logs a private key, a pairing code, a
derived password, a master secret, or a clipboard body. TURN credentials are
logged as an expiry only. Grepping for the obvious identifiers finds them only
in the crypto code that must handle them:

```
$ grep -rn 'Log\|log\.' --include=*.go . | grep -i 'password\|privkey\|secret\|code)'
server/cmd/allshare-server/main.go:130:  log.Info("generated a relay secret", "path", ...)
```

The single hit logs the *path* the relay secret was written to, not its
contents. The client's log console, which the hidden debug mode exposes, is on
the same diet: it records that pairing succeeded and with which fingerprint,
never the code.

**Status:** OK.

### 5.7 Weak pairing

**Looked for:** entropy, guess limits, expiry, and whether the exchange is
actually authenticated by the code.

**Found:** 60 bits of entropy, one attempt, three-minute window, PBKDF2 at
210 000 iterations, mutual key confirmation before either side commits. The
password enters the KDF *with* the X25519 output, so knowing the code without
the exchange (or the exchange without the code) yields nothing.

The honest caveat, stated in the package documentation: this is a
code-authenticated exchange, not a full PAKE. A PAKE (CPace, SPAKE2) would
additionally resist *offline* brute force by an attacker who records a pairing
exchange and grinds it afterwards. We do not use one because no PAKE is
available in WebCrypto and implementing elliptic-curve hash-to-curve in
JavaScript, in a `file://` page, with no dependencies, would be a
constant-time hazard worse than the risk it addresses. Instead the risk is
retired by the one-attempt-and-expire policy: an attacker who records the
exchange can grind offline at PBKDF2 cost, ~2^60 / (rate at 210 000 iterations),
against a code that has already expired and can pair nothing.

**Status:** Acceptable, with the limitation documented rather than papered over.

### 5.8 Replay attacks

**Looked for:** any message that would be accepted twice, or accepted in a
context other than the one it was made for.

**Found:** the auth signature covers a server-issued random nonce plus the
server ID plus the role. The signalling signature covers session ID, sender,
recipient and payload. Pairing tags cover the full transcript including both
ephemeral keys. Session IDs are random and single-use; a session is removed from
the hub on teardown, so signalling for a closed session is rejected.

The input plane deliberately does *not* implement anti-replay, and this is
correct: it runs inside DTLS, and SRTP/DTLS already provide replay protection at
the transport layer. Adding a second sequence check above it would only add
latency.

**Status:** OK.

### 5.9 Insecure storage

**Looked for:** secrets at rest that a low-privileged local process could read.

**Found:** client private key is non-extractable in IndexedDB, which is
origin-scoped and cannot be read by another page. Agent identity key is 0600 in
a 0700 directory under `ProgramData`. The registry and pairing store are the
same. Settings in `localStorage` contain no secrets — quality preset, theme,
toolbar behaviour, and the *public* IDs of paired PCs.

One residual risk, stated plainly: on Windows, `ProgramData` permissions protect
the agent key from ordinary users but not from an administrator or from SYSTEM.
That is inherent — the service must read the key without a human present, so it
cannot be protected by a passphrase. Threat N1 covers it.

**Status:** OK.

### 5.10 Clipboard abuse

**Looked for:** silent capture, background reads, and unbounded transfers.

**Found:** off by default, per-direction switches, event-driven so there is no
background read capability at all, text only, 256 KB cap, visible indicator on
every transfer.

**Status:** OK.

### 5.11 Unauthorised input control

**Looked for:** any way to reach `SendInput` without being a paired peer inside
an established DTLS session.

**Found:** none. The input handler is a method on a live session; sessions are
created only after the agent's own pairing check. Malformed packets are dropped
by length check before parsing (every input message is fixed-size).

**Status:** OK.

### 5.12 Privilege escalation

**Looked for:** whether a compromise of the streaming process yields SYSTEM.

**Found:** it does not. The supervisor runs as SYSTEM and does almost nothing:
it waits for an active console session and launches a child. The child — which
parses all network input, decodes all media, and runs the entire attack surface
— runs as the **logged-in user**, in `winsta0\default`, with that user's token
and environment. An attacker who achieves code execution in the agent gets the
user's privileges, not the machine's.

The supervisor's own inputs are the service control manager and the session
notification API, neither of which is network-reachable.

**Status:** OK, and this is the single most valuable structural decision in the
Windows design.

### 5.13 Malicious signalling

**Looked for:** what a compromised server can do beyond denial of service.

**Found:** it can refuse to introduce peers, and it can learn metadata. It
cannot forge or substitute SDP (Section 4.4), cannot authorise an unpaired
client (Section 4.5), cannot recover media keys (they are DTLS-derived), and
cannot pair itself (Section 4.3). Denial of service is real and unavoidable for
any rendezvous design; it is why the LAN path exists.

**Status:** OK — this was the primary design constraint, not an afterthought.

### 5.14 Malicious peer connections

**Looked for:** memory-safety and resource exhaustion from a paired-but-hostile
peer.

**Found:** Go's memory safety covers the protocol surface. Every fixed-size
message checks its length before indexing. Variable-length messages (clipboard,
cursor shape) are capped. Cursor PNGs are decoded by Go's standard `image/png`,
which is memory-safe and size-limited. The C++ layer never parses network data —
it only receives capture parameters that the Go layer has already validated
against the enumerated monitor list and a fixed range of encoder settings.

The one place hostile data reaches C++ is not present: audio and video flow
*outward* only. There is no decoder in the agent.

**Status:** OK.

### 5.15 Findings

One finding was fixed during this review.

**Unencrypted service addresses were accepted (fixed).** Both the browser client
and the agent would connect to a `ws://` address to any host. The end-to-end
signatures meant an attacker still could not substitute a DTLS fingerprint, so
sessions could not be hijacked — but confidentiality of the signalling was gone:
an observer on the path would see the full SDP (every ICE candidate, meaning
every address your devices know about) and the TURN credentials minted for the
session. Fixed by adding a single policy function per language,
`rvclient.ValidateEndpoint` and `AS.Util.serviceAddressProblem`, applied at
three points: when the address is typed in Settings, when the agent's
`config -service` flag is set (so the person configuring it sees the error, not
a service log), and again at connect time in both the client and the agent — the
last of these because an address can also arrive from a shipped `config.js` that
never passed through the settings screen. `ws://` remains available for
local-network hosts, for the reasons in
[Section 4.8](#48-transport-defends-a1). Both implementations are tested against
the same case list.

Two further places are called out for anyone auditing the code, because the
obvious implementation would have been wrong:

1. **Codec list truncation.** A client's advertised codec list is attacker-sized
   input that gets forwarded to the agent. It is truncated to 64 entries at the
   server before relaying (`hub.go`, `onConnect`), so a hostile client cannot
   inflate the agent's parsing work by advertising a huge list.
2. **Forget-device authority.** The `forget` handler ignores any device ID in
   the request body and uses the authenticated connection's own ID
   (`hub.go`, `onForgetDevice`). Taking it from the body — the natural way to
   write it — would let any paired client unpair any other.

No unresolved findings.

---

## 6. Windows privilege boundaries

What actually works, without pretending Windows security does not exist.

| PC state | Streaming works? | Why |
|---|---|---|
| Logged in, desktop visible | Yes | Normal case |
| Screen locked | Yes, you see the lock screen and can type the password | The secure desktop is capturable by a process in the user session; input reaches it |
| UAC prompt on screen | **No** — the screen freezes on the last frame | The secure desktop is isolated; nothing in the user session may capture or inject there. Approve at the PC, or configure UAC not to dim, which weakens a real protection |
| Logged out, at the sign-in screen | Screen: no. Wake and reconnect: yes | The supervisor is running, but there is no user session to launch into. It attaches automatically the moment someone signs in |
| Switched to another user | No | Capture is bound to the console session |
| Asleep | No, until woken | See [wake.md](wake.md) |

None of these are bugs. Each is a Windows security boundary doing its job, and
any product claiming otherwise is either installing a kernel driver or is
mistaken.

---

## 7. What to do if something goes wrong

| Situation | Action |
|---|---|
| A device was lost or stolen | On the PC: **ALL SHARE → Devices → Forget**. Effective immediately; no server involvement needed |
| You suspect the server is compromised | Nothing to do for confidentiality — it never had your keys. Point the agent and clients at a different server, or run your own ([deployment.md](deployment.md)) |
| You suspect the PC is compromised | Stop the service (`sc stop AllShareAgent`), then treat it as a full machine compromise. ALL SHARE cannot help here |
| A pairing code was seen by someone | It expires in three minutes and dies after one wrong attempt. If it was *used*, unpair the unexpected device |
| You want to revoke everything | Delete `C:\ProgramData\ALL SHARE\` and restart the service. A new identity is generated and every existing pairing is void |

---

## 8. Deliberate trade-offs

Each of these was a real decision with a real cost.

**No password on top of pairing.** A paired device connects without further
prompting. Adding a password would protect against a stolen unlocked device
(N2), but it would be typed on the same device that already holds the key — so a
thief who has one usually has the other — and it would be entered dozens of
times a day, which reliably trains people to choose weak ones. The screen lock
on the client device is the correct control at this layer. If you want a second
factor, the honest answer is to lock the Chromebook.

**Code-authenticated key exchange, not a PAKE.** Analysed in Section 5.7. Cost:
offline grinding is theoretically possible against a recorded exchange. Benefit:
no hand-rolled hash-to-curve in browser JavaScript, which is where this class of
implementation actually gets broken.

**The server sees metadata.** Any rendezvous design has this property, because
something must know that A wants to reach B. We minimise it — no accounts, no
email, no stored IP addresses (only an HMAC for LAN grouping) — but we do not
claim to eliminate it.

**TURN relays your encrypted traffic.** When a direct path cannot be
established, media goes through the relay. The relay sees ciphertext only; it
cannot decrypt SRTP. It does see volume and timing.

**Clipboard is opt-in, which means it is off when you first want it.** We chose
the mild annoyance over the silent-exfiltration risk.

**`unadjustedMovement: true` on pointer lock.** Bypasses OS mouse acceleration
so aiming feels correct. It is a documented Chromium capability behind a user
gesture and a permission prompt, not a sandbox escape.

---

## 9. Reporting a problem

Open an issue with a description of what you observed. Do not include private
keys, pairing codes, or full logs from the debug console without reading them
first — the debug console does not print secrets, but it does print device
fingerprints and network addresses you may not want public.
