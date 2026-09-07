# Setting up the ALL SHARE service

ALL SHARE needs one small server on the internet whose only job is to introduce
your PC to your Chromebook. It is called the **rendezvous**. This page explains
what it does, how to run one, and how much it costs.

If somebody has already given you a service address that starts with `wss://`,
you do not need this page — go to the [user guide](user-guide.md).

---

## 1. Why there is a server at all

Your Chromebook and your PC both sit behind routers that block unsolicited
incoming connections. Neither can simply dial the other. Something with a fixed,
public address has to hold the door open so the two can find each other.

That is all the rendezvous does. Once the two devices have found each other,
video, audio, keyboard, mouse and clipboard travel **directly between them**,
encrypted end to end. The server does not see any of it and could not decrypt it
if it tried — the keys are negotiated by the two devices and never leave them.

What the server *does* know:

- which devices are online, and their public keys
- which device asked to reach which other device, and when
- the IP addresses connections came from (it keeps only an HMAC of these, used
  to spot when two devices are on the same network)

What it cannot do, even if completely taken over: read your screen, read your
keystrokes, join a session, or pair a device of its own. That is not an accident
of configuration — the design assumes the server is hostile. See
[security.md](security.md), sections 4.4 and 4.5, for exactly why.

---

## 2. What it costs

The rendezvous is nearly free to run. The relay, if you need it, is not.

| Component | Traffic | Realistic cost |
|---|---|---|
| Rendezvous (signalling only) | A few kilobytes per connection | The smallest VPS anywhere — €3–5/month, or free tier |
| STUN (address discovery) | A few hundred bytes | Free; public servers are fine |
| TURN relay (fallback) | **All your video**, ~1 GB per 15 minutes at 10 Mbit/s | Bandwidth-priced; see below |

About nine connections in ten never touch the relay — they find a direct path.
The tenth needs it, usually because both ends are behind carrier-grade NAT or a
network blocks UDP. Whether you run a relay is the main decision on this page.

**Recommendation:** run the rendezvous with the relay enabled on a VPS that
includes generous bandwidth (Hetzner, OVH and Contabo all include 20 TB or more
on their cheapest plans). At that point the relay is effectively free and you
never hit the "it works everywhere except at my parents' house" problem.

If you would rather not run a relay at all, `-stun-only` advertises public STUN
servers and no relay. It costs nothing and works most of the time. Networks that
need a relay will report *"Could not find a path to your PC"* and there will be
nothing the user can do about it, which is why it is not the default.

---

## 3. The quick way

On a fresh Debian or Ubuntu VPS with a domain name pointed at it:

```bash
# 1. Get the binary (or build it: bash build/build-server.sh)
sudo mkdir -p /opt/allshare /var/lib/allshare
sudo cp allshare-server-linux-amd64 /opt/allshare/allshare-server
sudo chmod +x /opt/allshare/allshare-server

# 2. Run it as its own user
sudo useradd --system --home /var/lib/allshare --shell /usr/sbin/nologin allshare
sudo chown -R allshare:allshare /var/lib/allshare
```

`/etc/systemd/system/allshare.service`:

```ini
[Unit]
Description=ALL SHARE rendezvous
After=network-online.target
Wants=network-online.target

[Service]
User=allshare
Group=allshare
ExecStart=/opt/allshare/allshare-server \
    -listen 127.0.0.1:8443 \
    -data /var/lib/allshare \
    -trust-proxy \
    -turn-listen :3478 \
    -turn-public-ip YOUR.PUBLIC.IP.HERE
Restart=always
RestartSec=5

# The server needs almost nothing from the system. Take the rest away.
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/allshare
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
# Binding :3478 needs the capability, nothing more.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

`/etc/caddy/Caddyfile`:

```
allshare.example.com {
    reverse_proxy 127.0.0.1:8443
}
```

Then:

```bash
sudo systemctl enable --now allshare
sudo systemctl reload caddy
sudo ufw allow 443/tcp
sudo ufw allow 3478/udp
sudo ufw allow 49152:65535/udp   # relay media ports
```

Your service address is `wss://allshare.example.com/rv`. Check it:

