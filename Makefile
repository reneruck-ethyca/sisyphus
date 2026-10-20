.PHONY: build test install-agent uninstall-agent

LABEL := com.reneruck.waterboy
PLIST := $(HOME)/Library/LaunchAgents/$(LABEL).plist

build:
	go build -o bin/waterboy .

test:
	go test ./...

# PATH is captured from the installing shell so gh/git/wtp resolve under launchd.
install-agent: build
	mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/Library/Logs
	sed -e "s|@BIN@|$(CURDIR)/bin/waterboy|" -e "s|@PATH@|$$PATH|" -e "s|@HOME@|$(HOME)|g" launchagent.plist > $(PLIST)
	launchctl bootout gui/$$(id -u)/$(LABEL) 2>/dev/null || true
	launchctl bootstrap gui/$$(id -u) $(PLIST)

uninstall-agent:
	launchctl bootout gui/$$(id -u)/$(LABEL) 2>/dev/null || true
	rm -f $(PLIST)
