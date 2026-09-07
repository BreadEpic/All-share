# ALL SHARE

**Powered by MMC**

Control your Windows PC from your Chromebook — or any other device with a modern
browser — from anywhere, as if you were sitting in front of it.

No extension. No app. No Node.js. No local server. You open one file.

---

## What it is

ALL SHARE streams your PC's screen and sound to a browser and sends your
keyboard, mouse and clipboard back the other way. It uses the same technology
video calls use, so it gets through home routers without port forwarding, and
the connection is encrypted end to end between your two devices.

- **Fast.** 15–30 ms glass-to-glass on a home network. The mouse pointer is
  drawn locally, so it moves instantly no matter how far away you are.
- **Real remote control.** Proper mouse capture through Pointer Lock, with
  relative movement and OS acceleration bypassed. Full keyboard, modifiers and
  F-keys included. Not a fake cursor drawn over a screenshot.
- **Always there.** The PC runs a Windows service that starts at boot. Nobody
  has to be logged in and nobody has to launch anything.
- **Private.** Video, audio and input travel directly between your devices under
  DTLS-SRTP. The server that introduces them cannot read any of it, and is
  designed on the assumption that it is hostile.
- **Honest.** Where Windows or the browser makes something impossible, ALL SHARE
  says so in plain language instead of pretending.

## What it is not

It cannot show you a UAC prompt (Windows isolates the secure desktop from
everything, by design). It cannot stream when nobody is signed in to the PC,
though it stays reachable and connects the moment somebody does. It cannot make
your internet connection faster. Details, with what *does* work in each case,
are in [docs/security.md](docs/security.md), Section 6.

---

## Getting started

**On your Windows PC:** run `AllShareSetup.exe`. It asks for your service
address and a name for the PC, installs the service, and shows you a pairing
code.

**On your Chromebook:** unzip `allshare-client.zip` and double-click
`index.html`. Type the code. That is the whole setup.

Full walkthrough: **[docs/user-guide.md](docs/user-guide.md)**.

You also need one small server on the internet to introduce your devices. It
runs on the cheapest VPS there is, or on your own network if you only need
access from home: **[docs/deployment.md](docs/deployment.md)**.

---

## Documentation

| | |
|---|---|
| **[User guide](docs/user-guide.md)** | Setting up, connecting, mouse lock, keyboard, clipboard, waking, troubleshooting |
| **[Deployment](docs/deployment.md)** | Running the service: systemd, Caddy, nginx, every option, firewall, backup |
| **[Waking a sleeping PC](docs/wake.md)** | Why the obvious approach fails, and the four methods that work |
| **[Performance](docs/performance.md)** | Where the milliseconds go, and what to change when it is not fast enough |
| **[Security](docs/security.md)** | Threat model, cryptography, the full security review, deliberate trade-offs |
| **[Architecture](ARCHITECTURE.md)** | Every major decision: alternatives considered, why rejected, why chosen |
| **[Developer guide](docs/developer.md)** | Layout, building, testing, and the things that will bite you |
| **[Testing](docs/testing.md)** | What is proved automatically, what is not, and the manual acceptance run |

---

## How it works

```
  Chromebook                    Rendezvous                    Windows PC
  ──────────                    ──────────                    ──────────
  index.html                                                  Service (SYSTEM)
  from file://                  Introduces the two                   │
       │                        devices. Cannot read           launches into the
       │  ── signed SDP ──────▶  anything they say.  ◀────────  desktop session
       │                              │                              │
       │                              │                         Agent (user)
       │                                                             │
       └──────── WebRTC: video, audio, input, clipboard ─────────────┘
                    DTLS-SRTP, keyed by the two devices
```

The rendezvous relays signalling and nothing else. The SDP that passes through
it is signed end to end by the device identity keys, and it carries the DTLS
fingerprint — so a compromised server cannot substitute its own and put itself
in the middle. Media never touches it at all.

**On the PC:** Desktop Duplication captures the screen into video memory, a
D3D11 video processor converts and scales it there, and the GPU's hardware
encoder produces H.264 — the frame never leaves the GPU until it is compressed.
Unchanged frames are skipped entirely, so an idle desktop costs nothing.

**In the browser:** WebRTC decodes in hardware, the cursor is drawn separately
on a canvas so it moves without waiting for the network, and a small adaptive
jitter buffer keeps a bad network smooth rather than broken.

---

## Building

```bash
go build ./server/cmd/allshare-server    # the rendezvous, pure Go
bash build/build-agent.sh                # the Windows agent (needs mingw-w64)
bash build/build-client.sh               # zips the client; there is no build step
```

```bash
go test ./...                            # unit tests
node test/tools/client-unit.js           # client logic
node test/tools/protocol-conformance.js  # the JS encoder against Go, byte for byte
ALLSHARE_E2E=1 go test ./test/e2e/       # real browser, real agent, real server
```

The end-to-end suite starts a real rendezvous, a real agent with a real WebRTC
stack, and a real Chromium loading the real client from a real `file://` URL,
then types, clicks, scrolls and measures the actual latency. See
[docs/developer.md](docs/developer.md).

---

## Requirements

**PC:** Windows 10 1903 or later, 64-bit, with a GPU that has a hardware video
encoder — every Intel CPU with integrated graphics since about 2012, every
NVIDIA card since the 600 series, every AMD card since the HD 7000 series.

**Client:** Chrome or a Chromium-based browser (Edge, Brave, ChromeOS) from
**version 137 or later** — that is the release, from May 2025, where Ed25519
arrived in Web Crypto unflagged, and ALL SHARE uses it for device identity. A
Chromebook that updates itself is well past this; one that has not been updated
in a while needs *Settings → About ChromeOS → Check for updates*. If the browser
is too old, ALL SHARE says exactly that on the first screen rather than failing
later.

Firefox and Safari have Ed25519 and will connect, but Keyboard Lock — what makes
Alt+Tab and Ctrl+W reach the PC instead of the browser — is Chromium-only, so
some keys stay with the local browser there.

**Network:** about 5 Mbit/s for desktop work, 15–25 Mbit/s for gaming at high
frame rates.

---

## Credit

Built on [Pion WebRTC](https://github.com/pion/webrtc), [Opus](https://opus-codec.org/),
and the web platform's own standards. Everything here was written from public
documentation and specifications; no proprietary code was copied. The reasoning
behind every dependency choice is recorded in
[ARCHITECTURE.md](ARCHITECTURE.md).

No licence has been chosen yet.