```bash
curl https://allshare.example.com/healthz
```

---

## 4. TLS is not optional

The server process speaks plain HTTP and expects a reverse proxy in front of it
to terminate TLS. That is deliberate: Caddy and nginx solve certificate issuance
and renewal properly, and a hand-rolled ACME client inside a remote-desktop
server would be a worse version of software that already exists.

**The client enforces this.** Both the browser client and the Windows agent
refuse a `ws://` service address unless the host is on your local network. There
is no flag to override it. Signalling carries the SDP — which lists every
address your devices know about — and the relay credentials for the session, and
none of that belongs in the clear.

`ws://` remains available for hosts on your own network (loopback, `10/8`,
`172.16/12`, `192.168/16`, link-local, and `.local` names), because a rendezvous
running on your own LAN is a real deployment and nobody can get a publicly
trusted certificate for `192.168.1.10`.

### nginx, if you prefer it

```nginx
server {
    listen 443 ssl http2;
    server_name allshare.example.com;

    ssl_certificate     /etc/letsencrypt/live/allshare.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/allshare.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8443;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;

        # Signalling connections are long-lived and mostly idle. Without this,
        # nginx closes them after 60 seconds and every PC in the list blinks
        # offline once a minute.
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

That `proxy_read_timeout` is the single most common deployment mistake. If your
PCs keep going grey and coming back, this is why.

**`-trust-proxy` goes with a reverse proxy and only with one.** It makes the
server believe `X-Forwarded-For`. Behind Caddy or nginx that header is the real
client address; exposed directly to the internet it is whatever an attacker
types, and every per-address rate limit becomes trivially evadable. Set it when
something you control sets the header, and never otherwise.

---

## 5. Every option

| Flag | Environment variable | Default | What it does |
|---|---|---|---|
| `-listen` | `ALLSHARE_LISTEN` | `:8443` | Address for signalling. Use `127.0.0.1:8443` behind a proxy |
| `-data` | `ALLSHARE_DATA` | `./data` | Where the device registry and server key live |
| `-realm` | `ALLSHARE_REALM` | `allshare` | TURN realm |
| `-turn-listen` | `ALLSHARE_TURN_LISTEN` | *(off)* | UDP address for the built-in relay, e.g. `:3478` |
| `-turn-public-ip` | `ALLSHARE_TURN_PUBLIC_IP` | *(none)* | **Required with `-turn-listen`** — the server refuses to start without it. The public IPv4 peers can reach |
| `-turn-secret` | `ALLSHARE_TURN_SECRET` | *(generated)* | Secret keying ephemeral relay credentials. Generated into the data directory if unset |
| `-turn-ttl` | — | `12h` | How long a minted relay credential lasts |
| `-stun-only` | `ALLSHARE_STUN_ONLY` | off | Advertise public STUN, run no relay |
| `-ice-servers` | `ALLSHARE_ICE_SERVERS` | *(none)* | Extra ICE servers as JSON, for a managed relay |
| `-trust-proxy` | `ALLSHARE_TRUST_PROXY` | off | Believe `X-Forwarded-For`. Only behind a proxy you control |
| `-local-wol` | `ALLSHARE_LOCAL_WOL` | off | Let this server broadcast wake packets on its own network. Only meaningful when self-hosted on the same LAN as the PCs |
| `-log-level` | `ALLSHARE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `-log-json` | `ALLSHARE_LOG_JSON` | off | Structured logs for a log collector |

`-turn-public-ip` is worth a second look: a relay that advertises a private
address hands out candidates nobody can reach, and the symptom is a connection
that negotiates happily and then carries no video. If the server is behind NAT,
this must be the address the outside world sees.

### Using a managed relay instead

If you would rather not run the relay yourself, point at one you already have:

