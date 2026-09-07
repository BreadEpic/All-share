# Waking a sleeping PC

This is the part of remote access that most products hand-wave, so this page is
specific about what works, what does not, and why.

**The short version:** a "wake my PC from anywhere" button that just sends a
magic packet over the internet does not work. ALL SHARE figures out which of
four real methods your PC can support, tells you which one you have, and tells
you how long it will take. If none of them apply, it says so instead of showing
you a button that does nothing.

---

## 1. Why the obvious approach fails

Wake-on-LAN works by sending a "magic packet" — 102 bytes containing the
network card's MAC address repeated sixteen times — to a machine whose network
card is still listening while the rest of the machine is off.

The catch is that a magic packet is a **layer-2 broadcast**. It is delivered by
MAC address on a local network segment, not by IP address across the internet.
Two things break when you try to send one from outside:

1. **Your router has no ARP entry for a sleeping machine.** ARP maps an IP
   address to a MAC address, and entries expire in minutes. A PC that has been
   asleep for an hour is not in the table, so the router does not know where to
   send the packet.

2. **Consumer routers refuse to forward to a broadcast address.** The workaround
   is to send to the subnet broadcast (`192.168.1.255`), which would reach every
   machine including the sleeping one. Virtually every consumer router drops
   these — it is a long-standing anti-amplification default, and it is the right
   default.

You can work around both with a static ARP entry and a router that allows
directed broadcast forwarding, which is a level of configuration that does not
belong in a consumer product and that most routers cannot do at all.

So ALL SHARE does not offer it. What it offers instead is four things that
genuinely work, ranked by how well.

---

## 2. The four methods

### Method 1 — Modern Standby (best; nothing to wake)

Most laptops made since about 2018, and some desktops, use **S0 low-power idle**
(Microsoft calls it Modern Standby) instead of the old S3 sleep state. On these
machines, sleep is more like a phone's screen-off: the network stays connected.

That means the ALL SHARE agent keeps its connection to the rendezvous alive
while the PC sleeps. There is nothing to wake — you connect, the machine
resumes, and you are in.

- **Time to connect:** effectively instant, about 5 seconds
- **Setup needed:** none
- **How ALL SHARE detects it:** `GetPwrCapabilities()`, checking the `AoAc`
  ("Always On, Always Connected") flag. If it is set, the machine never enters
  S3, so a wake timer would never fire and a magic packet is unnecessary.

If you have this, you have the best case, and the app will say
*"Stays reachable while asleep — waking is instant."*

### Method 2 — Another PC on the same network (reliable, needs two PCs)

If you have **two or more** PCs running ALL SHARE on the same home network, and
at least one is awake, that one can send the magic packet on the other's behalf.
It is on the local network, so the layer-2 problem simply does not exist.

- **Time to connect:** about 20 seconds — a few seconds for the packet, the rest
  for Windows to resume and the agent to reconnect
- **Setup needed:** wake-on-LAN enabled on the sleeping PC's network adapter
  (Section 3)
- **Router configuration needed:** none

The server groups devices onto the same network by comparing an HMAC of the
address it *observed* the connection coming from — never an address a device
claims. A PC cannot talk its way into a LAN group it is not actually on.

The app says *"Another PC on the same network can wake it, usually within
seconds."*

### Method 3 — Scheduled check-in (works everywhere, needs patience)

This is the one that needs nothing: no second machine, no router configuration,
no port forwarding, no static IP.

Before sleeping, the PC arms a **Windows wake timer**. On schedule it wakes
briefly, the agent connects to the rendezvous, asks whether anyone is waiting,
and if nobody is, it goes straight back to sleep. If someone *is* waiting, it
stays up (for three minutes by default) so you can connect.

- **Time to connect:** up to one check-in interval. At the default 15 minutes,
  the average wait is about 7 minutes and the worst case is 15
- **Setup needed:** none beyond enabling it
- **Battery cost:** a few seconds of wake per interval. Intervals below 5
  minutes are refused, because the machine would spend more time waking than
  sleeping; above 12 hours they are capped

The app shows the actual interval: *"Wakes itself every about 15 minutes to
check whether it is wanted."* It does not imply an instant wake, because it is
not one.

The default is 15 minutes, which is the compromise the setting was chosen for:
shorter costs battery for nothing, longer makes "Wake PC" feel broken. If you
know you will want the PC this evening, connecting once beforehand arms it.

### Method 4 — The service is on your network (self-hosted only)

If you run the rendezvous yourself on the same LAN as the PCs, it can broadcast
the magic packet directly. Same reliability as Method 2, without needing a
second PC.

