# ALL SHARE — using it

*Powered by MMC*

ALL SHARE lets you use your Windows PC from another device, over the internet,
as if you were sitting in front of it.

This guide is for using it. If you are setting up the service that connects your
devices, see [deployment.md](deployment.md).

---

## What you need

* A Windows 10 or 11 PC (the one you want to reach).
* Another device with an up-to-date Chrome or Edge browser — a Chromebook,
  laptop, tablet, anything.
* The address of an ALL SHARE service. Someone may have given you one; if not,
  [deployment.md](deployment.md) explains how to run your own in a few minutes.

You do **not** need to change any router settings, forward any ports, or know
what an IP address is.

---

## Setting up your PC

1. Download `AllShareSetup.exe` and run it.
2. Say yes when Windows asks for permission. ALL SHARE needs it to see your
   screen and to start automatically.
3. When the installer asks for the **ALL SHARE service address**, paste in the
   address you were given. It looks like `wss://allshare.example.com/rv`.
4. Give your PC a name you will recognise — "My Gaming PC", "Office".
5. Finish. ALL SHARE is now running, and it will start again by itself every
   time your PC starts.

Nothing else on your PC changes. There is no window in the way and no icon you
have to keep open.

---

## Setting up the device you will connect from

1. Download the ALL SHARE client and extract it anywhere — your Downloads folder
   is fine.
2. Open the folder and double-click **index.html**.

That is the whole installation. There is nothing to install, no extension, and
no app.

The first time you open it, ALL SHARE asks for the same service address your PC
uses. Your PC shows it on its **Add a device** screen, just above the code.

> **Tip:** bookmark the page, or drag `index.html` onto your bookmarks bar, so
> you do not have to find the folder again.

---

## Pairing the two

Pairing tells your PC that this device is allowed in. You do it once.

**On your PC:** open ALL SHARE from the Start Menu (or run
`allshare-agent pair`). It shows a code like:

```
  K7M2 - Q9XR - 4TVZ
```

**On your other device:** click **Add a PC** and type the code.

After a second or two the two are paired, and your PC appears in the list. You
will not need the code again.

The code expires after three minutes and works only once. If it stops working,
ask your PC for a new one.

---

## Connecting

Click **Connect**. Within a few seconds your desktop appears.

Along the bottom is a small toolbar that fades away when you stop moving the
mouse and comes back when you move it again.

| Button | What it does |
|---|---|
| **Mouse Lock** | Captures your mouse for games and 3D applications |
| **All Keys** | Sends every key, including Alt+Tab and the Windows key |
| **Clipboard** | Shares copied text between the two devices |
| **Send to the PC** | Ctrl+Alt+Delete, lock the PC, wake its screen |
| **Sound** | Turns your PC's sound on or off |
| **Balanced** | Switches between Gaming, Balanced and Desktop |
| **Displays** | Chooses which monitor to see (if your PC has several) |
| **Performance** | Shows live speed and quality details |
| **Fullscreen** | Fills the screen |
| **Disconnect** | Ends the session |

---

## Mouse Lock, for games

Normally your mouse behaves like a mouse: point at something on the remote
desktop and click it.

Games are different. They need the mouse to be *captured*, so that moving it
turns your character rather than sliding a pointer to the edge of the window.
That is what **Mouse Lock** does.

Press it, and:

* Your pointer disappears and your mouse controls the PC directly.
* Mouse movement is sent raw, without your Chromebook's own smoothing, so aiming
  feels the way it does on the PC itself.
* Keys that the browser would normally take — Alt+Tab, the Windows key, Escape —
  go to your PC instead.

**To get out again:** hold **Esc** for about a second.

If that ever fails, **Ctrl + Alt + Shift + Q** releases everything immediately,
including any keys that are still held down.

Mouse Lock works best in fullscreen, and turning it on takes you there
automatically.

---

## The keyboard

Almost everything works exactly as it would on the PC: letters, numbers,
symbols, function keys, arrows, Home, End, Page Up and Down, Insert, Delete,
Caps Lock, and combinations such as Ctrl+C and Ctrl+Shift+Esc.

A few things are worth knowing:

**Alt+Tab and the Windows key** need **All Keys** turned on *and* fullscreen.
Without both, your own device keeps them.

**Ctrl+Alt+Delete** is special. Windows deliberately reserves it so that no
program can imitate a sign-in screen — nothing ALL SHARE could type would ever
produce it. Use the **keyboard button** in the toolbar instead, which asks the
Windows service to do it. The same menu can lock the PC and wake its screen if
it has gone black.

**On a Chromebook,** the top row (back, refresh, brightness, volume) is sent as
F1 to F12, because that is what Windows programs expect. You can turn that off in
Settings if you would rather keep the media keys.

**Different keyboard layouts.** By default ALL SHARE sends the *physical* key you
pressed, and your PC applies its own layout — exactly what would happen if you
were typing on the PC. If your two devices have different layouts and you would
rather the characters match, turn on **Match the characters I type** in Settings.
(That mode cannot drive games, which is why it is not the default.)

---

## More than one monitor

If your PC has several monitors, a **Displays** button appears in the toolbar.
It lists each one with its size, and switching is one click.

You see one monitor at a time, at full quality, rather than all of them squashed
side by side into a picture where nothing is readable. Your mouse is mapped to
whichever monitor you are watching, so clicking where you are looking clicks
where you meant — which is the part that usually goes wrong.

The button is hidden entirely when your PC has one monitor. If you plug in or
unplug a screen during a session, the list updates on its own.

