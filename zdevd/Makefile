# zdevd Makefile — Phase 1.
# CONTEXT decisions:
#   D-03: build → /dotfiles/zdevd/bin/{zdevd,zdev-sidebar}; install symlinks
#         into ~/.local/bin and the plist into ~/Library/LaunchAgents.
#   D-05: rename existing ~/.local/bin/zdev-sidebar-render → *-bash on install.
#   D-06: make use-go is the only renderer path (use-bash removed per D4-20).
#   D-14: make test = sub-10s; race detector + plutil lint + anti-fork gate.
#   D-15: make bench-idle = sudo powermetrics, asserts < 1000 wakeups/sec.

# REPO_ROOT resolves at parse time from this Makefile's own location
# (<repo>/zdevd/Makefile), not from a VCS shell-out. `sl root` is not safe to
# probe with: `sl` is an ambiguous command name shared by Sapling's CLI and
# sl(1), the steam-locomotive joke program (`brew install sl`), which exits 0
# and writes a curses animation to stdout — that lands verbatim in REPO_ROOT
# and every path built from it, and its embedded newlines break Makefile
# parsing outright. The layout is fixed anyway, so derive it.
# Override via `make REPO_ROOT=/path/to/repo install` if needed.
_MK_DIR       := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
REPO_ROOT     := $(patsubst %/,%,$(dir $(_MK_DIR)))
ZDEVD_DIR     := $(REPO_ROOT)/zdevd
BIN_DIR       := $(ZDEVD_DIR)/bin
LOCAL_BIN     := $(HOME)/.local/bin
RENDER_LINK   := $(LOCAL_BIN)/zdev-sidebar-render
GO_RENDERER   := $(BIN_DIR)/zdev-sidebar
GO_DAEMON     := $(BIN_DIR)/zdevd
GO_SHOW       := $(BIN_DIR)/zdev-show

# OS detection — controls which service manager (launchd / systemd) the
# install/start/stop/restart/status/logs targets dispatch to. Path layout
# for logs and the socket follow internal/platform's per-OS conventions.
UNAME_S := $(shell uname -s)

ifeq ($(UNAME_S),Darwin)
  # macOS — launchd + Library directories.
  LAUNCH_AGENTS := $(HOME)/Library/LaunchAgents
  LOG_DIR       := $(HOME)/Library/Logs/zdev
  SOCKET_DIR    := $(HOME)/Library/Application Support/zdev
  PLIST_SRC     := $(ZDEVD_DIR)/launchd/com.zdev.zdevd.plist.in
  PLIST_BUILT   := $(BIN_DIR)/com.zdev.zdevd.plist
  PLIST_DST     := $(LAUNCH_AGENTS)/com.zdev.zdevd.plist
  REAPER_PLIST_SRC   := $(ZDEVD_DIR)/launchd/com.zdev.reaper.plist.in
  REAPER_PLIST_BUILT := $(BIN_DIR)/com.zdev.reaper.plist
  REAPER_PLIST_DST   := $(LAUNCH_AGENTS)/com.zdev.reaper.plist
else ifeq ($(UNAME_S),Linux)
  # Linux — systemd user units + XDG directories.
  XDG_STATE_HOME ?= $(HOME)/.local/state
  XDG_RUNTIME_DIR ?= /run/user/$(shell id -u)
  SYSTEMD_USER_DIR := $(HOME)/.config/systemd/user
  LOG_DIR       := $(XDG_STATE_HOME)/zdev
  SOCKET_DIR    := $(XDG_RUNTIME_DIR)/zdev
  ZDEVD_UNIT_SRC     := $(ZDEVD_DIR)/systemd/zdevd.service.in
  ZDEVD_UNIT_BUILT   := $(BIN_DIR)/zdevd.service
  ZDEVD_UNIT_DST     := $(SYSTEMD_USER_DIR)/zdevd.service
  REAPER_UNIT_SRC    := $(ZDEVD_DIR)/systemd/zdev-reaper.service.in
  REAPER_UNIT_BUILT  := $(BIN_DIR)/zdev-reaper.service
  REAPER_UNIT_DST    := $(SYSTEMD_USER_DIR)/zdev-reaper.service
  REAPER_TIMER_SRC   := $(ZDEVD_DIR)/systemd/zdev-reaper.timer.in
  REAPER_TIMER_BUILT := $(BIN_DIR)/zdev-reaper.timer
  REAPER_TIMER_DST   := $(SYSTEMD_USER_DIR)/zdev-reaper.timer