Enable it with `-local-wol` on the server. It is opt-in because it only makes
sense in that specific deployment — see [deployment.md](deployment.md),
Section 9.

- **Time to connect:** about 20 seconds
- **Setup needed:** `-local-wol`, plus wake-on-LAN on the adapter

### When none apply

The app says why, specifically. For example:

> No network adapter on this PC is set to wake it. In Device Manager, open your
> network adapter's properties and turn on "Allow this device to wake the
> computer."

or

> Waking this PC remotely is turned off in ALL SHARE's settings.

The PC still appears in your list. It is shown as offline, without a wake
button, and with the reason underneath.

---

## 3. Turning on wake-on-LAN

Methods 2 and 4 need the network adapter armed. It is usually two settings.

**In Windows:**

1. Right-click Start → **Device Manager**
2. Expand **Network adapters**, right-click yours, choose **Properties**
3. **Power Management** tab: tick **Allow this device to wake the computer**
   and **Only allow a magic packet to wake the computer**
4. **Advanced** tab: set **Wake on Magic Packet** to *Enabled*

**In your PC's BIOS/UEFI** (usually only needed on desktops):

Look for *Wake on LAN*, *Power On By PCI-E*, *Resume by PCI-E Device* or
*Network Boot* and enable it. The name varies by manufacturer.

**Check it worked:**

```
allshare-agent status
```

It reports what ALL SHARE believes your PC can do, using `powercfg /devicequery
wake_armed` — which is what Windows itself thinks is armed, rather than a guess
based on registry keys.

### Two things that quietly break it

**Windows Fast Startup.** "Shut down" with Fast Startup on is really a
hibernate, and the network card is fully powered off. Wake-on-LAN cannot work
from that state. Either use Sleep rather than Shut down, or turn Fast Startup
off: Control Panel → Power Options → Choose what the power buttons do → Change
settings that are currently unavailable → untick **Turn on fast startup**.

**Wi-Fi.** Wake-on-Wireless-LAN exists but is unreliable and unsupported on most
consumer adapters. If your PC is on Wi-Fi, expect Methods 2 and 4 not to work
and use Method 1 or 3 instead. Ethernet is far more dependable here.

---

## 4. What happens after the wake

You do not have to do anything. The client:

1. Shows *"Waking your PC… this usually takes about 20 seconds"* with the real
   estimate for your method, not a generic spinner
2. Watches for the PC to come back online
3. Connects automatically the moment it does

A freshly woken PC is asked to stay awake for three minutes — long enough to
finish connecting, short enough that a stray wake does not keep the machine up
all night. Once you are connected, ALL SHARE holds sleep off for the duration of
the session and releases it when you disconnect.

If the PC does not come back, the client says so plainly and offers to try
again, rather than spinning forever.

---

## 5. Which one do I have?

Open ALL SHARE and look under the PC's name. The app tells you which method
applies and how long it takes. Or from the PC:

```
allshare-agent status
```

Rough guide:

| Your setup | Expect |
|---|---|
| Laptop from ~2018 or later | Method 1 — instant |
| Desktop, plus another PC on the same network | Method 2 — ~20 seconds |
| Desktop, wired, only PC in the house | Method 3 — up to your interval |
| Desktop on Wi-Fi, only PC in the house | Method 3 — up to your interval |
| Self-hosted service on the same network | Method 4 — ~20 seconds |

---

## 6. Settings

```
allshare-agent config -wake-check-in 15    # the default: every 15 minutes
allshare-agent config -wake-check-in 30    # less often, longer wait
allshare-agent config -wake-check-in 0     # turn check-ins off
```

The interval is clamped to between 5 minutes and 12 hours. Check-ins are
automatically disabled on Modern Standby machines, where they would be pure
waste — the machine is already reachable.

---

## 7. Things that will not be added, and why

**"Just send a magic packet from the browser."** Browsers cannot send raw UDP.
There is no API for this and there is not going to be one.

**"Send the magic packet from the server to the user's public IP."** Section 1.
It fails for the reasons given there, and worse, it fails *silently* — the
packet is accepted and discarded, so the product would appear to work until
somebody actually needed it.

**"Let the user enter a MAC address and broadcast address to wake."** This makes
the server into a packet reflector aimed at whatever address a user types. The
MAC addresses ALL SHARE wakes come from the target device's own registry entry,
written when that device authenticated — never from a request.

**Intel AMT / vPro.** Genuinely works and genuinely wakes a machine from
anywhere. It is business-class hardware, needs firmware provisioning, and has
had a bad security history. If you have it and know how to use it, it is
excellent; it is not something a consumer product should be steering people
into.
