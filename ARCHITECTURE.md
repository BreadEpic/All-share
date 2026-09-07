# ALL SHARE — architecture and decisions

*Powered by MMC*

This document records what ALL SHARE is built from and, more usefully, what it
is **not** built from and why. Every section states the alternatives that were
considered, the reason each was rejected, and what the choice costs.

Where a number appears, it is either measured on this codebase or taken from a
named source. Where a number is an estimate, it says so.

---

## 1. The shape of the problem

One person wants to use their own Windows PC from a Chromebook, from anywhere,
and have it feel like sitting in front of it. The Chromebook has restrictive
policy left on it, so the client has to be a web page opened straight off disk —
no extension, no app, no local server.

Three constraints follow, and they drive everything else:

| Constraint | Consequence |
|---|---|
| The client is a `file://` page | Whatever the browser allows from an opaque origin is the entire budget |
| The PC is behind a home router | The transport must traverse NAT without the user configuring anything |
| It must feel local | The latency budget is a frame, not a second |

---

## 2. What a `file://` page can actually do

This was measured against Chromium 141, not assumed. The results shaped the
whole client.

| Capability | From `file://` | Consequence for ALL SHARE |
|---|---|---|
| `isSecureContext` | **true** | WebRTC, Web Crypto, Pointer Lock, Keyboard Lock, IndexedDB all available |
| `<script src="…">` classic | **works** | The client can be many readable files, not one bundle |
| `<script type="module">` | **blocked** (CORS, origin `null`) | No ES modules anywhere; modules are namespaced by hand |
| `new Worker('file.js')` | **SecurityError** | Workers, if ever needed, must come from a Blob URL |
| `fetch()` / `XHR` of a sibling file | **blocked** | Nothing may be loaded at runtime; no `.wasm`, no `.json`, no sprite sheet |
| `<link rel=stylesheet>` | **works** | Normal CSS files are fine |
| `<img src="sibling.png">` | works, but **taints canvas** | Icons are inline SVG, which is better anyway |
| `@font-face` | **blocked** (fonts are CORS-fetched) | System font stack only |
| `localStorage`, IndexedDB | **work** | Settings persist; the device key lives in IndexedDB |
| Ed25519, X25519, PBKDF2, HKDF | **all present**, Ed25519 from Chrome 137 | Modern pairing crypto with no vendored library; the client probes Ed25519 at startup and says so plainly if the browser is older |
| Pointer Lock / Keyboard Lock permission | **granted**, not prompted | Mouse Lock works without a permission dance |
| WebSocket to `ws://` and `wss://` | **works**, sends `Origin: null` | The rendezvous accepts `null`; safe, because nothing works without a signature |

The single most useful finding is the first one: because Chromium treats `file:`
as a potentially trustworthy origin, **a local HTML file is a secure context**
and loses almost nothing. The restrictions that remain are about *loading*, not
about *capability*, and they are all designed around rather than worked around.

---

## 3. Transport

### The decision

**WebRTC media tracks for video and audio; WebRTC data channels for input and
control; a rendezvous server for signalling only.**

### Alternatives considered

**Screenshots over HTTP or WebSocket (JPEG/PNG).** Rejected outright. A 1080p
JPEG at even middling quality is 150–400 KB; thirty of those a second is
40–100 Mbit/s for a picture that still looks worse than a 5 Mbit/s H.264 stream,
because every frame is coded independently. It also cannot use the GPU decoder,
so a Chromebook would burn its battery on `decodeImage` in JavaScript.

**WebTransport (HTTP/3) with WebCodecs.** Genuinely attractive on paper: QUIC
datagrams, no jitter buffer, full control. Rejected because **WebTransport
cannot traverse NAT.** It needs a reachable HTTP/3 server, which means either the
user forwards a port — explicitly ruled out — or every byte of video is relayed
through a server. That is worse on latency, worse on cost, and worse on privacy
than a direct connection, and it fails the "works from anywhere" requirement in
the exact cases where it matters.

**Raw WebSocket carrying H.264 for WebCodecs.** Same NAT problem, plus TCP.
Head-of-line blocking on a lossy link turns one lost packet into a visible
stall, which is precisely the failure mode a remote desktop must avoid.