endif

# VERSION derives from the nearest annotated git/sl tag plus dirty-state if any.
# Falls back to "dev" outside a VCS checkout (e.g. tarball install).
# The Sapling leg is gated on a real .sl directory rather than tried blind:
# a bare `sl` on PATH may be sl(1), the steam-locomotive joke program, whose
# animation would otherwise be baked into the binary as its version string.
VERSION       := $(shell git -C "$(REPO_ROOT)" describe --tags --dirty --always 2>/dev/null)
ifeq ($(strip $(VERSION)),)
  ifneq ($(wildcard $(REPO_ROOT)/.sl),)
    VERSION   := $(shell sl log -r . --template "{latesttag}" 2>/dev/null)
  endif
endif
VERSION       := $(if $(strip $(VERSION)),$(strip $(VERSION)),dev)
GOFLAGS       := -trimpath -ldflags="-s -w -X main.version=$(VERSION)"

# CONTEXT D4-16: bench-idle threshold finalized from Phase 4 bench-idle measurement.
# bench-idle on 2026-05-06 showed 0 wakeups/sec at true idle (event-driven daemon).
#
# Tightened 2026-05-11 (quick task 260511-kqz, staff-review PR #1): was 50, now 10.
# With the hub snapshot-equality short-circuit, idle wakeups are bounded by the
# supervisor's 1Hz polls (~2/sec on a single goroutine, batched by Go's runtime
# poller into fewer OS-level wakeups) plus negligible hub event processing. The
# previous 50 setting was 25× looser than the measurement justified — a real
# regression to e.g. 49 wakeups/sec would silently pass. 10 still allows
# powermetrics sampling noise but catches any genuine ticker / busy-loop
# regression. See CUTOVER-LOG.md for the original verdict; this comment for the
# tightening rationale.
BENCH_IDLE_THRESHOLD ?= 10

LABEL  := com.zdev.zdevd
REAPER_LABEL  := com.zdev.reaper
DOMAIN := gui/$(shell id -u)

.PHONY: all build test golden bench-idle bench-workday bench-workday-verdict drift-check live-test install uninstall use-go kill-renderers clean help start stop restart status logs

all: build

help:
	@echo "Targets:"
	@echo "  build           — go build both binaries (-trimpath -ldflags=-s -w)"
	@echo "  install         — build, symlink renderer + daemon + zdev-show, install plist, bootstrap launchd"
	@echo "  uninstall       — bootout, remove symlinks and plist"
	@echo ""
	@echo "  start           — bootstrap launchd (start daemon)"
	@echo "  stop            — bootout launchd (stop daemon)"
	@echo "  restart         — rebuild + kill stale renderers + stop + start (full restart with fresh binary)"
	@echo "  kill-renderers  — SIGTERM all running zdev-sidebar processes (panes close; re-toggle to reopen)"
	@echo "  status          — show daemon pid, uptime, and exit status"
	@echo "  logs            — tail -f daemon logs (stdout + stderr interleaved)"
	@echo ""
	@echo "  test            — plutil-lint + anti-fork gate + go test -race ./..."
	@echo "  golden          — golden-frame parity test (UPDATE=1 to regenerate)"
	@echo "  bench-idle      — sudo powermetrics; asserts < $(BENCH_IDLE_THRESHOLD) wakeups/sec (D4-16 calibrated)"
	@echo "  bench-workday   — sudo powermetrics 8h capture (operator-driven; D4-15)"
	@echo "  bench-workday-verdict — compare two most-recent bench-workday-*.out files; D4-16 gate"
	@echo "  drift-check     — run scripts/drift-check.sh; D4-17 byte-parity vs bash baseline"
	@echo "  live-test       — go test -tags live (kill-9 + 10-renderer drills; D4-06 SC1/SC2)"
	@echo "  use-go          — symlink ~/.local/bin/zdev-sidebar-render → Go binary"
	@echo "  clean           — rm -rf bin/"

ifeq ($(UNAME_S),Darwin)
start:
	@if launchctl print "$(DOMAIN)/$(LABEL)" >/dev/null 2>&1; then \
		echo "start: already running — use 'make restart' to reload"; \
	else \
		launchctl bootstrap "$(DOMAIN)" "$(PLIST_DST)"; \
		echo "start: daemon started"; \
	fi

