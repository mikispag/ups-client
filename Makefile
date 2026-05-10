# ups-client — build, test, and install targets.

BINARY  := ups-client
PKG     := ./...
GOFLAGS := -trimpath
LDFLAGS := -s -w

# Install layout. Override on the command line, e.g.:
#   sudo make install PREFIX=/usr SYSCONFDIR=/etc
# Use DESTDIR for staged installs (deb/rpm packagers).
PREFIX      ?= /usr/local
DESTDIR     ?=
BINDIR      ?= $(PREFIX)/bin
SYSCONFDIR  ?= /etc
UNITDIR     ?= $(SYSCONFDIR)/systemd/system
SYSUSERSDIR ?= $(SYSCONFDIR)/sysusers.d

INST_BIN     := $(DESTDIR)$(BINDIR)
INST_CONF    := $(DESTDIR)$(SYSCONFDIR)/ups-client
INST_UNIT    := $(DESTDIR)$(UNITDIR)
INST_SYSUSER := $(DESTDIR)$(SYSUSERSDIR)

.PHONY: all build install install-bin install-config install-systemd uninstall \
        test test-race cover vet check tidy clean help

## all: housekeeping pass — tidy + vet + race tests + build (the default)
all: tidy vet test-race build

## build: compile the static binary into ./bin/$(BINARY)
build:
	@mkdir -p bin
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY) .

## install: install binary, systemd unit, sysusers snippet, and example config
install: build install-bin install-systemd install-config
	@$(MAKE) -s install-postinstall-warning

install-bin:
	install -d $(INST_BIN)
	install -m 0755 bin/$(BINARY) $(INST_BIN)/$(BINARY)

install-systemd:
	install -d $(INST_UNIT)
	sed 's,@BINDIR@,$(BINDIR),g' init/ups-client.service.in > $(INST_UNIT)/ups-client.service
	chmod 0644 $(INST_UNIT)/ups-client.service
	install -d $(INST_SYSUSER)
	install -m 0644 init/ups-client.sysusers $(INST_SYSUSER)/ups-client.conf

install-config:
	install -d $(INST_CONF)
	install -m 0644 ups-client.example.yaml $(INST_CONF)/config.example.yaml

# Internal: emits the post-install checklist. Kept separate so it doesn't
# fire on individual sub-targets.
install-postinstall-warning:
	@printf '\n'
	@printf '════════════════════════════════════════════════════════════════════\n'
	@printf '  ups-client installed.\n'
	@printf '════════════════════════════════════════════════════════════════════\n'
	@printf '  Files placed:\n'
	@printf '    %s\n' '$(INST_BIN)/$(BINARY)'
	@printf '    %s\n' '$(INST_UNIT)/ups-client.service'
	@printf '    %s\n' '$(INST_SYSUSER)/ups-client.conf'
	@printf '    %s\n' '$(INST_CONF)/config.example.yaml'
	@printf '\n'
	@printf '  ⚠  ACTION REQUIRED before starting the service:\n'
	@printf '\n'
	@printf '  1. Create the system user (declarative; survives reinstalls):\n'
	@printf '       sudo systemd-sysusers\n'
	@printf '\n'
	@printf '  2. Copy the example config and edit it:\n'
	@printf '       sudo cp $(SYSCONFDIR)/ups-client/config.example.yaml \\\n'
	@printf '              $(SYSCONFDIR)/ups-client/config.yaml\n'
	@printf '       sudo chown root:ups-client $(SYSCONFDIR)/ups-client/config.yaml\n'
	@printf '       sudo chmod 0640 $(SYSCONFDIR)/ups-client/config.yaml\n'
	@printf '       sudo $${EDITOR:-vi} $(SYSCONFDIR)/ups-client/config.yaml\n'
	@printf '\n'
	@printf '     Replace these PLACEHOLDER values (and delete any block you do\n'
	@printf '     not need — the config validator only requires the keys that\n'
	@printf '     are present):\n'
	@printf '\n'
	@printf '       nut.ups                              -> the section name from\n'
	@printf '                                                /etc/nut/ups.conf\n'
	@printf '       webhook[name=ntfy].url               -> https://ntfy.sh/<your-private-topic>\n'
	@printf '                                                or your self-hosted ntfy URL\n'
	@printf '       webhook[name=home-assistant].url     -> remove the block, or set to\n'
	@printf '                                                https://<ha-host>/api/webhook/<id>\n'
	@printf '       ssh[name=nas].host / .user           -> your real SSH target\n'
	@printf '       ssh[name=nas].private_key_file       -> a key readable by ups-client\n'
	@printf '                                                (e.g. /etc/ups-client/id_ed25519,\n'
	@printf '                                                 chown root:ups-client, chmod 0640)\n'
	@printf '       ssh[name=nas].known_hosts_file       -> populate with: ssh-keyscan -H <host>\n'
	@printf '       telegram[name=ops].bot_token         -> token from @BotFather\n'
	@printf '       telegram[name=ops].chat_id           -> destination chat id\n'
	@printf '\n'
	@printf '  3. Validate the config (no upsd connection required):\n'
	@printf '       $(BINDIR)/$(BINARY) -check -config $(SYSCONFDIR)/ups-client/config.yaml\n'
	@printf '\n'
	@printf '  4. Enable and start the service:\n'
	@printf '       sudo systemctl daemon-reload\n'
	@printf '       sudo systemctl enable --now ups-client.service\n'
	@printf '       journalctl -u ups-client.service -f\n'
	@printf '\n'
	@printf '  ⚠  The installed example contains placeholder ntfy/Telegram/SSH\n'
	@printf '     targets. The service unit looks for config.yaml (NOT\n'
	@printf '     config.example.yaml), so it refuses to start until you do step 2\n'
	@printf '     — this prevents UPS events from being fanned out to whoever owns\n'
	@printf '     those public placeholders.\n'
	@printf '════════════════════════════════════════════════════════════════════\n'
	@printf '\n'

## uninstall: remove files placed by `make install` (keeps config.yaml + the user)
uninstall:
	rm -f $(INST_BIN)/$(BINARY)
	rm -f $(INST_UNIT)/ups-client.service
	rm -f $(INST_SYSUSER)/ups-client.conf
	rm -f $(INST_CONF)/config.example.yaml
	@printf '\n  Removed binary, unit, sysusers snippet, and config.example.yaml.\n'
	@printf '  Left intact:\n'
	@printf '    %s (if any) — your edited config\n' '$(INST_CONF)/config.yaml'
	@printf '    the ups-client system user — drop /etc/sysusers.d/ups-client.conf\n'
	@printf '    if you also want it gone, then `userdel ups-client`.\n\n'

## test: run unit tests
test:
	go test $(PKG)

## test-race: run unit tests with the race detector
test-race:
	go test -race $(PKG)

## cover: run tests with coverage; writes coverage.out and prints summary
cover:
	go test -race -covermode=atomic -coverprofile=coverage.out $(PKG)
	@go tool cover -func=coverage.out | tail -n 1

## vet: run go vet
vet:
	go vet $(PKG)

## check: vet + race tests (what CI runs)
check: vet test-race

## tidy: tidy go.mod / go.sum
tidy:
	go mod tidy

## clean: remove build artifacts
clean:
	rm -rf bin coverage.out

## help: list available targets
help:
	@awk 'BEGIN {FS = ":.*?## "} /^## / { sub(/^## /, "", $$0); split($$0, a, ": "); printf "  %-12s %s\n", a[1], a[2] }' $(MAKEFILE_LIST)