**WebRTC data channel carrying H.264 into WebCodecs**, instead of a media track.
This is the closest competitor and was taken seriously. It removes the jitter
buffer entirely and gives frame-exact control. It was rejected because it means
re-implementing, in JavaScript, everything a media track already provides:
congestion control, retransmission, pacing, and frame assembly. Those are not
small; getting them wrong is how a stream becomes unwatchable at 2% loss. It
also gives up the browser's zero-copy path from decoder to compositor: a
`VideoFrame` painted to a canvas costs a copy and roughly a frame of latency
that a `<video>` element does not.

### Why the choice holds up

WebRTC is the only browser transport that performs ICE — the only one that can
get a direct path between a Chromebook on a café network and a PC behind a home
router with nothing configured. Everything else in the comparison is downstream
of that fact.

The precedent is strong: Stadia, GeForce NOW and Parsec's web client all stream
interactive video over WebRTC media tracks with data channels for input. That is
not an argument from popularity — it is evidence that the latency floor is low
enough for games, which is the hardest case here.

### Costs, stated plainly

* WebRTC brings a jitter buffer. Section 6 covers what is done about it.
* A relay is needed for the minority of networks where ICE fails. Section 5.
* Pion's stack is more code than a WebSocket would be. Accepted.

### Expected figures

| | Direct (same city) | Direct (cross-country) | Relayed |
|---|---|---|---|
| Network round trip | 5–25 ms | 30–70 ms | +10–40 ms over direct |
| Added by ALL SHARE | capture ≤2 ms + encode 3–8 ms + jitter buffer 0–30 ms + decode 2–8 ms | same | same |
| Bandwidth, desktop use | 0.3–3 Mbit/s | | |
| Bandwidth, 1080p60 gaming | 8–25 Mbit/s | | |

The capture and encode figures assume a hardware encoder, which is the normal
case; software encoding is several times slower and is treated as a fallback,
not a target.

---

## 4. Codec

### The decision

**H.264 by default, negotiated rather than assumed. HEVC where both ends
support it.**

### How the negotiation works

The client reports what `RTCRtpReceiver.getCapabilities()` actually says it can
decode. The agent intersects that with what its GPU can actually encode, and
picks the best match. Neither side guesses.

This is why the same client works on a Chromebook that has H.264 and on a
Chromium build that has none — the end-to-end test in this repository runs
against exactly such a build and negotiates VP8 instead, without a special case.

### Alternatives considered

**AV1.** Best compression by a wide margin and excellent screen-content tools.
Rejected as a default because hardware *encode* is confined to very recent GPUs
(Intel Arc, RTX 40-series, RDNA3) and hardware *decode* to Intel 11th-gen and
later. Software AV1 at 60 fps is not close to real time. Available as an
explicit choice for users whose hardware suits it.

**VP9.** Good compression and widely decodable. Rejected because hardware encode
on Windows is rare — NVIDIA has none at all — so on most PCs it would mean
software encoding a 1080p60 stream, which is exactly the CPU cost this design
exists to avoid.

**HEVC.** Roughly 25–40% better than H.264 at the same quality and hardware-
encoded on essentially every GPU sold since 2016. Not the default because
browser support for receiving HEVC over WebRTC is inconsistent, and a remote
desktop that fails to display is worse than one that uses slightly more
bandwidth. Offered as an advanced option, negotiated like everything else.

**VP8.** Universally decodable, which is why it backs the test suite. Poor
compression for screen content; not a serious default.

### Why H.264 wins here

It is the only codec with hardware *decode* on effectively every client this
product targets — including ARM Chromebooks through V4L2 and Intel/AMD ones
through VA-API — **and** hardware *encode* on effectively every Windows PC. For
a product whose whole point is not to burn the battery of a thin client, that
intersection is the argument.

### The text-clarity problem, and what is done about it

H.264 has a reputation for making remote text mushy. Three specific causes, each
addressed:

1. **Periodic keyframes.** A keyframe every few seconds spends a large share of
   the bitrate re-sending a screen that has not changed, and the picture visibly
   blurs and re-sharpens on a cycle. ALL SHARE sets an effectively infinite GOP
   and emits a keyframe only when the receiver asks for one, so a still screen
   keeps refining until it is effectively lossless.
2. **Rescaling.** Resampling is the largest single cause of soft text. The
   default is to stream the desktop pixel-for-pixel, and the "Fit my screen"
   option matches the stream to the client's viewport so the browser draws it
   one-to-one rather than scaling twice.