stop:
	@if launchctl print "$(DOMAIN)/$(LABEL)" >/dev/null 2>&1; then \
		launchctl bootout "$(DOMAIN)/$(LABEL)"; \
		echo "stop: daemon stopped"; \
	else \
		echo "stop: daemon not running"; \
	fi
else
start:
	@if systemctl --user is-active --quiet zdevd.service; then \
		echo "start: already running — use 'make restart' to reload"; \
	else \
		systemctl --user start zdevd.service; \
		echo "start: daemon started"; \
	fi

stop:
	@if systemctl --user is-active --quiet zdevd.service; then \
		systemctl --user stop zdevd.service; \
		echo "stop: daemon stopped"; \
	else \
		echo "stop: daemon not running"; \
	fi
endif

# kill-renderers sends SIGTERM to all running zdev-sidebar processes.
# Each renderer's Layer 2 signal handler restores the cursor and exits cleanly,
# closing its tmux pane. Re-toggle via zdev-sidebar-toggle to reopen with the
# new binary. Called automatically by `make restart` after a fresh build so
# stale binary images do not persist across renderer-affecting code changes.
kill-renderers:
	@N=$$(pgrep -x zdev-sidebar-render 2>/dev/null | wc -l | tr -d ' '); \
		if [ "$$N" != "0" ]; then \
			pkill -TERM -x zdev-sidebar-render 2>/dev/null || true; \
			echo "kill-renderers: sent SIGTERM to $$N renderer(s); re-toggle each pane to reopen"; \
		else \
			echo "kill-renderers: no running zdev-sidebar-render processes found"; \
		fi

ifeq ($(UNAME_S),Darwin)
restart: build kill-renderers
	@# Reuses install-launchd.sh so the bootout-settle-bootstrap sequencing
	@# lives in exactly one place (a fixed sleep here used to race launchd).
	@bash "$(ZDEVD_DIR)/scripts/install-launchd.sh" "$(LABEL)" >/dev/null
	@launchctl list | grep "$(LABEL)" | awk '{printf "restart: pid=%s exit=%s\n", $$1, $$2}'

status:
	@launchctl list | grep "$(LABEL)" | awk '{printf "pid=%s  last-exit=%s\n", $$1, $$2}' \
		|| echo "status: not running"
	@echo "log:    $(LOG_DIR)/zdevd.out.log"
	@echo "errlog: $(LOG_DIR)/zdevd.err.log"
	@if [ -S "$(SOCKET_DIR)/zdevd.sock" ]; then \
		echo "socket: $(SOCKET_DIR)/zdevd.sock  [exists]"; \
	else \
		echo "socket: $(SOCKET_DIR)/zdevd.sock  [missing]"; \
	fi

logs:
	@echo "--- tailing $(LOG_DIR)/zdevd.out.log and zdevd.err.log (Ctrl-C to stop) ---"
	@tail -f "$(LOG_DIR)/zdevd.out.log" "$(LOG_DIR)/zdevd.err.log"
else
restart: build kill-renderers
	@systemctl --user restart zdevd.service
	@sleep 0.5
	@systemctl --user show -p MainPID -p ExecMainStatus --value zdevd.service | paste -sd' ' - | awk '{printf "restart: pid=%s exit=%s\n", $$1, $$2}'

status:
	@systemctl --user is-active --quiet zdevd.service && \
		systemctl --user show -p MainPID -p ExecMainStatus --value zdevd.service | paste -sd' ' - | awk '{printf "pid=%s  last-exit=%s\n", $$1, $$2}' || \
		echo "status: not running"
	@echo "log:    $(LOG_DIR)/zdevd.out.log"
	@echo "errlog: $(LOG_DIR)/zdevd.err.log"
	@if [ -S "$(SOCKET_DIR)/zdevd.sock" ]; then \
		echo "socket: $(SOCKET_DIR)/zdevd.sock  [exists]"; \
	else \
		echo "socket: $(SOCKET_DIR)/zdevd.sock  [missing]"; \
	fi

logs:
	@echo "--- tailing journalctl + $(LOG_DIR)/zdevd.{out,err}.log (Ctrl-C to stop) ---"
	@journalctl --user -u zdevd.service -f & \
		tail -f "$(LOG_DIR)/zdevd.out.log" "$(LOG_DIR)/zdevd.err.log" 2>/dev/null; \
		kill %1 2>/dev/null
endif

build: $(GO_DAEMON) $(GO_RENDERER) $(GO_SHOW)

