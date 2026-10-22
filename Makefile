.PHONY: build test plist install-agent uninstall-agent reviewer-image start stop restart reset

LABEL := com.reneruck.waterboy
PLIST := $(HOME)/Library/LaunchAgents/$(LABEL).plist

# Match Go's own macOS minimum so cgo objects (Wails) and the linker agree; silences ld warnings.
export CGO_CFLAGS := -O2 -g -mmacosx-version-min=13.0
export CGO_LDFLAGS := -mmacosx-version-min=13.0

# production tag: no devtools/inspector in the window.
build:
	go build -tags production -o bin/waterboy .

test:
	go test ./...

# Renders the LaunchAgent. PATH is captured from the installing shell so gh/git/wtp
# resolve under launchd; plutil sets values literally (no sed escaping pitfalls).
plist:
	mkdir -p $(dir $(PLIST)) $(HOME)/Library/Logs
	cp launchagent.plist $(PLIST)
	plutil -replace ProgramArguments -json '["$(CURDIR)/bin/waterboy"]' $(PLIST)
	plutil -replace EnvironmentVariables.PATH -string "$$PATH" $(PLIST)
	plutil -replace EnvironmentVariables.HOME -string "$(HOME)" $(PLIST)
	plutil -replace StandardOutPath -string "$(HOME)/Library/Logs/waterboy.log" $(PLIST)
	plutil -replace StandardErrorPath -string "$(HOME)/Library/Logs/waterboy.log" $(PLIST)

LOG := $(HOME)/Library/Logs/waterboy.log

# Stops waterboy however it runs (LaunchAgent or by hand) and waits for it to exit.
stop:
	@launchctl bootout gui/$$(id -u)/$(LABEL) 2>/dev/null || true
	@pkill -x waterboy 2>/dev/null || true
	@for i in $$(seq 1 50); do pgrep -x waterboy >/dev/null || exit 0; sleep 0.2; done; \
		echo "waterboy didn't stop" >&2; exit 1

# Starts waterboy: through the LaunchAgent if it's installed, else in the background.
# Either way it logs to $(LOG).
start: build
	@if [ -f $(PLIST) ]; then launchctl bootstrap gui/$$(id -u) $(PLIST) && echo "waterboy started (LaunchAgent)"; \
	else mkdir -p $(dir $(LOG)) && nohup $(CURDIR)/bin/waterboy >> $(LOG) 2>&1 & echo "waterboy started, logging to $(LOG)"; fi

restart: stop start

# Wipes waterboy's state and starts it again: review containers, review history and
# run dirs, the worktrees it created, and what it tracks; the restart rediscovers PRs
# as new. Everything by default; PR='owner/repo#n' for one PR.
reset: build stop
	bin/waterboy -reset '$(or $(PR),all)'
	@$(MAKE) --no-print-directory start

install-agent: build plist
	launchctl bootout gui/$$(id -u)/$(LABEL) 2>/dev/null || true
	launchctl bootstrap gui/$$(id -u) $(PLIST)

uninstall-agent:
	launchctl bootout gui/$$(id -u)/$(LABEL) 2>/dev/null || true
	rm -f $(PLIST)

# Review container for ethyca/fidesplus: the fidesplus devcontainer (built from the
# local checkout, whatever is checked out there) with Claude Code pinned, plus
# reviewer/Dockerfile on top.
CLAUDE_CODE_VERSION := 2.1.306
FIDESPLUS ?= $(HOME)/Dev/fidesplus

reviewer-image:
	docker build --target devcontainer --build-arg CLAUDE_CODE_VERSION=$(CLAUDE_CODE_VERSION) \
		-t waterboy-base:fidesplus $(FIDESPLUS)
	docker build -t waterboy-reviewer:fidesplus reviewer