3. **Profile.** Constrained High is requested first, falling back to Main and
   then Baseline. CABAC and the 8×8 transform are worth roughly ten percent on
   screen content, which is visible directly as sharper glyph edges.

Chroma subsampling remains 4:2:0, which does soften coloured text on a coloured
background. 4:4:4 would fix it and is not an option: no browser decodes H.264
4:4:4 in WebRTC. This is a real, honest limitation rather than something to
paper over.

---

## 5. Reaching the PC from anywhere

### The decision

**A rendezvous server for signalling and presence, direct peer-to-peer for
media, and a TURN relay only when ICE fails.**

### Why not each alternative

**Pure peer-to-peer with no server.** There is no way for two devices to find
each other, and no way to exchange the keys that make the session secure.

**Everything through a relay.** Simple and always works, and wrong. It adds a
round trip through a third machine to every frame, costs real money in
bandwidth, and puts the user's screen on a server they may not control.

**A VPN — WireGuard, Tailscale.** Excellent, and genuinely the right answer for
some people. Rejected as the product's own mechanism because it needs software
on the Chromebook, which is exactly what the constraints forbid.

**Port forwarding.** Explicitly ruled out: the user is not to be asked about
NAT, ports or routers.

### What the rendezvous is trusted with

Almost nothing, and this is the important part.

Every session-establishing message is signed end to end with the peers' Ed25519
identity keys, and the signature covers the SDP — which contains the DTLS
fingerprint that keys the media. A rendezvous that rewrites an offer produces an
invalid signature and the session is refused. It can therefore **deny service,
but it cannot observe or inject.**

That property is what makes it reasonable to use a shared or hosted rendezvous
rather than demanding everyone run their own. It is enforced in code and covered
by tests that rewrite an SDP and assert the peer rejects it.

### Cost

A rendezvous for one household is a few kilobytes per session and idles at
essentially zero. The relay is the only part that can cost money, and only for
the minority of sessions that need it — roughly one in ten by the usual industry
figure. A single small VPS runs both comfortably.

---

## 6. The latency budget

### Where the milliseconds go

| Stage | Typical | What controls it |
|---|---|---|
| Capture | 0.5–2 ms | Desktop Duplication, GPU-resident |
| Colour convert and scale | ~0.5 ms | D3D11 video processor, GPU-resident |
| Encode | 3–8 ms | Hardware encoder in low-latency mode |
| Network | 5–70 ms | Physics |
| Jitter buffer | 0–30 ms | Adaptive, see below |
| Decode | 2–8 ms | Browser hardware decoder |
| Compositing | ~1 frame | Browser |

### The jitter buffer, and a mistake not made

The obvious move is to pin the receiver's jitter buffer to zero. It is also
wrong. Projects that tried it report that a target of zero starves the decoder
on ordinary network jitter and shows up as stutter, most visibly at high
resolutions — trading a few milliseconds of latency for a visible fault.

ALL SHARE instead offers a small target with a controller behind it. The default
is 25 ms; the "Ultra low" setting starts near zero, and if freezes actually
accumulate the target is raised in small steps, at most three times, so a
genuinely bad network degrades to smooth rather than to broken.

### Measuring rather than assuming

The agent tags every encoded frame with the input sequence it reflects. The
client matches that tag against `requestVideoFrameCallback`, which tells it when
that exact frame reached the screen. The difference is a true end-to-end
"key press to pixels" figure, shown in the performance panel and asserted on in
the test suite.

That measurement immediately earned its keep: it exposed an eight-deep frame
queue in the capture path that was adding a quarter of a second of pure latency
for no benefit. The queue is now one deep, and the test fails if a median over
loopback exceeds four frame intervals.

### The cursor

The pointer is excluded from the video and sent separately as a shape and a
position. The client draws it locally, at the position the browser already
knows. Pointer motion is therefore **instant regardless of round-trip time** —
the single largest improvement to how the product feels, and it costs almost
nothing.

---

## 7. Capture and encode on Windows

### The decision

**DXGI Desktop Duplication → D3D11 video processor → Media Foundation hardware
encoder, entirely in video memory.**

### Capture: why Desktop Duplication

**Windows Graphics Capture** is newer, works across GPUs without the display
being attached to the capturing adapter, and is the right choice for capturing a
single window. It was rejected as the primary path for one reason: it does not
tell you whether the desktop image actually changed.

