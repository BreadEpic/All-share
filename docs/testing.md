# Testing

What is proved automatically, what has to be checked by hand on real hardware,
and — the part that matters most — what the automated suite **cannot** prove and
why.

```bash
go test ./...                             # 80 Go tests
node test/tools/client-unit.js            # 458 checks on the client's logic
node test/tools/protocol-conformance.js   # the JS encoder against Go, byte for byte
node test/tools/pairing-interop.js        # pairing and fingerprints against Go
ALLSHARE_E2E=1 go test ./test/e2e/        # real browser, real agent, real server
```

---

## 1. What the automated suite proves

### Identity and authentication — `internal/rendezvous/signal/auth_test.go`

| Test | What would break without it |
|---|---|
| Garbage signature rejected | Anyone could claim any device ID |
| Signature from a different key rejected | Same |
| Role substitution rejected | A client's signature could claim an agent's slot |
| Cross-server replay rejected | A signature captured by one server would work on another |
| Foreign nonce rejected | A recorded handshake could be replayed |
| Future protocol version rejected | Silent misinterpretation instead of a clear message |
| Unauthenticated connection cannot act | Every handler reachable before sign-in |
| Auth is rate limited | Unbounded signature verification on demand |

### Pairing — `shared/pair/pair_test.go`, `hub_test.go`

Succeeds with the right code; succeeds with the user's spacing; **fails on a
single-character typo**; fails with a swapped identity key (this is the
man-in-the-middle case); different sessions derive different secrets; confirm
tags differ by role; the generator uses the whole alphabet; confusable
characters fold; malformed peer keys are refused. End to end: the code is
single-use, and an unknown code is indistinguishable from a wrong one.

### Signalling — `hub_test.go`

A session is introduced only between paired devices; signed payloads relay
intact; **a tampered payload fails verification**; payloads for another session
are rejected; stale payloads are rejected; an outsider cannot inject into a
session; the device list leaks no network detail; the server derives the LAN key
itself rather than trusting what an agent claims; forgetting a device removes
only the caller.

### The wire protocol — `shared/protocol/wire_test.go` + `protocol-conformance.js`

Every message round-trips, including negative deltas. Short buffers are rejected
rather than read past. Oversize lengths are rejected. Decoders never panic on
random input (fuzzed). Normalised coordinates survive a resolution change.
Scancodes cover the common keys and flag the extended ones.

**And the JavaScript encoder is compared against Go byte for byte.** This is not
belt-and-braces: it caught a real bug, a mouse-move struct that JavaScript
thought was 13 bytes and Go thought was 14, with the button field one byte out.
Nothing else would have found it before a user did.

### Key state — `wire_test.go`

`TestKeyStateSnapshotHealsDroppedPacket` is the important one. Input rides an
unreliable channel; the test drops a key-up and asserts the next packet's
bitmap corrects the agent's view. Without this, a lost packet leaves a key held
down on the PC, which is the worst small bug this product could ship.

### Adaptive quality — `TestQualityControllerDoesNotOscillate`

Flaps the bandwidth estimate and asserts the controller holds its rung, then
collapses it and asserts the bitrate actually drops (6100 → 700 kbps). Pumping
quality is the failure mode users notice most and the one most easily
introduced by a "simplification".

### End to end, in a real browser — `test/e2e/`

A real rendezvous, a real agent with a real Pion WebRTC stack, and a real
Chromium loading `client/index.html` from a real `file://` URL. Nothing is
stubbed except the pixels: the capture backend replays a pre-encoded VP8 stream,
because CI has no desktop.

| Assertion | Recent output |
|---|---|
| The browser genuinely decodes video | 24 frames, 1 keyframe, 960×540 |
| A click lands where it was aimed | browser 25%/75% → agent (16384, 49151) of 65535 |
| Keys arrive as the right HID usages | 5 distinct keys including Shift and an arrow |
| Scroll arrives in notch units | dy=120 = one notch |
| **Pointer Lock is real** | `document.pointerLockElement` set; fullscreen and Keyboard Lock too |
| Relative movement arrives relative | 4 of 4, and **zero** absolute positions during the lock |
| Emergency release works | Ctrl+Alt+Shift+Q freed the pointer and every held key |
| Clipboard both ways, intact | 49 characters with non-ASCII and a tab, plus PC → browser |
| Oversized clipboard is refused, not truncated | 80 KB paste never reached the PC |
| Ending a session releases held input | the agent was told to let go |
| The local cursor is drawn | 1 shape received, 199 pixels painted |
| Ctrl+Alt+Delete reaches the agent | 3 privileged actions crossed, chosen from the real menu |
| A screen plugged in mid-session appears | the picker went from 1 to 2 with no reconnect |
| Forgetting a device takes effect at once | the running agent dropped it with no restart |
| Latency is measured, not assumed | median 49 ms over loopback (fails above 140) |
| A dropped session recovers on its own | reconnected with a new session ID |
| A session survives the rendezvous going away | still streaming while the service was down |
| Sessions are torn down completely | goroutines settled at 8 against a baseline of 7 after two sessions |
| An unpaired device is refused | by the agent, not only the server |

