# The networking architecture

Why you do not have to run a server, what the hosted part actually does, and
what happens when a direct connection is impossible.

This document was written after auditing the existing implementation against the
question "does this actually work from anywhere, without the user becoming a
network administrator?" — and after comparing it with how established products
solve the same problem.

---

## 1. The finding

**The architecture was already right. The gap was that nobody was running the
infrastructure, and the user had to type its address by hand.**

That distinction matters, because the two problems have completely different
fixes. A wrong architecture needs rewriting. A missing operator needs a
deployment story. The audit in [Section 6](#6-audit-of-the-implementation)
walks through the evidence question by question.

What was fixed:

| Problem | Fix |
|---|---|
| No way to stand up the control plane without server knowledge | `render.yaml` — one-click deploy, free plan, no card, no domain, no certificate |
| The service address had to be typed into every client by hand | `allshare-agent client` writes a client that already contains it; the installer runs it automatically |
| The server ignored the port its host assigned it | `$PORT` is honoured, so a hosting platform can route to it |
| No container image | `Dockerfile`, static binary `FROM scratch`, non-root |
| Nobody had checked that a host with disposable storage was safe | It is, and there is now a test proving it |

---

## 2. Why there has to be a service at all

Your PC and your Chromebook both sit behind routers doing NAT. Neither has an
address the other can dial. Both can make outgoing connections; neither can
accept an incoming one.

So something with a fixed, reachable address has to introduce them. Every
product in this category does the same thing:

| Product | The introducer |
|---|---|
| Chrome Remote Desktop | Google's signalling service |
| RustDesk | `hbbs`, the ID/rendezvous server |
| TeamViewer / AnyDesk | The vendor's broker |
| Parsec | Parsec's coordination service |
| **ALL SHARE** | `allshare-server` |

The distinction the brief draws is the right one, and it is the whole design:

> A product can have a server without requiring the **user** to set one up.

The company runs it. The user never learns it exists. ALL SHARE is built that
way — the only thing missing was somebody to press deploy once, which is now one
click rather than an afternoon of system administration.

---

## 3. What the service does, and what it never touches

```
                    ALL SHARE SERVICE
              ┌──────────────────────────┐
              │  Device registry          │
              │  Online / offline         │
              │  Signalling relay         │
              │  Session introduction     │
              │  Wake coordination        │
              │  ICE server list          │
              └───────┬──────────────────┘
                      │  outbound WSS from both sides
          ┌───────────┴───────────┐
          │                       │
     Chromebook               Windows PC
     index.html               ALL SHARE Agent
          │                       │
          └────────  ICE  ────────┘
                     │
              direct possible?
                ╱          ╲
             YES            NO
              │              │
         DIRECT P2P    TURN RELAY
     video, audio, input, clipboard
        DTLS-SRTP, keys held only
          by your two devices
```

**The service carries:** who is online, and a handful of signalling messages to
introduce two devices. Kilobytes per session.

**The service never carries:** your screen, your sound, your keystrokes, your
mouse, your clipboard. Those go directly between your two devices, and even when
a relay is needed the relay sees ciphertext only.

This is not a nicety. A 1080p60 stream is 10–25 Mbit/s. Relaying every user's
session would cost more in bandwidth than the entire rest of the product, and it
would add a detour through a datacentre to every mouse movement. Direct-first is
what makes it both affordable and fast.

### Verified, not assumed

The server package contains no media types at all — no track, no RTP, no codec.
That is checkable:

```
$ grep -rn "TrackLocal|WriteRTP|rtp.Packet" internal/rendezvous/
(no matches)
```

The signalling relay treats its payload as opaque bytes it cannot read the
meaning of, and forwards it to the other party. It could not insert itself into
the media path even if it wanted to — see [Section 5](#5-what-a-hostile-service-can-and-cannot-do).

---

## 4. How a connection is actually made

1. **The agent connects outward.** On boot, the Windows service opens a WSS
   connection to the service and holds it open. Nothing is listening for
   incoming connections on your PC, so there is no port to forward and no
   firewall rule to add.

2. **It proves who it is.** The server sends a random challenge; the agent signs
   it with its Ed25519 private key. The signature covers the server's identity
   and the connecting role, so a captured signature cannot be replayed against a
   different server or used to claim a different role.

3. **It registers.** Name, capabilities, wake support, and the list of devices
   it is paired with. This is why your PC is found by identity rather than by
   address — an IP change is invisible, because nothing ever refers to one.

4. **The client asks for an introduction.** The server checks the pairing and
   creates a session. Both sides get an ICE server list.

5. **They negotiate directly.** SDP offer, answer and ICE candidates pass
   through the server, each one signed end to end. ICE then tries every path in
   parallel: local network first, then public addresses discovered via STUN,
   then a relay.

6. **The best working path wins.** Same-network connections stay on the LAN.
   Most internet connections go direct. The rest relay.

Nobody chooses between UDP and TCP, or direct and relay. ICE picks, continuously
— and if a better path appears mid-session it migrates.

### The three routes, in the diagnostics panel

| Route | What it means | Typical added latency |
|---|---|---|
| `host` | Same network — the packets never leave the building | 0 |
| `srflx` | Direct across the internet, discovered via STUN | 0 (this is the direct path) |
| `relay` | Bounced through a TURN server because nothing else worked | 20–50 ms |

Chrome Remote Desktop names the same three states, for the same reasons.

---

## 5. What a hostile service can and cannot do

The design assumes the service is compromised, because it is the one component
that must be exposed to the internet.

**It cannot read your session.** Media and input are DTLS-SRTP, keyed by a
handshake between your two devices. The keys never exist on the server.

**It cannot get in the middle.** The obvious attack is to substitute its own SDP
— SDP carries the DTLS fingerprint that decides who the session is encrypted to.
Every signalling payload is therefore signed end to end by the device identity
keys, and verified against the key pinned at pairing time. A rewritten SDP fails
verification and the session is abandoned.

**It cannot authorise a device.** Pairing is checked twice: once by the server,
and again by the agent against its own list. Only the second one counts. A
compromised server can skip its own check; it cannot skip the one running on
your PC.

**It cannot pair itself.** Pairing is a code-authenticated X25519 exchange with
mutual key confirmation. Without the code shown on your PC's screen, the
confirmation fails and both sides abandon it having stored nothing.

**What it can do:** refuse to introduce devices, and learn metadata — which
device asked for which, and when. That is inherent to any rendezvous design.
Denial of service is real and is why the LAN path exists.

Full threat model: [security.md](security.md).

---

## 6. Audit of the implementation

The seventeen questions, answered against the code.

**1. What works?** Identity, pairing, signalling, presence, session lifecycle,
direct P2P, relay fallback, hardware capture and encode, the input plane,
clipboard, audio, wake, adaptive quality, reconnection, and the `file://`
client. All covered by the test suite; the acceptance test drives a real browser
against a real agent.

**2. What failed?** Not the architecture — the operating model. No hosted
service existed, the address had to be typed by hand on every device, the server
ignored `$PORT`, and there was no container image. Every one of those is a
deployment problem rather than a design problem, and all four are now fixed.

**3. Where does signalling happen?** `internal/rendezvous/signal/hub.go`,
`onSignal`. The server relays opaque, end-to-end-signed payloads.

**4. Is there a server dependency?** Yes, and there has to be — see Section 2.
The dependency is on *a* service, not on the user configuring one.

**5. Is the server supposed to carry video?** No, and it cannot: the server
package contains no media types (Section 3).

**6. Is P2P attempted?** Yes, first and always. ICE gathers host, server-reflexive
and relay candidates together; relay only wins if nothing else works.

**7. Is STUN configured?** Yes — `turnsvc.DefaultSTUNServers`, advertised to both
peers on every connection.

**8. Is TURN configured?** Supported two ways: a built-in relay
(`-turn-listen` with `-turn-public-ip`), or an external one via
`-ice-servers`. Free hosting plans do not offer the UDP ports the built-in relay
needs, which is why `render.yaml` documents the external option.

**9. Are ICE candidates exchanged correctly?** Yes. `kind: "candidate"` payloads,
relayed both ways, with candidates arriving before the remote description held
in `_pendingCandidates` and applied after — the race that otherwise drops the
first few.

**10. Is authentication correct?** Ed25519 challenge–response, domain-separated,
bound to server identity and role, rate-limited before the signature check so
verification cannot be used as a workload amplifier.

**11. Does the agent maintain an outbound connection?** Yes —
`rvclient.Client.Run`, a reconnect loop with exponential backoff and jitter,
capped at 60 s.

**12. Does the client work from `file://`?** Yes, verified in a real Chromium
rather than assumed. Secure context, WebRTC, WebCrypto, Pointer Lock, Keyboard
Lock, `localStorage` and IndexedDB all work; ES modules, `fetch` of sibling
files and `@font-face` do not, which is why the client is classic scripts with
everything inlined.

**13. Are browser APIs failing?** One real limit: Ed25519 in WebCrypto needs
Chrome 137+. The client probes for it at startup and says so plainly instead of
failing later.

**14. Is the streaming pipeline inefficient?** No. Desktop Duplication → D3D11
video processor → Media Foundation hardware encoder, entirely in video memory,
never copied to system RAM. Unchanged frames are skipped outright
(`LastPresentTime == 0`), so an idle desktop costs almost nothing. There is no
screenshot loop and no JPEG anywhere.

**15. Is the input pipeline inefficient?** No. Fixed-size binary messages on an
unreliable, unordered data channel — reliability there would mean head-of-line
blocking, and a retransmitted mouse move from 200 ms ago is worse than none.
Every packet carries the full 256-bit held-key bitmap so a dropped key-up cannot
leave a key stuck.

**16. Does it recover after disconnects?** Yes, both layers independently, and a
session survives the service going away entirely because media does not pass
through it. Covered by `TestReconnectAndServiceOutage`.

**17. Is the PC still discoverable after its IP changes?** Yes. Devices are
identified by Ed25519 public key. No IP address is ever stored or referred to,
so an address change is invisible — the agent simply reconnects.

---

## 7. Architectures considered

**A — everything through the service.** Simplest to build and to reason about.
Rejected: 10–25 Mbit/s per session in relay costs, and a mandatory datacentre
detour on every mouse movement. Nobody in this category does it, for both
reasons.

**B — hosted signalling, direct P2P only.** Cheap and fast when it works.
Rejected as a complete design: roughly one connection in ten cannot be made
directly, and a remote desktop that fails one time in ten is not a product.
It is, however, exactly what ALL SHARE degrades to when no relay is configured —
which is why that case reports honestly rather than hanging.

**C — hosted signalling, direct P2P, relay fallback.** **Chosen.** Direct
whenever possible, relay only when necessary, and the user never knows which
happened. This is what Chrome Remote Desktop and RustDesk both do.

**D — mesh VPN underneath.** Genuinely excellent when available: the devices get
stable private addresses and NAT stops being a problem at all. Rejected as the
*primary* design because it requires installing software on the client, which a
locked-down Chromebook cannot always do — the exact case this product exists
for. It is supported as an option: addresses in `100.64.0.0/10` are accepted, so
if you do run one, ALL SHARE works across it with no public service at all.

---

## 8. What this does not solve

Stated plainly, because a product that hides its limits wastes people's time.

**A network that blocks the service outright.** If a device cannot reach the
service, it cannot be introduced to anything. Working around a filter that
belongs to someone else is out of scope by design.

**A relay is still needed for the last ~10%.** Free hosting plans do not offer
UDP ports, so a deployment on one is direct-only unless an external relay is
configured. Those connections fail honestly rather than hanging.

**Waking a sleeping PC over the internet.** A magic packet is a layer-2
broadcast and cannot be routed to a sleeping machine from outside. ALL SHARE
reports which of four real methods your hardware supports and how long each
takes, rather than offering a button that might do nothing — see
[wake.md](wake.md).

**UAC prompts, and a signed-out PC.** Windows isolates the secure desktop; no
application in the user's session can capture or inject there. Documented in
[security.md](security.md), Section 6.