Desktop Duplication does. `LastPresentTime` is zero when only the pointer moved,
and that single fact is the largest saving in the product. On a desktop most
frames are identical; skipping them costs nothing, and every bit not spent
re-sending an unchanged screen goes to the pixels that did change. It is also
why still text converges to sharp instead of being re-encoded forever.

**GDI `BitBlt`** was not seriously considered: it is a CPU-side copy of the
entire framebuffer, misses hardware-accelerated content, and is several times
slower.

Desktop Duplication's costs are real and handled: it loses access whenever the
desktop changes — a resolution change, a UAC prompt, a lock, a fast user switch
— and the agent recreates it, which is routine rather than exceptional.

### Encode: why Media Foundation rather than vendor SDKs

**NVENC, AMF and oneVPL directly** give slightly more control: finer rate-control
knobs, explicit reference management, marginally lower latency. Rejected because
each needs its own SDK, its own code path and its own bugs — three partly-tested
paths instead of one well-tuned one. For a product that must run on whatever GPU
the user happens to own, the vendor-supplied Media Foundation Transform wraps
the same silicon behind one interface that works on NVIDIA, Intel and AMD alike.

The specific settings matter more than the API:

* `AVLowLatencyMode` — no frame reordering, one input to one output.
* Zero B-frames — a B-frame makes the decoder wait for a later picture before it
  can show an earlier one, which is pure latency here.
* CBR rate control — the congestion controller hands down a target and expects
  the encoder to hit it.
* Infinite GOP with keyframes on request — see section 4.
* A quantiser ceiling per preset, so a burst of motion cannot throw away all
  detail at the moment a user is most likely to be reading something.

**Software encoding** (x264, OpenH264) is the fallback when no hardware encoder
exists. It works, and it is honestly labelled in the client's status panel,
because 1080p60 in software is not a gaming-grade experience.

### Zero copy

The captured texture never leaves video memory: Desktop Duplication produces a
BGRA texture, the D3D11 video processor converts and scales it to NV12 in place,
and the encoder takes that texture directly through `MFT_MESSAGE_SET_D3D_MANAGER`.
A system-memory round trip would cost roughly 8 MB per 1080p frame each way — a
gigabyte a second at 60 fps — for no benefit.

---

## 8. Input

### The decision

**USB HID usages on the wire, PS/2 scancodes at injection, full state in every
packet, on an unreliable unordered channel.**

### Why scancodes, not virtual keys

DirectInput and Raw Input read scancodes. A game driven with virtual keys sees
nothing. Scancodes also let Windows apply its own keyboard layout, so the remote
machine behaves exactly as it would under the user's own hands.

The cost is that a user whose Chromebook layout differs from their PC's will get
their PC's layout. That is arguably correct — it is what sitting at the machine
would do — and a "match the characters I type" mode using Unicode injection is
offered for people who want the other behaviour. It cannot drive games, which is
why it is not the default.

### Why the input channel is unreliable

A retransmitted mouse position is stale by definition: by the time it arrives, a
newer position exists. Reliability on this channel would convert packet loss
into growing latency, which is worse than the loss.

The obvious objection is stuck keys, and it is handled structurally rather than
by cleanup: **every key packet carries the complete 256-bit held-key bitmap**,
and the agent applies it as an authoritative snapshot. No sequence of drops,
reorders or focus changes can leave a key down. On top of that, an all-clear
snapshot is sent on blur, on tab hide, on Pointer Lock exit and on session end,
and a low-rate heartbeat repeats the state while anything is held.

Thirty-two bytes per key event is the price. At realistic typing rates that is
noise.

### Mouse Lock

Mouse Lock is Pointer Lock **plus** Keyboard Lock **plus** fullscreen, requested
together. That combination is not decoration:

* Without fullscreen there is no Keyboard Lock.
* Without Keyboard Lock, Chrome exits Pointer Lock the instant Escape is
  pressed, so Escape can never reach the remote machine. With it, Chrome
  switches to press-and-hold, and a tap of Escape is delivered.
* `unadjustedMovement: true` bypasses the OS pointer-acceleration curve, which
  is what makes aiming in a game feel right rather than floaty.

Each step degrades on its own, and the client tells the user what it actually
got. There is always a way out: hold Escape, or Ctrl+Alt+Shift+Q, or the
toolbar.

