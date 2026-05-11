# ups-client

> 🔌 Pretty UPS event notifications — straight to your phone or chat — without [`upsmon`](https://networkupstools.org/docs/man/upsmon.html), without per-event shell scripts, without a custom shellout for every channel.

[![CI](https://github.com/mikispag/ups-client/actions/workflows/ci.yml/badge.svg)](https://github.com/mikispag/ups-client/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/mikispag/ups-client.svg)](https://pkg.go.dev/github.com/mikispag/ups-client)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

A single static Go binary that watches your UPS via [Network UPS Tools (NUT)](https://networkupstools.org/) and fans every state-change event out to whichever channels you wire up:

- 📱 **[ntfy.sh](https://ntfy.sh/)** — emoji-titled phone push with severity-tiered priority and tags
- 💬 **Telegram** — rich messages to a bot + chat
- 🐚 **Shell** — any local command, with `UPS_*` env vars (e.g. `shutdown -h +1` on `FSD`)
- 🔐 **SSH** — kick a remote command (stop a NAS job, page a server)
- 🔗 **Generic webhook** — Home Assistant, Slack, Discord, Mattermost, …

When the mains drop you get this on your phone, with a single ntfy buzz:

```
🔌 UPS ups · running on battery        (priority: high   · tags: orange_circle, battery)

🔌 Now running on battery.

────────────────────────────
🔋 Charge   98%
⏱  Runtime  3600s
⚖  Load     12%
🔌 Mains    230V
📊 Status   OB DISCHRG
🕒 At       2026-05-10 12:34:56 UTC
```

When it's almost out, an *urgent* push punches through Do Not Disturb:

```
🪫 UPS ups · LOW BATTERY              (priority: high   · tags: rotating_light, low_battery)
🛑 UPS ups · forced shutdown           (priority: urgent · tags: rotating_light, zap)
```

## Why not just upsmon?

`upsmon` ships with NUT, runs a single `NOTIFYCMD` shellout per event, and leaves you to write a wrapper that handles ntfy, Telegram, SSH, retries, templating, and APC firmware quirks. `ups-client` does that wrapper job natively:

- **Multi-channel out of the box** — every event fans out to ntfy + Telegram + SSH + shell + webhook in parallel, each with its own event filter and `text/template`-rendered body.
- **Severity-tiered notifications** — `FSD`/`NOCOMM`/`OVERLOAD` are urgent, `LOWBATT`/`REPLBATT`/`ALARM`/`ONBATT`/`COMMBAD` are high, transitions are default, `STARTUP` is low. No more single-pitch alerts that trains you to ignore them.
- **No surprise pages from APC firmware** — the BX-series `RB`-token flap is debounced client-side (default 600s) on top of the driver knobs `lbrb_log_delay_sec` + `maxreport=1`. See [Tuning the APC-BX flap mitigation](#tuning-the-apc-bx-flap-mitigation).
- **No CGO, no system Python, no helper shell** — single static Go binary (~8 MB), hardened systemd unit + sysusers snippet ship in the repo.
- **Built for an APC Back-UPS BX2200MI**, generic for any NUT-supported UPS.

## Quick start

```bash
# 1. Install NUT (see below for distro-specific commands), then:
git clone https://github.com/mikispag/ups-client && cd ups-client
sudo make install                                       # binary, unit, sysusers, example config
sudo systemd-sysusers                                   # creates the ups-client user
sudo cp /etc/ups-client/config.example.yaml /etc/ups-client/config.yaml
sudo $EDITOR /etc/ups-client/config.yaml                # plug in your ntfy topic, Telegram token, …
sudo /usr/local/bin/ups-client -check -config /etc/ups-client/config.yaml
sudo systemctl enable --now ups-client.service
journalctl -u ups-client -f
```

You'll get an emoji-titled push the next time the mains flicker.

## Features

- **Wire-protocol native** — speaks NUT TCP directly to `upsd`; no shellouts to `upsc`/`upsmon`.
- **upsmon-equivalent events** — `ONLINE`, `ONBATT`, `LOWBATT`, `FSD`, `REPLBATT`, `COMMBAD`, `COMMOK`, `NOCOMM`, plus enter/leave edges for `BYPASS`, `OVERLOAD`, `TRIM`, `BOOST`, `CAL`, `OFF`, `ALARM`, and a `STARTUP` ping.
- **Auto-reconnect with backoff** — transient `DATA-STALE` / `DRIVER-NOT-CONNECTED` surface as `COMMBAD`/`COMMOK` rather than crashing the daemon.
- **Per-target event filters** plus `text/template` rendering on every body, header, arg.
- **Optional `STARTTLS`** for credentialed or remote `upsd` setups.
- **Hardened systemd unit + dedicated user**, set up by `make install`.

## How it works

```
  ┌─────────────────────┐    USB
  │  APC BX2200MI / …   │ ◀────────┐
  └─────────────────────┘          │
                                   │
                       ┌───────────┴────────────┐
                       │ usbhid-ups (NUT driver)│
                       └───────────┬────────────┘
                                   │ shared mem
                                   ▼
                              ┌─────────┐
                              │  upsd   │  127.0.0.1:3493 (TCP)
                              └────┬────┘
                                   │ NUT protocol
                                   ▼
                            ┌─────────────┐
                            │ ups-client  │
                            └──────┬──────┘
              ┌────────┬───────────┼───────────┬────────┐
              ▼        ▼           ▼           ▼        ▼
            shell    webhook     ntfy        ssh     telegram
                    (HA, …)
```

`ups-client` keeps a long-lived TCP connection to `upsd`, polls `ups.status` every 2 s (matching `upsd`'s `pollinterval`), diffs the token set against the previous reading, and dispatches one event per token edge to every notifier whose event filter matches — all in parallel.

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
    # APC BX firmware misreports HID report lengths if the driver reads
    # several reports in one pass. Reading one at a time avoids DATA-STALE
    # storms and the spurious LB/RB tokens that come with them.
    maxreport = 1
    # Driver-side debounce of LB/RB transitions. 3s suppresses the very
    # short flaps; the client-side replbatt_debounce (600s) catches the
    # longer ones.
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
sudo systemctl enable --now nut-driver-enumerator.service nut-server.service
```

Verify with `upsc ups@localhost` (if you installed `nut-client`) or with `ups-client -list`.

## Build

A `Makefile` wraps the common workflows:

```bash
make             # housekeeping pass: tidy + vet + race tests + build
make build       # → ./bin/ups-client (trimpath, stripped)
make install     # binary + systemd unit + sysusers snippet + example config
make uninstall   # reverse of install (keeps your config.yaml + the user)
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

### `make install` layout

| Source in repo | Installed to | Mode |
|---|---|---|
| `bin/ups-client` | `$(PREFIX)/bin/ups-client` (default `/usr/local/bin`) | `0755` |
| `init/ups-client.service.in` | `$(SYSCONFDIR)/systemd/system/ups-client.service` (`@BINDIR@` → `$(PREFIX)/bin`) | `0644` |
| `init/ups-client.sysusers` | `$(SYSCONFDIR)/sysusers.d/ups-client.conf` | `0644` |
| `ups-client.example.yaml` | `$(SYSCONFDIR)/ups-client/config.example.yaml` | `0644` |

The defaults (`/usr/local/bin`, `/etc/systemd/system`, `/etc/sysusers.d`, `/etc/ups-client`) are the correct admin-install paths on every systemd-based distro — Arch, Debian, Ubuntu, Fedora/RHEL, openSUSE — so `sudo make install` works the same way everywhere with no distro detection. Packagers building a `.deb`/PKGBUILD/RPM override the per-package conventions via `DESTDIR`, `PREFIX`, `UNITDIR`, `SYSUSERSDIR`; see the `Makefile` for the full list of knobs.

After install you get a printed checklist of the placeholder values that **must** be replaced before the service can start (ntfy URL, Telegram token/chat id, SSH host/key, etc.) — the service unit deliberately points at `config.yaml`, not `config.example.yaml`, so the daemon refuses to start until you make a copy and edit it.

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
  replbatt_debounce: 600s    # hold RB this long before emitting REPLBATT
  alarm_debounce: 60s        # hold ALARM this long before emitting ALARM
  reconnect_backoff: 1s      # initial backoff; doubles, caps at 30s
```

`status_interval=2s` matches `upsd`'s default `pollinterval`. Going lower wastes CPU and risks `DATA-STALE`.

### Tuning the APC-BX flap mitigation

Three knobs at three layers — sane defaults for a BX2200MI in parentheses:

| Layer | Knob | Default | What it does |
|---|---|---|---|
| `usbhid-ups` driver (`ups.conf`) | `maxreport` | **`1`** | Read one HID report per polling pass. Avoids broken-length reads that surface as `DATA-STALE` and ghost `LB`/`RB` tokens. |
| `usbhid-ups` driver (`ups.conf`) | `lbrb_log_delay_sec` | **`3`** | Suppress LB/RB transitions shorter than this. Catches sub-second blips. |
| `ups-client` (`config.yaml`) | `monitor.replbatt_debounce` | **`600s`** | Only emit `REPLBATT` after the `RB` token has held continuously this long. Catches slow flaps the driver lets through. |
| `ups-client` (`config.yaml`) | `monitor.alarm_debounce` | **`60s`** | Only emit `ALARM` after the `ALARM` token has held continuously this long. APC BX firmwares assert brief ALARMs during background self-tests; 60s suppresses those without meaningfully delaying real alarms. The `ups.alarm` variable is captured and exposed as `{{.Alarm}}` so notifier templates can render the actual reason ("Replace battery", …). |

If you keep getting spurious `REPLBATT`, raise `replbatt_debounce` first; the driver-side knobs only need attention if you also see `DATA-STALE` or ghost `LOWBATT`s.

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
| `{{.Alarm}}` | `ups.alarm` reason string, captured when the `ALARM` token is asserted (e.g. `Replace battery`) |
| `{{.Vars}}` | Raw map of every NUT variable (use `{{index .Vars "some.key"}}`) |

> The bundled [`ups-client.example.yaml`](./ups-client.example.yaml) ships with **pretty, emoji-led message templates** wired up for every event — severity-tiered ntfy `Priority`/`Tags` (urgent for `FSD`/`NOCOMM`/`OVERLOAD`, high for `LOWBATT`/`REPLBATT`/`ALARM`/`ONBATT`/`COMMBAD`/`BYPASS`/`OFF`, default for transitions, low for `STARTUP`), a single `text/template` if/else chain on `.Event` per field, and a clean metric block. The snippets below show the pattern condensed; copy the full chains from the example file for production.

Sample rendering (ntfy):

```
🟢 UPS ups · back on mains          🔌 UPS ups · running on battery
🪫 UPS ups · LOW BATTERY            🛑 UPS ups · forced shutdown
🔋 UPS ups · replace battery        🚨 UPS ups · UNREACHABLE
🔥 UPS ups · OVERLOAD               ⚡ UPS ups · bypass active

🔋 Charge   98%
⏱  Runtime  3600s
⚖  Load     12%
🔌 Mains    230V
📊 Status   OL CHRG
🕒 At       2026-05-10 12:34:56 UTC
```

#### Shell

Emoji-prefixed log line for fast visual scanning in `journalctl -u ups-client -f`:

```yaml
shell:
  - name: log
    command: /usr/bin/logger
    args:
      - "-t"
      - "ups-client"
      - >-
        {{- if eq .Event "ONLINE" }}🟢
        {{- else if eq .Event "ONBATT" }}🔌
        {{- else if eq .Event "LOWBATT" }}🪫
        {{- else if eq .Event "FSD" }}🛑
        {{- else }}⚙️
        {{- end }} {{ .Event }} ups={{.UPS}} status="{{.Status}}" charge={{.BatteryCharge}}%
    timeout: 5s
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

ntfy is a pub/sub HTTP notifier. Pick a topic (any string — keep it private since the topic is the only secret), then POST plain text with optional `Title`, `Priority`, `Tags` headers. The generic webhook handles this without a dedicated channel. The full templates in [`ups-client.example.yaml`](./ups-client.example.yaml) tier the priority and tags by event severity; condensed pattern:

```yaml
webhook:
  - name: ntfy
    url: https://ntfy.sh/my-private-topic    # or self-host: https://ntfy.example/...
    headers:
      Title: >-
        {{- if eq .Event "ONLINE" }}🟢 UPS {{.UPS}} · back on mains
        {{- else if eq .Event "ONBATT" }}🔌 UPS {{.UPS}} · running on battery
        {{- else if eq .Event "LOWBATT" }}🪫 UPS {{.UPS}} · LOW BATTERY
        {{- else if eq .Event "FSD" }}🛑 UPS {{.UPS}} · forced shutdown
        {{- else }}⚙️ UPS {{.UPS}} · {{.Event}}{{ end }}
      Priority: >-
        {{- if eq .Event "FSD" "NOCOMM" "OVERLOAD" }}urgent
        {{- else if eq .Event "LOWBATT" "REPLBATT" "ALARM" "ONBATT" "COMMBAD" }}high
        {{- else if eq .Event "STARTUP" }}low
        {{- else }}default{{ end }}
      Tags: >-
        {{- if eq .Event "ONLINE" }}green_circle,electric_plug
        {{- else if eq .Event "ONBATT" }}orange_circle,battery
        {{- else if eq .Event "LOWBATT" }}rotating_light,low_battery
        {{- else }}gear{{ end }}
    body: |-
      {{- if eq .Event "ONLINE" }}🟢 Mains power restored.
      {{- else if eq .Event "ONBATT" }}🔌 Now running on battery.
      {{- else }}⚙️ {{ .Event }}{{ end }}

      🔋 Charge   {{ .BatteryCharge }}%
      ⏱  Runtime  {{ .BatteryRuntime }}s
      ⚖  Load     {{ .UPSLoad }}%
    timeout: 5s
```

Subscribe on your phone with the [ntfy app](https://ntfy.sh/) (`Subscribe to topic` → enter `my-private-topic`) or via `curl -s https://ntfy.sh/my-private-topic/sse`.

For self-hosted ntfy with auth, add a bearer token via headers:

```yaml
    headers:
      Authorization: "Bearer tk_xxxxxxxx"
      Title: "UPS {{.UPS}} – {{.Event}}"
```

ntfy emoji shortcode list: <https://docs.ntfy.sh/emojis/>. Reference: <https://ntfy.sh/docs/publish/>.

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
      logger -t ups "🔔 {{.Event}} on {{.UPS}} status={{.Status}} charge={{.BatteryCharge}}%"
      case "{{.Event}}" in
        ONBATT|LOWBATT|FSD) systemctl stop heavy-job.service ;;
        ONLINE)             systemctl start heavy-job.service ;;
      esac
    timeout: 10s
    events: [ONBATT, LOWBATT, FSD, ONLINE]
```

Generate the trust pin once with `ssh-keyscan -H nas.lan >> /etc/ups-client/known_hosts`.

#### Telegram

Create a bot with [@BotFather](https://t.me/BotFather), grab the token, and look up your chat id (e.g. message [@RawDataBot](https://t.me/RawDataBot)). Plain text (no `parse_mode`) keeps the message tolerant of arbitrary characters in `Status`; switch to `HTML` or `MarkdownV2` only if you need rich formatting and accept the escaping cost:

```yaml
telegram:
  - name: ops
    bot_token: "123456:ABC-DEF..."
    chat_id: "-1001234567890"          # negative for groups/channels
    message: |-
      {{- if eq .Event "ONLINE" }}🟢 UPS {{.UPS}} — back on mains
      {{- else if eq .Event "ONBATT" }}🔌 UPS {{.UPS}} — running on battery
      {{- else if eq .Event "LOWBATT" }}🪫 UPS {{.UPS}} — LOW BATTERY
      {{- else if eq .Event "FSD" }}🛑 UPS {{.UPS}} — forced shutdown imminent
      {{- else }}⚙️ UPS {{.UPS}} — {{.Event}}{{ end }}

      🔋 {{.BatteryCharge}}%   ⏱ {{.BatteryRuntime}}s   ⚖ {{.UPSLoad}}%   🔌 {{.InputVoltage}}V
      📊 {{.Status}}
    timeout: 5s
```

## Events

Detected by diffing successive `ups.status` token sets:

| Event | Trigger |
|---|---|
| `STARTUP` | First successful poll after the client launches |
| `ONLINE` | `OL` token entered (mains restored) |
| `ONBATT` | `OB` token entered (running on battery) |
| `LOWBATT` | `LB` token entered **while `OB` is also present** (bare `LB` on `OL` is noise on APC BX-series and is suppressed) |
| `FSD` | `FSD` token entered (forced shutdown) |
| `REPLBATT` | `RB` token persists past `replbatt_debounce` |
| `BYPASS` / `NOTBYPASS` | `BYPASS` token enter / leave |
| `OVERLOAD` / `NOTOVERLOAD` | `OVER` token enter / leave |
| `TRIM` / `NOTTRIM` | `TRIM` token enter / leave (mains too high — SmartTrim) |
| `BOOST` / `NOTBOOST` | `BOOST` token enter / leave (mains too low — SmartBoost) |
| `CAL` / `NOTCAL` | runtime calibration enter / leave |
| `OFF` / `NOTOFF` | output `OFF` token enter / leave |
| `ALARM` / `NOTALARM` | `ALARM` token persists past `alarm_debounce` / leaves after a confirmed alarm. `ups.alarm` is captured and exposed as `{{.Alarm}}` |
| `COMMBAD` | TCP loss, `DATA-STALE`, or `DRIVER-NOT-CONNECTED` |
| `COMMOK` | recovery from `COMMBAD` |
| `NOCOMM` | sustained `COMMBAD` past `nocomm_threshold` |

## Running as a service

The service runs as a dedicated, unprivileged system user `ups-client`. systemd does **not** create the account on its own — `make install` lays down the [`init/ups-client.sysusers`](./init/ups-client.sysusers) snippet for you to apply with `systemd-sysusers`.

The repo ships everything you need:

| File | Purpose |
|---|---|
| [`init/ups-client.service.in`](./init/ups-client.service.in) | systemd unit (with `@BINDIR@` substituted at install time) |
| [`init/ups-client.sysusers`](./init/ups-client.sysusers) | declarative `ups-client` system user for `systemd-sysusers` |
| [`ups-client.example.yaml`](./ups-client.example.yaml) | annotated example configuration |

End-to-end install:

```bash
sudo make install                                            # places binary, unit, sysusers snippet, example config
sudo systemd-sysusers                                        # creates the ups-client user/group from the snippet
sudo cp /etc/ups-client/config.example.yaml /etc/ups-client/config.yaml
sudo chown root:ups-client /etc/ups-client/config.yaml
sudo chmod 0640 /etc/ups-client/config.yaml
sudo "$EDITOR" /etc/ups-client/config.yaml                   # replace the placeholders!
sudo /usr/local/bin/ups-client -check -config /etc/ups-client/config.yaml
sudo systemctl daemon-reload
sudo systemctl enable --now ups-client.service
journalctl -u ups-client.service -f
```

`make install` prints a detailed post-install checklist enumerating every placeholder value you must edit before the service can start (ntfy URL, Telegram bot token & chat id, SSH host / user / key path, …). The unit deliberately points at `config.yaml`, not `config.example.yaml`, so the daemon refuses to start until you make the copy.

The unit's hardening is intentionally minimal — only the four directives that do real work for this daemon:

```ini
NoNewPrivileges=yes      # block setuid escalation
ProtectSystem=strict     # read-only rootfs (incl. /etc, /usr)
ProtectHome=yes          # no access to /home, /root
PrivateTmp=yes           # private /tmp namespace
```

Everything else (`Protect{KernelTunables,KernelModules,ControlGroups}`, `Restrict{Namespaces,Realtime}`, `LockPersonality`, `SystemCallFilter`) is omitted on purpose — those directives only block syscalls this daemon never makes.

### FSD shutdown without sudo

The example config ships a shell notifier that runs `systemctl --no-block poweroff` on `FSD`. The `ups-client` system user can't trigger a poweroff by default — logind's polkit policy requires an active local session, which a daemon doesn't have. The repo ships a tiny polkit rule that grants exactly the `power-off` / `halt` actions to the `ups-client` user only:

```bash
sudo install -m 0644 init/ups-client-poweroff.rules \
    /etc/polkit-1/rules.d/50-ups-client-poweroff.rules
```

Polkit picks the rule up immediately — no daemon reload. After this:

```bash
sudo -u ups-client systemctl --no-block poweroff   # would actually power off the box
```

Skip this step if you don't want ups-client to be able to trigger a shutdown — you can drop the `poweroff` block from the shell notifier list, and `FSD` will still trigger every other configured channel (ntfy, Telegram, …).

This approach is preferred over editing `/etc/sudoers.d/` (no sudo dependency, no command injection risk in the sudoers parser) and over a setuid wrapper (no separately-audited binary).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `connect: dial tcp 127.0.0.1:3493: connect: connection refused` | `upsd` not running or not listening on 127.0.0.1; check `LISTEN` in `upsd.conf`. |
| `nut: NUT error: ACCESS-DENIED` | The UPS section requires auth; add `username` + `password` to `nut:`. |
| `nut: NUT error: UNKNOWN-UPS` | `nut.ups` doesn't match the section name in `ups.conf`. |
| Spurious `REPLBATT` | APC BX firmware quirk. Raise `monitor.replbatt_debounce` past the default `600s`, or tighten the driver-side `lbrb_log_delay_sec` in `ups.conf`. See [Tuning the APC-BX flap mitigation](#tuning-the-apc-bx-flap-mitigation). |
| Spurious `ALARM` lasting a few seconds | APC BX firmware quirk — brief background self-tests assert `ALARM`. The default `monitor.alarm_debounce: 60s` suppresses any blip shorter than a minute. Raise it if you still see noise. The actual reason (when confirmed) is exposed as `{{.Alarm}}` in templates. |
| Spurious `LOWBATT` while on mains | Should not happen any more: `LOWBATT` only fires when `LB` *and* `OB` are both set, since a bare `LB` on `OL` has no operational meaning (no shutdown is coming) and APC BX-series firmware asserts spurious `LB`+`RB` during background battery self-tests at full charge. If you genuinely want the bare-`LB` signal on `OL`, watch the `STARTUP`/`ONLINE` events instead and inspect `{{.Vars.ups_status}}` from a shell hook. |
| `DATA-STALE` floods | The driver lost the device, or BX firmware returned a broken HID report length. Check `dmesg` for USB resets and make sure `maxreport = 1` is set in `ups.conf`. |

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