```bash
allshare-server \
  -listen 127.0.0.1:8443 \
  -ice-servers '[{"urls":["turn:turn.example.com:3478"],"username":"u","credential":"p"}]'
```

Anything the browser accepts in `RTCIceServer` works here.

---

## 6. Firewall

| Port | Protocol | Who needs it | Why |
|---|---|---|---|
| 443 | TCP | Everyone | Signalling over TLS |
| 3478 | UDP | Everyone, if you run the relay | TURN |
| 49152–65535 | UDP | Everyone, if you run the relay | Relayed media |

If you do not run a relay, port 443 alone is enough.

Cloud provider note: on AWS, GCP and Azure the security group is a second
firewall in front of the machine's own. A relay that "does not work" on a cloud
VM is almost always the UDP range missing there.

---

## 7. Setting up each PC

On each Windows PC, once:

```
allshare-agent config -service wss://allshare.example.com/rv -name "Gaming PC"
allshare-agent install
```

`install` registers the Windows service, which starts at boot and does not need
anyone to log in. Then, to add a device:

```
allshare-agent pair
```

It prints a 12-character code, valid for three minutes, that you type into the
client. Full walkthrough in the [user guide](user-guide.md).

The installer does all of this with a wizard — see
[installer/README.md](../installer/README.md).

---

## 8. Health, backup and upgrade

**Health.** `GET /healthz` returns liveness, version, the server's public
fingerprint, and connection counts. Point a monitor at it.

```bash
curl -s https://allshare.example.com/healthz | jq
```

**Backup.** The data directory holds three things:

| File | Losing it means |
|---|---|
| `server-identity.key` | Every device must re-authenticate once (harmless, automatic) |
| `devices.json` | Every device must be paired again (annoying) |
| `turn.secret`, `lan-key.secret` | In-flight relay credentials stop working; regenerated on start |

Copy the directory. It is small and does not change often. Note it contains
private keys — back it up somewhere you would be willing to keep an SSH key.

**Upgrade.** Replace the binary and restart. Agents and clients reconnect on
their own with exponential backoff, so a restart shows up as a few seconds of
"Reconnecting…" and nothing else. Running sessions survive it entirely — media
does not go through the server, so a session already established keeps streaming
while the rendezvous is down. This is covered by an automated test
(`TestReconnectAndServiceOutage`).

---

## 9. Running it on your own network

If all your devices are on one LAN and you never need access from outside, you
can skip the VPS and the domain entirely:

```bash
allshare-server -listen :8443 -stun-only -local-wol
```

Then use `ws://192.168.1.50:8443/rv` as the service address. The client allows
plain `ws://` for private addresses precisely for this case.

`-local-wol` lets the server send wake-on-LAN packets on its own network, which
is the most reliable wake method there is — see [wake.md](wake.md). It is opt-in
because it only makes sense when the server and the PCs share a network.

This setup gives you the lowest latency ALL SHARE can achieve (a direct LAN
path, typically 15–25 ms glass-to-glass) and costs nothing. It just does not
work from a coffee shop.

---

## 10. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| PCs go offline every ~60 seconds | Reverse-proxy read timeout | Set `proxy_read_timeout 3600s` (Section 4) |
| "Could not find a path to your PC" | No relay, and this network needs one | Run the relay, or check UDP 3478 and 49152–65535 |
| Connects, then no video | Relay advertising a private address | Set `-turn-public-ip` to the public address |
| Client refuses the address | It is `ws://` to a public host | Use `wss://` — see Section 4 |
| Rate limits triggering for everyone | `-trust-proxy` not set behind a proxy, so every client looks like the proxy | Set `-trust-proxy` |
| Rate limits never triggering | `-trust-proxy` set without a proxy | Remove it |
| `/healthz` fine, clients cannot connect | Proxy not forwarding WebSocket upgrade | Check `Upgrade`/`Connection` headers (Section 4) |

For anything else, `-log-level debug` prints the handshake step by step. It does
not print keys or pairing codes.