Relative motion is injected with `MOUSEEVENTF_MOVE` rather than by warping the
cursor, because warping defeats Raw Input entirely.

---

## 9. Waking a sleeping PC

This is the part of remote access most often hand-waved, so it is stated
bluntly.

**A Wake-on-LAN magic packet is a layer-2 broadcast and cannot be routed to a
sleeping machine from the internet.** The home router has no ARP entry for a
powered-down host, and essentially no consumer router will forward a packet to a
subnet broadcast address. Any design that claims otherwise works on a LAN and
fails silently everywhere else.

ALL SHARE therefore reports what a given PC can genuinely support, and the client
shows that rather than a button that might do nothing:

| Method | How it works | Speed | Needs |
|---|---|---|---|
| **Modern standby** | The PC keeps its network connection through sleep, so the agent is still reachable | Instant | A machine using S0 low-power idle |
| **LAN peer** | Another ALL SHARE PC on the same network sends the magic packet | ~20 s | A second machine that stays on |
| **Scheduled check-in** | The PC arms a Windows wake timer, wakes briefly on a schedule, asks whether anyone wants it, and sleeps again if not | Up to one interval, 15 min by default | Nothing at all |
| **None** | | | The client explains why, in plain language |

The check-in path is the interesting one: it needs no second machine, no router
configuration and no port forwarding. The cost is the wait, and the client shows
the real interval rather than implying an instant wake.

The server never trusts an agent's claim about which network it is on. LAN
grouping is derived from the address the server actually observes, so membership
can be observed but not asserted.

---

## 10. The Windows service

Windows isolates services in session 0, where there is no desktop to capture and
no input queue to inject into. Anything that touches the screen must run inside
the interactive session, on whichever desktop currently has input.

**The decision: the service is a supervisor.** It keeps exactly one agent process
running in the active session and replaces it when Windows switches desktops —
at a lock, an unlock, a UAC prompt or a fast user switch.

**The alternative** — service owns the network, helper owns the desktop, encoded
frames crossing a named pipe between them — keeps a session alive across a
desktop switch. It also doubles the moving parts, adds an IPC protocol carrying
megabits per second, and puts a second failure mode between the user and their
PC.

The supervisor costs a one-to-two second reconnect when the desktop changes. The
client already handles that, because reconnection is a first-class path that the
end-to-end suite exercises by dropping a live session and asserting recovery.
For a product whose second priority is reliability, fewer parts that are already
exercised beats more parts that are not.

### What is actually possible, by state

| Windows state | Screen | Input | Notes |
|---|---|---|---|
| Signed in, unlocked | Yes | Yes | The normal case |
| Locked | Yes, after the helper is relaunched onto the Winlogon desktop | Yes | The session reconnects automatically |
| UAC prompt (secure desktop) | Yes, after relaunch | Yes | Ctrl+Alt+Delete needs `SendSAS`, which `SendInput` can never produce, by Windows' design |
| Signed out, sign-in screen | Yes, after relaunch | Yes | |
| Asleep | No | No | See section 9 |
| Powered off | No | No | Nothing software can do |

---

## 11. Security

The full threat model is in [`docs/security.md`](docs/security.md). In summary:

* **Identity.** Every device owns an Ed25519 keypair. The public key *is* the
  device ID. There are no accounts and no passwords.
* **Pairing.** A twelve-character code (60 bits) is the password input to an
  authenticated X25519 exchange with mutual key confirmation — not a bearer
  token. Behind a 210,000-iteration PBKDF2, single-use, three-minute window. The
  transcript binds both identity keys, so a hostile rendezvous cannot substitute
  one.
* **Session.** Media is DTLS-SRTP. The fingerprint is inside an SDP that is
  signed end to end and verified against the key pinned at pairing.
* **Authorisation.** The agent checks its own pairing list; it never trusts the
  server's routing. A compromised rendezvous cannot introduce a new client to a
  PC.
* **Client key storage.** Generated non-extractable and kept in IndexedDB as a
  live `CryptoKey`, so the private key never exists as bytes in JavaScript.

### Why not a PAKE

A true PAKE — SPAKE2, CPace — removes the offline-attack term entirely, and is
strictly better in the abstract. It needs elliptic-curve point arithmetic that
WebCrypto does not expose, so the browser would have to ship hand-rolled or
vendored curve code in the most security-critical path in the product.