Plus 67 interface checks: the hidden developer mode (six taps do nothing, the
seventh works, a stale run does not accumulate), the service-address policy, all
six settings tabs opening in both colour schemes, every session menu measured
geometrically (a title and its description must stack, not run together — a
pure-CSS failure no DOM assertion would catch), settings surviving a reload, the
app surviving `localStorage` that throws, and an old browser being refused with
a reason.

---

## 2. What the automated suite cannot prove

This is the honest part. CI runs on Linux with no GPU, no desktop and no Windows.

| Not covered | Why | How it was verified instead |
|---|---|---|
| Desktop Duplication capture | Needs a Windows desktop and a GPU | By hand; the .exe is cross-built and its imports checked in CI |
| Hardware H.264 encoding | Needs a GPU encoder | By hand |
| WASAPI loopback audio | Needs a sound device | By hand |
| `SendInput` reaching a real application | Needs Windows | By hand, including DirectInput games |
| Wake from S3 | Needs a machine that sleeps | By hand |
| The Windows service in session 0 | Needs a real service host | By hand |
| Real-network latency and NAT traversal | Loopback is not a network | By hand |
| Hardware **decode** in the browser | Headless Chromium decodes VP8 in software | By hand on a Chromebook |
| `unadjustedMovement` | Headless Chromium refuses it and falls back | By hand |

The suite proves the *system* is correctly wired — protocol, crypto, signalling,
session lifecycle, input semantics, adaptation — and that the Windows half
compiles and links correctly. It does not prove the pixels. Anyone claiming
otherwise about a CI machine with no GPU is not being straight with you.

---

## 3. The manual acceptance run

Do this on the real hardware before calling a build good. Each step says what
"pass" means, so there is no room to talk yourself into a marginal result.

**1 — Install and first pairing.** Run the installer on a clean Windows PC.
Open `index.html` on the Chromebook, enter the address and the code.
*Pass:* paired in under two minutes with no configuration file edited and
nothing installed on the Chromebook.

**2 — First connection.** Click the PC.
*Pass:* the desktop appears within five seconds and is legible. Text in a
document is sharp, not smeared.

**3 — Mouse and keyboard.** Move, click, drag, right-click, scroll. Type a
sentence with capitals and punctuation. Press F5, Ctrl+C, Ctrl+V, the Windows
key.
*Pass:* everything lands where aimed and nothing repeats or sticks. Release
every key and confirm nothing is still held (open Notepad and watch).

**4 — Mouse Lock.** Turn it on. Move the mouse in a circle. Hold Escape to
leave.
*Pass:* the pointer is captured (it does not leave the picture), motion feels
1:1 with no acceleration, Escape-held exits, and Ctrl+Alt+Shift+Q releases
everything from any state.

**5 — Alt+Tab.** With **All Keys** on and in fullscreen, press Alt+Tab.
*Pass:* the PC switches windows; the Chromebook does not.

**6 — A game.** Launch something with raw mouse input.
*Pass:* aiming works, is not inverted or doubled, and does not drift. This is
the test that catches a scancode or relative-movement mistake nothing else will.

**7 — Sound.** Play music, then a video.
*Pass:* audio arrives, stays in sync with the picture, and the volume control
works.

**8 — Clipboard.** Copy on the PC, paste on the Chromebook. Then the reverse.
Then copy something over 64 KB.
*Pass:* text moves in both directions, the indicator shows each transfer, and
the oversized copy is refused with a message rather than silently shortened.

**9 — A bad network.** Start a large download, or move to the far end of the
Wi-Fi.
*Pass:* the picture softens and recovers. It does not freeze, and it does not
pump between sharp and soft every few seconds.

**10 — Interruption and recovery.** Pull the Chromebook's network for thirty
seconds, then restore it.
*Pass:* "Reconnecting…" appears, the session comes back on its own, and you are
not dumped to the menu.

**11 — Sleep and wake.** Let the PC sleep. Press **Wake PC**.
*Pass:* the wait matches what the app said it would be, and it connects
automatically when the PC comes back. If the PC cannot be woken, the app said so
before you tried.

**12 — Locked and logged out.** Lock the PC. Then sign out.
*Pass:* the lock screen is visible and you can sign in through it. Signed out,
the app reports the PC as unavailable and connects by itself once someone signs
in. Neither state hangs or lies.

**13 — Overnight.** Leave a session open for eight hours.
*Pass:* still connected, still responsive, and the agent's memory has not grown.
Check Task Manager before and after.

---

## 4. Before shipping a change

```bash
gofmt -l .                                # must be empty
go vet ./...
go test -race ./...
node test/tools/client-unit.js
node test/tools/protocol-conformance.js
node test/tools/pairing-interop.js
git diff --exit-code -- shared/pair/testdata/ shared/idkey/testdata/
ALLSHARE_E2E=1 go test ./test/e2e/
bash build/build-agent.sh                 # if anything Windows-side changed
```

CI runs all of this on every push ([`.github/workflows/ci.yml`](../.github/workflows/ci.yml)).

The vector diff is worth understanding rather than working around: those files
are regenerated by the test run, so a diff means the wire format, the pairing
exchange, or the fingerprint changed. Any of those needs the JavaScript side
changed to match, and the conformance tests are what prove you did.
