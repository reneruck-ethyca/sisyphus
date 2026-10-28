.PHONY: build test lint plist install-agent uninstall-agent reviewer-image reviewer-plugins start stop restart reset shell

LABEL := com.reneruck.sisyphus
PLIST := $(HOME)/Library/LaunchAgents/$(LABEL).plist

# Match Go's own macOS minimum so cgo objects (Wails) and the linker agree; silences ld warnings.
export CGO_CFLAGS := -O2 -g -mmacosx-version-min=13.0
export CGO_LDFLAGS := -mmacosx-version-min=13.0

# production tag: no devtools/inspector in the window.
build:
	go build -tags production -o bin/sisyphus .

test:
	go test -race ./...

lint:
	golangci-lint run ./...

# Renders the LaunchAgent. PATH is captured from the installing shell so gh/git/wtp
# resolve under launchd; plutil sets values literally (no sed escaping pitfalls).
plist:
	mkdir -p $(dir $(PLIST)) $(HOME)/Library/Logs
	cp launchagent.plist $(PLIST)
	plutil -replace ProgramArguments -json '["$(CURDIR)/bin/sisyphus"]' $(PLIST)
	plutil -replace EnvironmentVariables.PATH -string "$$PATH" $(PLIST)
	plutil -replace EnvironmentVariables.HOME -string "$(HOME)" $(PLIST)
	plutil -replace StandardOutPath -string "$(HOME)/Library/Logs/sisyphus.log" $(PLIST)
	plutil -replace StandardErrorPath -string "$(HOME)/Library/Logs/sisyphus.log" $(PLIST)

LOG := $(HOME)/Library/Logs/sisyphus.log

# Stops sisyphus however it runs (LaunchAgent or by hand) and waits for it to exit.
stop:
	@launchctl bootout gui/$$(id -u)/$(LABEL) 2>/dev/null || true
	@pkill -x sisyphus 2>/dev/null || true
	@for i in $$(seq 1 50); do pgrep -x sisyphus >/dev/null || exit 0; sleep 0.2; done; \
		echo "sisyphus didn't stop" >&2; exit 1

# Starts this checkout's build (a worktree's too): through the LaunchAgent if it's
# installed, re-rendered to point here, else in the background. Either way it logs
# to $(LOG). A running sisyphus is stopped first; launchd only reads the plist on bootstrap.
start: build stop
	@if [ -f $(PLIST) ]; then \
		$(MAKE) --no-print-directory -s plist && \
		launchctl bootstrap gui/$$(id -u) $(PLIST) && \
		for i in $$(seq 1 25); do pgrep -x sisyphus >/dev/null && break; sleep 0.2; done; \
		pgrep -x sisyphus >/dev/null && echo "sisyphus started (LaunchAgent): $(CURDIR)/bin/sisyphus" || \
			{ echo "sisyphus didn't start; see $(LOG) and: launchctl print gui/$$(id -u)/$(LABEL)" >&2; exit 1; }; \
	else mkdir -p $(dir $(LOG)) && nohup $(CURDIR)/bin/sisyphus >> $(LOG) 2>&1 & echo "sisyphus started, logging to $(LOG)"; fi

restart: stop start

# Wipes sisyphus's state and starts it again: review containers, review history and
# run dirs, the worktrees it created, and what it tracks; the restart rediscovers PRs
# as new. Everything by default; PR='owner/repo#n' for one PR.
reset: build stop
	bin/sisyphus -reset '$(or $(PR),all)'
	@$(MAKE) --no-print-directory start

# A shell in the container a review of PR='owner/repo#n' would run in, with its
# stack up; next to the running app.
shell: build
	@test -n '$(PR)' || { echo "usage: make shell PR='owner/repo#n'" >&2; exit 1; }
	bin/sisyphus -shell '$(PR)'

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

reviewer-image: reviewer-plugins
	docker build --target devcontainer --build-arg CLAUDE_CODE_VERSION=$(CLAUDE_CODE_VERSION) \
		-t sisyphus-base:fidesplus $(FIDESPLUS)
	docker build -t sisyphus-reviewer:fidesplus reviewer

# Plugins for the reviewer image, copied from this machine's Claude Code install
# (talos is private: no clone needed) into reviewer/staged/. Hooks are left out:
# talos's would self-update and steer to tools the container doesn't have, and
# superpowers' session-start prompt is for interactive sessions. USER_SKILLS from
# ~/.claude/skills become the user-skills plugin.
STAGE := reviewer/staged
PLUGINS := ethyca-talos@ethyca-private superpowers@claude-plugins-official
USER_SKILLS := adversarial-review

reviewer-plugins:
	rm -rf $(STAGE) && mkdir -p $(STAGE)/user-skills/.claude-plugin $(STAGE)/user-skills/skills
	@for p in $(PLUGINS); do \
		src=$$(jq -er --arg p $$p '.plugins[$$p][0].installPath' $(HOME)/.claude/plugins/installed_plugins.json) || \
			{ echo "$$p isn't installed" >&2; exit 1; }; \
		rsync -a --exclude hooks --exclude tests --exclude .git --exclude .in_use "$$src/" $(STAGE)/$${p%@*}/ && echo "staged $$p from $$src"; \
	done
	echo '{"name": "user-skills", "description": "skills from ~/.claude/skills"}' > $(STAGE)/user-skills/.claude-plugin/plugin.json
	for s in $(USER_SKILLS); do cp -R $(HOME)/.claude/skills/$$s $(STAGE)/user-skills/skills/; done