$(GO_DAEMON): $(shell find cmd/zdevd internal -name '*.go' 2>/dev/null) go.mod
	@mkdir -p "$(BIN_DIR)"
	cd "$(ZDEVD_DIR)" && go build $(GOFLAGS) -o "$(GO_DAEMON)" ./cmd/zdevd

$(GO_RENDERER): $(shell find cmd/zdev-sidebar internal -name '*.go' 2>/dev/null) go.mod
	@mkdir -p "$(BIN_DIR)"
	cd "$(ZDEVD_DIR)" && go build $(GOFLAGS) -o "$(GO_RENDERER)" ./cmd/zdev-sidebar

$(GO_SHOW): $(shell find cmd/zdev-show internal -name '*.go' 2>/dev/null) go.mod
	@mkdir -p "$(BIN_DIR)"
	cd "$(ZDEVD_DIR)" && go build $(GOFLAGS) -o "$(GO_SHOW)" ./cmd/zdev-show

ifeq ($(UNAME_S),Darwin)
# PLIST_BUILT — sed-substituted launchd plist. Generated into bin/ alongside
# the Go binaries. Symlinked into ~/Library/LaunchAgents by `make install`.
# plutil-lint validates the substituted output (real install target),
# not the unsubstituted template. The first sed expression strips the
# template-warning comment block (lines from a bare `<!--` to a bare `-->`)
# so the substituted output is byte-clean for downstream tooling.
$(PLIST_BUILT): $(PLIST_SRC) Makefile
	@mkdir -p "$(BIN_DIR)"
	sed -e '/^<!--$$/,/^-->$$/d' -e 's|@REPO_ROOT@|$(REPO_ROOT)|g' -e 's|@HOME@|$(HOME)|g' "$(PLIST_SRC)" > "$(PLIST_BUILT)"
	plutil -lint "$(PLIST_BUILT)"

# REAPER_PLIST_BUILT — substituted reaper plist. Same pattern as PLIST_BUILT.
$(REAPER_PLIST_BUILT): $(REAPER_PLIST_SRC) Makefile
	@mkdir -p "$(BIN_DIR)"
	sed -e '/^<!--$$/,/^-->$$/d' -e 's|@HOME@|$(HOME)|g' "$(REAPER_PLIST_SRC)" > "$(REAPER_PLIST_BUILT)"
	plutil -lint "$(REAPER_PLIST_BUILT)"
else
# systemd unit substitution — same @REPO_ROOT@ / @HOME@ placeholders.
# `systemd-analyze verify` lints the substituted output when systemd is
# available; otherwise we ship the raw substituted file (`make test` on
# CI runners without an active user bus would otherwise fail).
$(ZDEVD_UNIT_BUILT): $(ZDEVD_UNIT_SRC) Makefile
	@mkdir -p "$(BIN_DIR)"
	sed -e 's|@REPO_ROOT@|$(REPO_ROOT)|g' -e 's|@HOME@|$(HOME)|g' "$(ZDEVD_UNIT_SRC)" > "$(ZDEVD_UNIT_BUILT)"
	@command -v systemd-analyze >/dev/null && systemd-analyze --user verify "$(ZDEVD_UNIT_BUILT)" || true

$(REAPER_UNIT_BUILT): $(REAPER_UNIT_SRC) Makefile
	@mkdir -p "$(BIN_DIR)"
	sed -e 's|@HOME@|$(HOME)|g' "$(REAPER_UNIT_SRC)" > "$(REAPER_UNIT_BUILT)"
	@command -v systemd-analyze >/dev/null && systemd-analyze --user verify "$(REAPER_UNIT_BUILT)" || true

$(REAPER_TIMER_BUILT): $(REAPER_TIMER_SRC) Makefile
	@mkdir -p "$(BIN_DIR)"
	cp "$(REAPER_TIMER_SRC)" "$(REAPER_TIMER_BUILT)"
endif

ifeq ($(UNAME_S),Darwin)
# OPS-08, OPS-09 (the parts that run in pre-push). Sub-10s target per D-14.
test: $(PLIST_BUILT)
	plutil -lint "$(PLIST_BUILT)"
	bash "$(ZDEVD_DIR)/scripts/check-no-daemon-fork.sh"
	bash "$(ZDEVD_DIR)/scripts/check-no-tmux-poll.sh"
	bash "$(ZDEVD_DIR)/scripts/check-no-shell-out.sh"
	bash "$(ZDEVD_DIR)/scripts/check-no-lipgloss-scatter.sh"
	cd "$(ZDEVD_DIR)" && go test -race -count=1 ./...
