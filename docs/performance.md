# Performance

How fast ALL SHARE is, why, and what to change when it is not fast enough.

---

## 1. What to expect

| Your connection | Glass-to-glass latency | What it feels like |
|---|---|---|
| Same home network, wired | 15–30 ms | Indistinguishable from sitting at the PC |
| Same home network, Wi-Fi 5/6 | 25–45 ms | Excellent. Fine for anything |
| Good broadband, same city | 35–60 ms | Very good. Comfortable for most games |
| Broadband, cross-country | 60–100 ms | Good for desktop work, noticeable in fast games |
| Mobile or relayed | 90–180 ms | Usable for desktop work, not for reflexes |

"Glass-to-glass" means key press to the pixel changing on your screen. It is the
number that matters and the one ALL SHARE measures — not the network round trip,
which is roughly a third of it.

**Bandwidth:** 3–8 Mbit/s for desktop work at 1080p, 10–25 Mbit/s for gaming at
1440p60. Idle desktop uses almost nothing, because unchanged frames are not
encoded at all.

**PC load:** 2–5% of one CPU core plus a few percent of the GPU's dedicated
encoder. The main GPU is untouched, so your game keeps its performance.

---

## 2. Where the milliseconds go

| Stage | Typical | Who controls it |
|---|---|---|
| Screen capture | 0.5–2 ms | Us — GPU-resident, no copy to system memory |
| Colour convert and scale | ~0.5 ms | Us — done by the GPU's video processor |
| Encode | 3–8 ms | Us — hardware encoder in low-latency mode |
| **Network** | **5–70 ms** | **Physics. Usually most of the total** |
| Jitter buffer | 0–30 ms | Us — adaptive, Section 5 |
| Decode | 2–8 ms | Browser's hardware decoder |
| Compositing | ~1 frame | Browser |

Two things follow from this table.

**The network dominates.** Everything ALL SHARE controls adds up to roughly
10–20 ms. If your total is 120 ms, about 100 of that is distance and routing,
and no setting will remove it. This is why Section 6 is mostly about the network
and not about quality sliders.

**Nothing is measured on assumption.** The agent tags every encoded frame with
the input sequence it reflects; the client matches that tag against
`requestVideoFrameCallback`, which fires when that exact frame reaches the
screen. The difference is a true end-to-end figure, shown in the performance
panel and asserted on by the test suite (`e2e_test.go` fails if the median over
loopback exceeds 140 ms).

That measurement paid for itself immediately: it exposed an eight-frame-deep
queue in the capture path adding a quarter of a second of pure latency for no
benefit. A queue is not a buffer — for live video, a queue is just latency with
a nicer name. The capture channel is one frame deep now.

---

## 3. The cursor trick

The mouse pointer is **not in the video**. It is removed at capture and sent
separately as a shape plus a position, and the client draws it locally at the
position the browser already knows.

The result: pointer motion is instant regardless of round-trip time. Move the
mouse and the cursor moves *now*, even on a 150 ms link — only what you click on
takes a round trip. This is the single largest improvement to how the product
feels and it costs a few hundred bytes per cursor shape change.

It also saves bandwidth. A moving cursor drawn into the video makes every frame
different, defeating the "nothing changed, skip the encode" optimisation
entirely.

---

## 4. Why an idle desktop costs nothing

Desktop Duplication reports `LastPresentTime == 0` when nothing has been drawn
since the last frame. ALL SHARE skips the encode entirely on those frames rather
than re-encoding an identical picture.

Sitting on a static desktop therefore uses near-zero CPU, near-zero GPU and
near-zero bandwidth. Bitrate rises only when the screen actually changes, which
is also why a video call over ALL SHARE costs more than a spreadsheet.

---

## 5. Adaptive quality, and how it avoids pumping

The hard part of adapting to a network is not lowering quality. It is lowering
it without oscillating — the "pumping" you see on products that react to every
dip, where the picture visibly cycles between sharp and soft.

ALL SHARE separates two kinds of change:

**Bitrate follows the estimate closely.** Changing bitrate is free, so it tracks
the Google Congestion Control estimate continuously.

**Resolution changes are gated.** Each one costs a keyframe and is plainly
visible, so a change requires all of:

| Gate | Value | Why |
|---|---|---|
| Dwell between changes | 4 s | A brief dip cannot trigger one |
| Sustained before dropping | 1.5 s | Going down is quick — too big for the link is actively painful |
| Sustained before climbing | 6 s | Going up is slow — too small is merely soft |
| Headroom before climbing | 1.35× | The step up must not immediately overshoot and step back down |

The ladder is deliberately coarse — 100%, 75%, 60%, 50%, 37.5%, 30% of native.
Small steps cost a keyframe for a change nobody can see.

This behaviour is covered by a test
(`TestQualityControllerDoesNotOscillate`) that flaps the estimate and asserts
the controller holds its rung, then collapses the estimate and asserts it
actually drops.

### The jitter buffer, and a mistake not made

The obvious move is to pin the receiver's jitter buffer to zero. It is also
wrong: a target of zero starves the decoder on ordinary network jitter and shows
up as stutter, worst at high resolutions. That trades a few milliseconds for a
visible fault.

