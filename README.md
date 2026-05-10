# ups-client

A minimal, dependency-light Go client that tails a [Network UPS Tools (NUT)](https://networkupstools.org/) `upsd` instance and fans state-change events out to one or more notification channels: shell command, generic HTTP webhook (works great with [ntfy.sh](https://ntfy.sh/)), remote command over SSH, and Telegram bot.

It speaks the NUT TCP protocol directly — it does **not** shell out to `upsc` / `upsmon`. The NUT USB driver (`usbhid-ups` for APC) still does the actual hardware work and exposes data through `upsd`; this client just talks to `upsd`.

Built for an APC Back-UPS BX2200MI on Linux, but the protocol layer is generic and works with any NUT-supported UPS.

## Features

- Single static binary, no runtime deps beyond `upsd`.
- upsmon-equivalent event detection: `ONLINE`, `ONBATT`, `LOWBATT`, `FSD`, `REPLBATT`, `COMMBAD`, `COMMOK`, `NOCOMM`, `BYPASS`/`NOTBYPASS`, `OVERLOAD`/`NOTOVERLOAD`, `TRIM`/`NOTTRIM`, `BOOST`/`NOTBOOST`, `CAL`/`NOTCAL`, `OFF`/`NOTOFF`, `ALARM`/`NOTALARM`, `STARTUP`.
- Four notification channels with per-target event filtering and `text/template` rendering.
- Automatic reconnect with exponential backoff; transient `DATA-STALE` / `DRIVER-NOT-CONNECTED` are handled as comm-bad events without crashing.
- Optional `STARTTLS` for credentialed/remote `upsd` setups.
- APC BX-series firmware quirks accounted for (configurable debounce on the `RB` flap).

## Architecture

```
NUT USB driver (usbhid-ups) ── /var/run/nut/...
                                     │
                                     ▼
                                   upsd  ◄────── ups-client (this) ──► shell / webhook / ssh / telegram
```

`ups-client` connects to `upsd` over TCP (default `127.0.0.1:3493`), polls `ups.status`, diffs the token set against the previous reading, and dispatches one event per token edge to every notifier whose event filter matches.

## Prerequisites: install the NUT USB drivers

You only need the **driver + server** packages from NUT. The `upsc`/`upsmon` clients are not required by `ups-client`.

### Debian / Ubuntu

```bash
sudo apt update
sudo apt install nut-server nut-client    # nut-client gives you upsc for diagnostics; optional
# Or, even more minimal — driver + server only:
sudo apt install --no-install-recommends nut-server
```

### Arch Linux

```bash
sudo pacman -S nut
```

### Configure NUT for an APC BX2200MI

`/etc/nut/ups.conf`:

```ini
[ups]
    driver = usbhid-ups
    port = auto
    desc = "APC Back-UPS BX2200MI"
    # If you see frequent LB/RB flapping in syslog, the BX firmware is the
    # culprit; this driver-side knob suppresses spurious transitions:
    lbrb_log_delay_sec = 3
```

`/etc/nut/upsd.conf`:

```ini
LISTEN 127.0.0.1 3493
```

`/etc/nut/upsd.users`:

```ini
# ups-client only does read-only LIST/GET, so users are optional.
# Add one only if you intend to issue SET/INSTCMD via this client.
```

`/etc/nut/nut.conf`:

```ini
MODE=standalone
```

Then enable the driver and server:

```bash
# Debian/Ubuntu
sudo systemctl enable --now nut-driver@ups.service nut-server.service

# Arch
sudo systemctl enable --now nut-driver.service nut-server.service
```

Verify with `upsc ups@localhost` (if you installed `nut-client`) or with `ups-client -list`.

## Build

A `Makefile` wraps the common workflows:

```bash
make build       # → ./bin/ups-client (trimpath, stripped)
make install     # → $GOBIN/ups-client
make test        # plain unit tests
make test-race   # with the race detector
make cover       # writes coverage.out and prints the total
make vet         # go vet ./...
make check       # vet + race tests (what CI runs)
make tidy        # go mod tidy
make clean       # rm -rf bin/ coverage.out
make help        # list targets
```

Or directly:

```bash
go build -trimpath -o ups-client .
```

The repo targets `go 1.23+`. No CGO.

## Run

```bash
ups-client -config /etc/ups-client/config.yaml
```

CLI flags:

| Flag | Description |
|---|---|
| `-config` | Path to YAML config (default `/etc/ups-client/config.yaml`). |
| `-check` | Parse and validate the config, then exit. |
| `-list` | Connect, dump every NUT variable, and exit. Handy to inspect what your UPS exposes. |
| `-v` | Verbose (debug) logging. |

`SIGINT` / `SIGTERM` trigger a clean shutdown.

## Configuration

YAML; see [`ups-client.example.yaml`](./ups-client.example.yaml) for a complete sample. Top-level keys: `nut`, `monitor`, `notifications`.

### `nut`

```yaml
nut:
  address: 127.0.0.1:3493   # host:port; port defaults to 3493
  ups: ups                  # the section name from /etc/nut/ups.conf
  username: ""              # optional; only needed for SET/INSTCMD
  password: ""              # optional
  timeout: 5s
  tls:                      # optional STARTTLS
    enable: true
    ca_file: /etc/ssl/upsd-ca.pem
    server_name: upsd.example
    insecure_skip_verify: false
```

### `monitor`

```yaml
monitor:
  status_interval: 2s        # ups.status polling cadence (>= 500ms)
  snapshot_interval: 30s     # bulk LIST VAR cadence
  nocomm_threshold: 60s      # COMMBAD ➜ NOCOMM after this much sustained loss
  replbatt_debounce: 30s     # hold RB this long before emitting REPLBATT
  reconnect_backoff: 1s      # initial backoff; doubles, caps at 30s
```

`status_interval=2s` matches `upsd`'s default `pollinterval`. Going lower wastes CPU and risks `DATA-STALE`.

### `notifications`

Every target type accepts an optional `events:` list (case-insensitive); an empty list matches **all** events. Allowed names mirror `upsmon`:

```
STARTUP, ONLINE, ONBATT, LOWBATT, FSD, REPLBATT,
COMMBAD, COMMOK, NOCOMM,
BYPASS, NOTBYPASS, OVERLOAD, NOTOVERLOAD,
TRIM, NOTTRIM, BOOST, NOTBOOST,
CAL, NOTCAL, OFF, NOTOFF, ALARM, NOTALARM
```

#### Template variables

`text/template` is evaluated on every templatable field (shell args, webhook URL/headers/body, SSH command, Telegram message):

| Field | Description |
|---|---|
| `{{.Event}}` | Event name, e.g. `ONBATT` |
| `{{.Message}}` | Pre-formatted human summary |
| `{{.UPS}}` | UPS section name |
| `{{.Status}}` | Current `ups.status` string |
| `{{.PreviousStatus}}` | Previous `ups.status` string |
| `{{.Tokens}}` | Sorted slice of current status tokens |
| `{{.Time}}` | Event timestamp (`time.Time`) |
| `{{.BatteryCharge}}` | `battery.charge` (percent, string) |
| `{{.BatteryRuntime}}` | `battery.runtime` (seconds, string) |
| `{{.InputVoltage}}` / `{{.OutputVoltage}}` | Mains / output volts |
| `{{.UPSLoad}}` | `ups.load` percent |
| `{{.DeviceModel}}` / `{{.DeviceSerial}}` | From `device.*` |
| `{{.Vars}}` | Raw map of every NUT variable (use `{{index .Vars "ups.alarm"}}`) |

#### Shell

```yaml
shell:
  - name: log
    command: /usr/bin/logger
    args: ["-t", "ups-client", "{{.Event}} on {{.UPS}} charge={{.BatteryCharge}}%"]
    timeout: 5s
    events: [ONBATT, ONLINE, LOWBATT, FSD]
```

The child process inherits your environment plus `UPS_*` variables: `UPS_EVENT`, `UPS_NAME`, `UPS_STATUS`, `UPS_PREVIOUS_STATUS`, `UPS_BATTERY_CHARGE`, `UPS_BATTERY_RUNTIME`, `UPS_INPUT_VOLTAGE`, `UPS_OUTPUT_VOLTAGE`, `UPS_LOAD`, `UPS_DEVICE_MODEL`, `UPS_DEVICE_SERIAL`, `UPS_TIMESTAMP`. The optional per-target `env:` map adds extra keys.

#### Webhook (and ntfy)

```yaml
webhook:
  - name: home-assistant
    url: https://ha.example/api/webhook/ups-events
    timeout: 5s
    # When body is empty the request body is the JSON-encoded TemplateData.
```

##### ntfy ([ntfy.sh](https://ntfy.sh/))

ntfy is a pub/sub HTTP notifier. Pick a topic (any string — keep it private since the topic is the only secret), then POST plain text with optional `Title`, `Priority`, `Tags` headers. The generic webhook handles this without a dedicated channel:

```yaml
webhook:
  - name: ntfy
    url: https://ntfy.sh/my-private-topic    # or self-host: https://ntfy.example/...
    headers:
      Title:    "UPS {{.UPS}} – {{.Event}}"
      Priority: "high"                       # min, low, default, high, urgent
      Tags:     "warning,electric_plug"      # comma-separated emoji shortcodes
    body: "{{.Event}} on {{.UPS}} (status: {{.Status}}, charge: {{.BatteryCharge}}%, runtime: {{.BatteryRuntime}}s)"
    timeout: 5s
    events: [ONBATT, LOWBATT, FSD, ONLINE, COMMBAD, COMMOK]
```

Subscribe on your phone with the [ntfy app](https://ntfy.sh/) (`Subscribe to topic` → enter `my-private-topic`) or via `curl -s https://ntfy.sh/my-private-topic/sse`.

For self-hosted ntfy with auth, add a bearer token via headers:

```yaml
    headers:
      Authorization: "Bearer tk_xxxxxxxx"
      Title: "UPS {{.UPS}} – {{.Event}}"
```

Reference: <https://ntfy.sh/docs/publish/>.

#### SSH

```yaml
ssh:
  - name: nas
    host: nas.lan
    port: 22
    user: ops
    # Pick exactly one auth method:
    private_key_file: /etc/ups-client/id_ed25519
    private_key_passphrase: ""    # optional
    # password: "..."             # alternative
    known_hosts_file: /etc/ups-client/known_hosts
    # insecure_ignore_host_key: true  # NOT recommended
    command: |
      logger -t ups "{{.Event}} on {{.UPS}}"
      [ "{{.Event}}" = "ONBATT" ] && systemctl stop heavy-job.service
    timeout: 10s
    events: [ONBATT, LOWBATT, FSD, ONLINE]
```

Generate the trust pin once with `ssh-keyscan -H nas.lan >> /etc/ups-client/known_hosts`.

#### Telegram

Create a bot with [@BotFather](https://t.me/BotFather), grab the token, and look up your chat id (e.g. message [@RawDataBot](https://t.me/RawDataBot)).

```yaml
telegram:
  - name: ops
    bot_token: "123456:ABC-DEF..."
    chat_id: "-1001234567890"          # negative for groups/channels
    parse_mode: MarkdownV2             # optional
    message: "*UPS {{.UPS}}*: `{{.Event}}` \\| status `{{.Status}}` \\| charge {{.BatteryCharge}}%"
    timeout: 5s
```

## Events

Detected by diffing successive `ups.status` token sets:

| Event | Trigger |
|---|---|
| `STARTUP` | First successful poll after the client launches |
| `ONLINE` | `OL` token entered (mains restored) |
| `ONBATT` | `OB` token entered (running on battery) |
| `LOWBATT` | `LB` token entered |
| `FSD` | `FSD` token entered (forced shutdown) |
| `REPLBATT` | `RB` token persists past `replbatt_debounce` |
| `BYPASS` / `NOTBYPASS` | `BYPASS` token enter / leave |
| `OVERLOAD` / `NOTOVERLOAD` | `OVER` token enter / leave |
| `TRIM` / `NOTTRIM` | `TRIM` token enter / leave (mains too high — SmartTrim) |
| `BOOST` / `NOTBOOST` | `BOOST` token enter / leave (mains too low — SmartBoost) |
| `CAL` / `NOTCAL` | runtime calibration enter / leave |
| `OFF` / `NOTOFF` | output `OFF` token enter / leave |
| `ALARM` / `NOTALARM` | active alarm enter / leave |
| `COMMBAD` | TCP loss, `DATA-STALE`, or `DRIVER-NOT-CONNECTED` |
| `COMMOK` | recovery from `COMMBAD` |
| `NOCOMM` | sustained `COMMBAD` past `nocomm_threshold` |

## systemd unit

The service runs as a dedicated, unprivileged system user `ups-client`. systemd does **not** create the account on its own — you have to set it up once, either manually or declaratively via `systemd-sysusers`.

### 1. Create the system user

Pick one of:

```bash
# Imperative (Debian/Ubuntu/Arch)
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ups-client
```

```bash
# Declarative — recommended; survives package reinstalls
sudo tee /etc/sysusers.d/ups-client.conf >/dev/null <<'EOF'
u ups-client - "ups-client service" -
EOF
sudo systemd-sysusers
```

Then make sure the config (and any SSH key file referenced from it) is readable by that user:

```bash
sudo install -d -o root -g ups-client -m 0750 /etc/ups-client
sudo install -o root -g ups-client -m 0640 ups-client.example.yaml /etc/ups-client/config.yaml
# If you use the SSH notifier:
sudo install -o root -g ups-client -m 0640 /path/to/id_ed25519 /etc/ups-client/id_ed25519
```

### 2. Install the unit

```ini
# /etc/systemd/system/ups-client.service
[Unit]
Description=UPS event client
After=nut-server.service network-online.target
Wants=network-online.target
Requires=nut-server.service

[Service]
Type=simple
User=ups-client
Group=ups-client
ExecStart=/usr/local/bin/ups-client -config /etc/ups-client/config.yaml
Restart=on-failure
RestartSec=5s

# Hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
ReadOnlyPaths=/etc/ups-client

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ups-client.service
journalctl -u ups-client.service -f
```

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `connect: dial tcp 127.0.0.1:3493: connect: connection refused` | `upsd` not running or not listening on 127.0.0.1; check `LISTEN` in `upsd.conf`. |
| `nut: NUT error: ACCESS-DENIED` | The UPS section requires auth; add `username` + `password` to `nut:`. |
| `nut: NUT error: UNKNOWN-UPS` | `nut.ups` doesn't match the section name in `ups.conf`. |
| Spurious `REPLBATT` | APC BX firmware quirk. Increase `monitor.replbatt_debounce` (e.g. `120s`) or set `lbrb_log_delay_sec` in `ups.conf`. |
| `DATA-STALE` floods | The driver lost the device. Check `dmesg` for USB resets; consider `maxreport=1` in `ups.conf` for some BX firmware. |

## Development

```bash
make check       # go vet + race tests
make cover       # coverage report
```

## License

MIT — see [`LICENSE`](./LICENSE).

## References

- NUT Developer Guide — [Network protocol](https://networkupstools.org/docs/developer-guide.chunked/net-protocol.html)
- [`usbhid-ups(8)`](https://networkupstools.org/docs/man/usbhid-ups.html)
- [`upsmon.conf(5)`](https://networkupstools.org/docs/man/upsmon.conf.html)
- [RFC 9271](https://datatracker.ietf.org/doc/rfc9271/) — NUT protocol
- [ntfy.sh](https://ntfy.sh/) — pub/sub HTTP notifier