else
test: $(ZDEVD_UNIT_BUILT)
	@# systemd-analyze is best-effort (CI may lack a user bus); the unit
	@# file is still substituted and present in bin/ for inspection.
	@command -v systemd-analyze >/dev/null && systemd-analyze --user verify "$(ZDEVD_UNIT_BUILT)" || \
		echo "test: systemd-analyze not available; skipping unit-file lint"
	bash "$(ZDEVD_DIR)/scripts/check-no-daemon-fork.sh"
	bash "$(ZDEVD_DIR)/scripts/check-no-tmux-poll.sh"
	bash "$(ZDEVD_DIR)/scripts/check-no-shell-out.sh"
	bash "$(ZDEVD_DIR)/scripts/check-no-lipgloss-scatter.sh"
	cd "$(ZDEVD_DIR)" && go test -race -count=1 ./...
endif

# OPS-09 — golden-frame byte-equality (subset of `make test`, runnable solo).
# Pass UPDATE=1 to regenerate fixtures.
golden:
ifeq ($(UPDATE),1)
	cd "$(ZDEVD_DIR)" && go test -race -count=1 -run Golden ./internal/render/... -update
else
	cd "$(ZDEVD_DIR)" && go test -race -count=1 -run Golden ./internal/render/...
endif

ifeq ($(UNAME_S),Darwin)
# OPS-10 — manual/nightly idle-wakeup benchmark.
# If zdevd is already running (e.g. via launchd) we measure that instance
# directly — the bench cares about idle wakeups of the production daemon, not
# a freshly-spawned one. If nothing is running, spawn a foreground daemon
# bound to the canonical socket for the bench window. Either way, the
# parse-powermetrics.sh awk pattern grabs zdevd's row from each sample.
bench-idle: build
	@mkdir -p "$(LOG_DIR)" "$(SOCKET_DIR)"
	@rm -f /tmp/zdevd-bench.pid
	@if pgrep -x zdevd >/dev/null 2>&1; then \
		echo "bench-idle: existing zdevd detected — measuring it in place"; \
		echo "bench-idle: (for a true idle reading, close sidebar panes first)"; \
	else \
		echo "bench-idle: no zdevd running — starting one in background..."; \
		"$(GO_DAEMON)" & echo $$! > /tmp/zdevd-bench.pid; \
		sleep 5; \
	fi
	@echo "bench-idle: running 60s sudo powermetrics window (sudo prompt incoming)..."
	@bash "$(ZDEVD_DIR)/scripts/parse-powermetrics.sh" > /tmp/zdevd-bench.out || true
	@if [ -f /tmp/zdevd-bench.pid ]; then \
		kill $$(cat /tmp/zdevd-bench.pid) 2>/dev/null || true; \
		rm -f /tmp/zdevd-bench.pid; \
	fi
	@echo "bench-idle: per-sample wakeups (one per line):"
	@cat /tmp/zdevd-bench.out
	@max=$$(awk 'BEGIN{m=0}{ if ($$1+0 > m) m=$$1+0 } END{print m}' /tmp/zdevd-bench.out); \
		echo "bench-idle: max wakeups/sec = $$max (threshold: < $(BENCH_IDLE_THRESHOLD), Go_bench_median * 1.25 per D4-16)"; \
		if [ "$$max" -ge $(BENCH_IDLE_THRESHOLD) ]; then echo "bench-idle: FAIL"; exit 1; fi
	@echo "bench-idle: PASS"

# OPS-07 / D4-15 — 8h workday powermetrics capture.
# Output: $(LOG_DIR)/bench-workday-$(shell date +%F).out (one per day).
# Run alternating days: one bash day, one Go day, then `make bench-workday-verdict`.
# Requires sudo for powermetrics. Unattended: ensure laptop is powered.
# NOTE: bench-workday invokes sudo powermetrics and blocks for ~8h. Do NOT run
# in CI. This is an operator-driven workday benchmark gate per D4-15/D4-16.
bench-workday: build
	@mkdir -p "$(LOG_DIR)"
	@OUTFILE="$(LOG_DIR)/bench-workday-$$(date +%F).out"; \
		echo "bench-workday: writing to $$OUTFILE (8h sudo powermetrics window incoming)"; \
		bash "$(ZDEVD_DIR)/scripts/parse-powermetrics-workday.sh" > "$$OUTFILE" || true; \
		echo "bench-workday: complete — file at $$OUTFILE"; \
		wc -l "$$OUTFILE"