ALL SHARE ships a small target with a controller behind it:

| Latency setting | Starting target | Use it when |
|---|---|---|
| Ultra low | ~0 ms | Wired LAN only. Will stutter on anything else |
| **Low (default)** | **25 ms** | **Almost always right** |
| Balanced | 60 ms | Wi-Fi that drops out, or a long link |
| Smooth | 120 ms | Mobile, or a genuinely bad connection |

If freezes accumulate, the target is raised in small steps — at most three
times — so a bad network degrades to *smooth* rather than to *broken*.

---

## 6. Making it faster

In order of how much they actually help.

### Use Ethernet on the PC

The biggest single improvement available to most people. Wi-Fi adds 5–20 ms of
variable latency, and the variance hurts more than the average: a jitter buffer
sized for the worst case penalises every frame.

### Check whether you are being relayed

Open the performance panel — the pulse button in the session toolbar, or
**Settings → Advanced → Show performance details** to have it open every time.
If the route says **relay**, your traffic is going through a server instead of
directly, typically adding 20–50 ms.

Usually this means UDP is blocked or both ends are behind carrier-grade NAT.
Sometimes it clears on a reconnect. If it is permanent, the network is the
cause.

### Pick the right preset

| Preset | What it does | For |
|---|---|---|
| **Gaming** | Higher frame rate, lower starting resolution. Drops *resolution* first under pressure | Anything where motion matters |
| **Desktop** | Native resolution, lower frame rate. Drops *frame rate* first under pressure | Reading, writing, coding |
| **Balanced** | Middle ground | The default |

The distinction matters: a soft image is fine when the picture is moving fast
and terrible when you are reading text. That is why the two presets drop
different things.

### Lower the picture size

Streaming 1440p when you are watching on a 1366×768 Chromebook screen wastes
bandwidth and encoder time for detail you cannot see. **Settings → Streaming → Picture
size** — match your screen, or leave it on Auto. The quality button in the
session toolbar changes it without leaving the session.

### Cap the frame rate

60 fps costs roughly twice the bandwidth of 30. For desktop work 30 is often
indistinguishable. For games it is not.

### Close the tab you left playing video

An idle desktop costs nothing; an animated one costs a lot. A background tab
playing video keeps every frame different and defeats the skip-unchanged path.

---

## 7. Reading the performance panel

The pulse button in the session toolbar, or **Settings → Advanced → Show
performance details** to keep it on.

| Row | Good | Concerning | What it means |
|---|---|---|---|
| Latency | < 60 ms | > 120 ms | Key press to pixels, measured |
| Round trip | < 40 ms | > 100 ms | Network only |
| Frame rate | Near your cap | Under 20 | Frames actually painted |
| Bitrate | 3–25 Mbit/s | Pinned at minimum | What is being sent |
| Packet loss | < 0.5% | > 2% | Loss is what triggers quality drops |
| Decode time | < 10 ms | > 20 ms | High means software decoding |
| Route | direct | relay | Whether traffic is relayed |
| Freezes | 0 | Rising | Usually jitter, not bandwidth |

**Decode time over 20 ms** is the one worth acting on: it means the browser is
decoding in software. Try switching the codec preference in Settings — on most
Chromebooks H.264 is hardware-decoded and VP9 or AV1 may not be.

---

## 8. Common problems

| Symptom | Likely cause | Fix |
|---|---|---|
| Smooth but blurry | Bandwidth-limited; quality dropped correctly | Check the bitrate row. If it is pinned low, the link is the limit |
| Sharp but stuttering | Jitter, not bandwidth | Raise the latency setting to Balanced |
| Feels laggy, latency reads low | Wi-Fi variance, or a queue between you and the router | Ethernet; check for a large download running |
| Text is fuzzy | Resolution scaled down | Desktop preset, or fix the picture size |
| Fine, then bad every few seconds | Something else on your network | Look for backups, updates, or cloud sync |
| High decode time | Software decoding | Change the codec preference (Section 7) |
| Audio out of sync | Audio buffered ahead of video | Reconnect; the buffer is re-established |
| Everything fine but input feels heavy | Mouse acceleration | Turn on Mouse Lock, which requests unadjusted movement |

---

## 9. Why these technologies

Short version; full reasoning with alternatives and rejections is in
[ARCHITECTURE.md](../ARCHITECTURE.md).

**WebRTC** rather than WebSockets or WebTransport, because it is the only
browser transport that performs ICE and can therefore get through NAT without
port forwarding. It also brings congestion control, packet loss recovery and
hardware decode paths for free.

**Hardware encoding** rather than software, because a hardware encoder costs
3–8 ms and a few percent of a dedicated engine, while x264 at the same quality
costs 20–40 ms and real CPU that your game wants.

**GPU-resident capture** — Desktop Duplication → D3D11 video processor →
encoder, entirely in video memory. Copying a 4K frame to system memory and back
costs 5–10 ms per frame and achieves nothing.

**Cursor sent separately** — Section 3.

**Opus at 20 ms frames** for audio: 10 ms halves the algorithmic delay but adds
real per-packet overhead; 40 ms is audible as lag against video already inside
30 ms.