---

## The clipboard

Copy on one device, paste on the other. It just works, in both directions, for
text.

Nothing is copied in the background: text only moves when *you* copy or paste.
You can turn either direction off in Settings.

Only text is shared. Files and images are not.

There is a size limit of 64 KB — roughly thirty pages of writing. Anything
larger is not sent at all, and ALL SHARE tells you so. It does not send half of
it, because a clipboard that pastes most of what you copied is worse than one
that says it could not.

---

## Removing a device

If you lose a device, or just want to stop using one, remove it **from the PC**:

```
allshare-agent status                     shows which devices are paired
allshare-agent forget "Chromebook"        removes that one
allshare-agent forget -all                removes all of them
```

It takes effect straight away — nothing to restart, and the removed device
cannot connect again until you pair it afresh.

This lives on the PC on purpose. If the device is lost, you no longer have it,
so being able to remove it only *from* that device would be no use at all. You
can also remove a PC from a device's own list in **Settings → General**, but
that only tidies up that device's list; the PC is where the decision is made.

---

## Waking your PC

If your PC is asleep, its card shows **Offline** and a **Wake PC** button.

How quickly it wakes depends on your PC, and ALL SHARE tells you honestly which
applies:

* **Instantly** — some PCs (most laptops and newer desktops) stay connected to
  the network while asleep. Nothing needs waking; they simply resume.
* **In a few seconds** — if you have another ALL SHARE PC on the same home
  network, it sends the wake signal for you.
* **Within about 15 minutes** — your PC wakes itself briefly on a schedule to
  check whether anyone wants it. This needs nothing at all: no second PC, no
  router settings. The card tells you the interval.
* **Not possible** — the card explains why and what you could change.

Whichever applies, once your PC comes online ALL SHARE connects automatically.
You do not have to press anything else.

To make waking faster, you can shorten the check-in interval on the PC:

```
allshare-agent config -wake-check-in 5
```

Shorter means the PC wakes more often for nothing, which uses a little more
power. Fifteen minutes is a reasonable balance.

> **If waking never works at all:** your network adapter probably is not set to
> wake the PC. In Device Manager, open your network adapter's properties, go to
> **Power Management**, and turn on **Allow this device to wake the computer**.

---

## Getting the best picture and speed

**Quality** in the toolbar has three modes:

* **Gaming** — the lowest possible delay and the highest frame rate. Use it for
  anything that moves.
* **Balanced** — a good default.
* **Desktop** — the sharpest text. Fewer frames when nothing is moving, and more
  detail in the ones that are. Use it for reading and writing.

**Picture size** matters more than people expect. **Original** is the sharpest,
because nothing is resized anywhere in the chain. **Fit my screen** is the next
best, because your browser then draws the picture without scaling it.

ALL SHARE adjusts itself as your connection changes: it lowers quality when the
network struggles and raises it again when it recovers, deliberately slowly so
the picture does not pulse.

To see what is actually happening, press the **Performance** button. It shows
frame rate, end-to-end delay, data rate, packet loss and how long decoding takes.

---

## When something goes wrong

### "That code did not work"

Codes last three minutes and can be used once. Ask your PC for a new one. Check
you typed it correctly — ALL SHARE never uses the letters I, L, O or U, so if you
think you see one, it is a 1, a 1, a 0 or a V.

### "That PC is offline"

It is asleep or switched off. Try **Wake PC**. If there is no Wake button, see
the waking section above.

### "We couldn't reach your PC"

Both networks are refusing a direct connection and no relay was available. This
is usually a restrictive Wi-Fi network. Trying a different network — a phone
hotspot, for instance — nearly always works, and tells you it was the network.

If it happens on every network, the ALL SHARE service probably has no relay
configured. See [deployment.md](deployment.md).

### "We could not verify that PC"

ALL SHARE refused to connect because the PC did not prove it is the one you
paired with.

If you reinstalled ALL SHARE on that PC, that is expected — its identity changed.
Remove it from the list and pair again.

If you did **not** reinstall anything, do not connect. Something is interfering
with the connection.

### The picture is blurry

Set **Picture size** to **Original**, and **Quality** to **Desktop**. If it is
still soft, your connection may not have the bandwidth for your screen's
resolution — the Performance panel shows the data rate it is actually achieving.

### A key seems stuck

Press **Ctrl + Alt + Shift + Q**. That releases everything on the PC
immediately.

(ALL SHARE is built so this should not happen: every key message carries the
complete list of keys you are holding, so the PC can always correct itself. If
you do hit it, it is worth reporting.)

### Sound is not working

Check the **Sound** button is on, and that sound is enabled in Settings. ALL
SHARE plays whatever your PC is playing, so if the PC itself is muted there is
nothing to hear.

### The connection keeps dropping

ALL SHARE reconnects on its own and tells you it is doing so. If it happens
often, the Performance panel will show packet loss, which points at the network
rather than at either device.

---

## Privacy and control

* Nobody can connect to your PC without a device you paired yourself.
* Your PC checks that for itself. Even if the ALL SHARE service were
  compromised, it could not let a stranger in.
* Your screen, keystrokes and clipboard are encrypted between the two devices.
  The service in the middle carries only the messages that set the connection
  up, and cannot read the session.
* Only one device can control your PC at a time.
* To remove a device, open its card's menu and choose **Remove this PC**, or on
  the PC run `allshare-agent status` to see what is paired.

To stop ALL SHARE entirely, uninstall it from Settings › Apps like any other
program.