# OPS-07 / D4-16 — verdict gate: >=50% median wakeups/sec reduction.
# Reads the two most-recent bench-workday-*.out (assumes alternating-day cadence).
bench-workday-verdict:
	@bash "$(ZDEVD_DIR)/scripts/parse-workday-verdict.sh"
else
bench-idle bench-workday bench-workday-verdict:
	@echo "$@: powermetrics is macOS-only. On Linux, use one of:"
	@echo "  pidstat -p \"\$$(pgrep -x zdevd)\" 5 12"
	@echo "  perf stat -e context-switches -p \"\$$(pgrep -x zdevd)\" sleep 60"
	@exit 1
endif

# D4-17 — drift detection: ZDEV_SIDEBAR_DETERMINISTIC=1 bash vs Go-self goldens.
# The script lives at scripts/drift-check.sh (created in Plan 04-08).
drift-check:
	@bash "$(ZDEVD_DIR)/scripts/drift-check.sh"

# SC1 + SC2 readiness — kill-9 recovery and 10-pane reconnect drills (Plan 04-06).
# Excluded from default `make test` to keep that gate sub-15s.
live-test:
	cd "$(ZDEVD_DIR)" && go test -tags live -count=1 -timeout 90s ./...

ifeq ($(UNAME_S),Darwin)
install: build $(PLIST_BUILT) $(REAPER_PLIST_BUILT)
	@mkdir -p "$(LOCAL_BIN)" "$(LAUNCH_AGENTS)" "$(LOG_DIR)" "$(SOCKET_DIR)"
	@chmod 700 "$(LOG_DIR)" "$(SOCKET_DIR)"
	@# Symlink Go binaries into PATH.
	ln -sf "$(GO_RENDERER)" "$(RENDER_LINK)"
	ln -sf "$(GO_DAEMON)" "$(LOCAL_BIN)/zdevd"
	ln -sf "$(GO_SHOW)" "$(LOCAL_BIN)/zdev-show"
	@# Symlink substituted plists into LaunchAgents.
	ln -sf "$(PLIST_BUILT)" "$(PLIST_DST)"
	ln -sf "$(REAPER_PLIST_BUILT)" "$(REAPER_PLIST_DST)"
	@# launchctl bootout-then-bootstrap for zdevd (Pitfall B).
	bash "$(ZDEVD_DIR)/scripts/install-launchd.sh"
	@# Reaper job — same script, which owns the bootout-settle-bootstrap race.
	bash "$(ZDEVD_DIR)/scripts/install-launchd.sh" "$(REAPER_LABEL)"
	@echo "install: reaper job bootstrapped (hourly, $${ZDEV_REAP_AFTER_HOURS:-24}h threshold)"
	@# Sapling pre-push hook (Pitfall D).
	bash "$(ZDEVD_DIR)/scripts/install-prepush.sh"
	@echo "install: complete. Open a tmux pane via zdev-sidebar-toggle to verify."

uninstall:
	@launchctl bootout "$(DOMAIN)/$(LABEL)" 2>/dev/null || true
	@launchctl bootout "$(DOMAIN)/$(REAPER_LABEL)" 2>/dev/null || true
	@rm -f "$(PLIST_DST)" "$(REAPER_PLIST_DST)"
	@rm -f "$(RENDER_LINK)" "$(LOCAL_BIN)/zdevd" "$(LOCAL_BIN)/zdev-show"
	@echo "uninstall: complete."