With a 60-bit code, a 210,000-iteration KDF and a single-use three-minute
window, the offline term is already far out of reach: a GPU managing 50,000
PBKDF2 evaluations a second would need on the order of 10^13 seconds. Not
shipping bespoke curve arithmetic is the better trade here, and this is recorded
as a deliberate decision rather than an oversight.

---

## 11b. Audio

**WASAPI loopback on the default playback device, encoded as Opus.**

Loopback taps the mix the Windows audio engine is about to send to the speakers,
so it captures every application without any of them cooperating. That is what
"hear what the PC is playing" has to mean.

Opus is the only real choice. It is the one audio codec every browser decodes
over WebRTC, it is designed for interactive latency rather than broadcast, and
at 128 kbit/s stereo it is transparent for the music and system sounds this
carries. The alternatives:

* **G.722** avoids the dependency entirely and sounds like a telephone — 16 kHz
  mono is not acceptable for anything but speech.
* **Raw PCM over a data channel** costs 1.5 Mbit/s to carry what Opus does in a
  twelfth of that, and gives up WebRTC's audio-video synchronisation.
* **AAC through Media Foundation** would need no new dependency, but its frame
  size adds latency and WebRTC does not carry it.

Settings chosen for this workload:

* 20 ms frames — 10 ms halves the algorithmic delay but adds real per-packet
  overhead; 40 ms is audible as lag against video already inside 30 ms.
* `OPUS_APPLICATION_AUDIO`, not `VOIP` — this carries music, and the
  speech-tuned mode audibly damages it.
* In-band forward error correction with an assumed 5% loss. A dropped video
  frame is a flicker; a dropped audio packet is an audible click, so audio gets
  redundancy that video does not.
* Complexity 5, the knee of the curve: near-maximum quality for roughly half the
  CPU of complexity 10, which matters when a game is already using the machine.

libopus is fetched and cross-compiled once by the build script and linked
statically, so the shipped executable still depends on nothing but Windows
system DLLs. If it cannot be built, the library is produced without audio and
the agent reports sound as unavailable rather than sending silence.

---

## 12. Technology choices

| Component | Choice | Why, and what was rejected |
|---|---|---|
| Client | Plain JavaScript, no build step | ES modules are blocked from `file://`; a bundler would add a build step to something whose entire premise is "double-click index.html". TypeScript was rejected for the same reason |
| Client crypto | WebCrypto only | Ed25519, X25519, PBKDF2 and HKDF are all present (Ed25519 unflagged from Chrome 137, May 2025). A vendored library would be more code in the most sensitive path |
| Agent transport | Go with Pion | Pure Go WebRTC with a real Google Congestion Control implementation. libwebrtc would mean a C++ build measured in hours; SIPSorcery and webrtc-rs are less proven |
| Agent capture | C++ through cgo | Direct3D and Media Foundation are C++ APIs. Everything above the capture boundary stays in Go |
| Server | Go, single static binary | No runtime, no dependencies, cross-compiles anywhere. Node would need a runtime on the box; Rust would be fine but buys nothing here |
| Installer | Inno Setup | WiX produces an MSI, right for fleet deployment and overkill for one PC. NSIS needs every step micromanaged, which is where installer bugs become uncleanable machines |

---

## 13. What this design does not do

Stated so nobody has to discover it the hard way.

* **No 4:4:4 chroma.** No browser decodes it over WebRTC. Coloured text on a
  coloured background will be softer than local.
* **No file transfer.** Out of scope; the clipboard carries text only, and only
  when the user copies or pastes themselves.
* **No multi-user control.** One controller at a time, by design. Simultaneous
  control is a source of confusion, not a feature.
* **A locked or switching desktop drops the session briefly.** Section 10.
* **Ctrl+Alt+Delete needs the service.** Windows reserves the sequence so no
  program can fake a sign-in screen; that is a feature, not a bug.
* **Audio is captured from the default playback device only.** Per-application
  capture is possible on Windows 10 2004 and later but adds real complexity for
  a case most users do not have.
* **Audio resampling is cubic, not windowed-sinc.** It only runs when the
  Windows mix rate is not 48 kHz, and for desktop audio the difference is not
  audible. A full polyphase filter would be more code in a path most machines
  never take.
