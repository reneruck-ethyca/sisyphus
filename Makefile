.PHONY: build test plist install-agent uninstall-agent

LABEL := com.reneruck.waterboy
PLIST := $(HOME)/Library/LaunchAgents/$(LABEL).plist

build:
	go build -o bin/waterboy .

test:
	go test ./...

# Renders the LaunchAgent. PATH is captured from the installing shell so gh/git/wtp
# resolve under launchd; plutil sets values literally (no sed escaping pitfalls).
plist:
	mkdir -p $(dir $(PLIST)) $(HOME)/Library/Logs
	cp launchagent.plist $(PLIST)
	plutil -replace ProgramArguments.0 -string "$(CURDIR)/bin/waterboy" $(PLIST)
	plutil -replace EnvironmentVariables.PATH -string "$$PATH" $(PLIST)
	plutil -replace EnvironmentVariables.HOME -string "$(HOME)" $(PLIST)
	plutil -replace StandardOutPath -string "$(HOME)/Library/Logs/waterboy.log" $(PLIST)
	plutil -replace StandardErrorPath -string "$(HOME)/Library/Logs/waterboy.log" $(PLIST)

install-agent: build plist
	launchctl bootout gui/$$(id -u)/$(LABEL) 2>/dev/null || true
	launchctl bootstrap gui/$$(id -u) $(PLIST)

uninstall-agent:
	launchctl bootout gui/$$(id -u)/$(LABEL) 2>/dev/null || true
	rm -f $(PLIST)