else
install: build $(ZDEVD_UNIT_BUILT) $(REAPER_UNIT_BUILT) $(REAPER_TIMER_BUILT)
	@mkdir -p "$(LOCAL_BIN)" "$(SYSTEMD_USER_DIR)" "$(LOG_DIR)"
	@# SOCKET_DIR is XDG_RUNTIME_DIR/zdev — only mkdir if XDG_RUNTIME_DIR
	@# exists (systemd-logind creates it on login; not present in some CI).
	@if [ -d "$(XDG_RUNTIME_DIR)" ]; then mkdir -p "$(SOCKET_DIR)" && chmod 700 "$(SOCKET_DIR)"; fi
	@chmod 700 "$(LOG_DIR)"
	@# Symlink Go binaries into PATH.
	ln -sf "$(GO_RENDERER)" "$(RENDER_LINK)"
	ln -sf "$(GO_DAEMON)" "$(LOCAL_BIN)/zdevd"
	ln -sf "$(GO_SHOW)" "$(LOCAL_BIN)/zdev-show"
	@# Symlink substituted unit files into ~/.config/systemd/user.
	ln -sf "$(ZDEVD_UNIT_BUILT)" "$(ZDEVD_UNIT_DST)"
	ln -sf "$(REAPER_UNIT_BUILT)" "$(REAPER_UNIT_DST)"
	ln -sf "$(REAPER_TIMER_BUILT)" "$(REAPER_TIMER_DST)"
	@# Reload + enable + start — but only when a systemd user bus exists.
	@# CI runners and bare SSH sessions without lingering have none;
	@# failing the whole install over it would block the build/symlink
	@# steps that DO work everywhere.
	@if systemctl --user show-environment >/dev/null 2>&1; then \
		systemctl --user daemon-reload; \
		systemctl --user enable --now zdevd.service; \
		systemctl --user enable --now zdev-reaper.timer; \
		systemctl --user restart zdevd.service; \
		echo "install: zdevd + reaper.timer enabled and (re)started."; \
	else \
		echo "install: WARNING — no systemd user bus (CI, or SSH without lingering)."; \
		echo "install:           units linked but not started. To run at login:"; \
		echo "install:             loginctl enable-linger $$USER"; \
		echo "install:             systemctl --user enable --now zdevd.service"; \
	fi
	@# Sapling pre-push hook (Pitfall D).
	bash "$(ZDEVD_DIR)/scripts/install-prepush.sh"
	@echo "install: complete. Open a tmux pane via zdev-sidebar-toggle to verify."

uninstall:
	@systemctl --user disable --now zdevd.service 2>/dev/null || true
	@systemctl --user disable --now zdev-reaper.timer 2>/dev/null || true
	@rm -f "$(ZDEVD_UNIT_DST)" "$(REAPER_UNIT_DST)" "$(REAPER_TIMER_DST)"
	@systemctl --user daemon-reload 2>/dev/null || true
	@rm -f "$(RENDER_LINK)" "$(LOCAL_BIN)/zdevd" "$(LOCAL_BIN)/zdev-show"
	@echo "uninstall: complete."
endif

use-go:
	@if [ ! -x "$(GO_RENDERER)" ]; then echo "use-go: $(GO_RENDERER) missing — run 'make build' first" >&2; exit 1; fi
	ln -sf "$(GO_RENDERER)" "$(RENDER_LINK)"
	@echo "use-go: $(RENDER_LINK) → $(GO_RENDERER)"
	@# Existing renderer processes bound to the old symlink survive — terminate
	@# them so their tmux panes close. Re-running zdev-sidebar-toggle picks up
	@# the new symlink. The Go and bash renderers both restore the cursor on
	@# SIGTERM (Pitfall 8), so this is safe.
	@N=$$(pgrep -f 'zdev-sidebar-render$$' 2>/dev/null | wc -l | tr -d ' '); \
		if [ "$$N" != "0" ]; then \
			pkill -TERM -f 'zdev-sidebar-render$$' 2>/dev/null || true; \
			echo "use-go: terminated $$N running renderer(s); re-toggle the pane to see the Go renderer"; \
		fi


clean:
	rm -rf "$(BIN_DIR)"

# demo-gif — regenerate docs/demo.gif: the README's fake-fleet recording
# (idle → working → waiting → dead) rendered by the REAL sidebar against
# `zdevd demo`. Owns the demo-server lifecycle so docs/demo.tape stays a
# pure recording script. Requires vhs (brew install vhs) and an installed
# zdev. Not part of `test` — it is a docs artifact, regenerated on demand.
.PHONY: demo-gif
demo-gif:
	@command -v vhs >/dev/null || { echo "demo-gif: vhs not installed (brew install vhs)" >&2; exit 1; }
	@rm -f /tmp/zdev-demo.sock
	@$(GO_DAEMON) demo --socket /tmp/zdev-demo.sock & \
	  DEMO_PID=$$!; \
	  trap 'kill $$DEMO_PID 2>/dev/null; rm -f /tmp/zdev-demo.sock' EXIT; \
	  sleep 1; \
	  cd $(REPO_ROOT) && vhs docs/demo.tape
