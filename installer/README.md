# Building the ALL SHARE installer

The installer is an [Inno Setup](https://jrsoftware.org/isinfo.php) script. It
produces a single `AllShareSetup.exe` that installs the agent, registers the
Windows service, and walks the user through their first pairing.

## Prerequisites

* Inno Setup 6 or newer (`winget install JRSoftware.InnoSetup`)
* A built agent at `dist\windows\allshare-agent.exe`

## Build

```powershell
# From a machine with the Go and mingw toolchains, or on Windows with MSVC:
bash build/build-agent.sh
bash build/build-client.sh

# Then compile the installer:
iscc installer\allshare.iss
```

The result is `dist\AllShareSetup.exe`.

To stamp a version:

```powershell
$env:ALLSHARE_VERSION = "1.2.0"
iscc installer\allshare.iss
```

## What the installer does

1. Asks for administrator rights once, up front. Screen capture and service
   registration both need them, and failing halfway through is worse than
   asking early.
2. Collects the ALL SHARE service address and a name for this PC, so the user
   never has to edit a configuration file.
3. Copies `allshare-agent.exe` into Program Files.
4. Runs `allshare-agent install`, which registers the Windows service with
   automatic start, delayed start, and restart-on-failure.
5. Optionally adds a Windows Firewall **program** rule. ALL SHARE listens on no
   fixed port — it makes outbound connections and receives peer-to-peer traffic
   on ports the operating system chooses — so a program rule is both sufficient
   and much narrower than opening a port range.
6. Offers to show a pairing code immediately.

Uninstalling runs `allshare-agent uninstall`, which stops and removes the
service, deletes the scheduled wake timer, and removes the firewall rule. The
PC's cryptographic identity is deliberately left in place unless settings are
cleared: deleting it would silently un-pair every device the user has set up.
