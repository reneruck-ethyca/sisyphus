# sisyphus-reviewer: the repo's own devcontainer image (built as BASE by
# `make reviewer-image`) plus what headless review runs need. sisyphus supplies
# the command, mounts and env at run time.
ARG BASE=sisyphus-base:fidesplus
FROM ${BASE}
# The base's non-root user: fidesuser in the fidesplus devcontainer, node in ops.Dockerfile.
ARG RUN_USER=fidesuser

# Pinned Claude Code (installed in the base); no background updates or
# nonessential traffic from inside review containers.
ENV DISABLE_AUTOUPDATER=1 \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

# semgrep for talos's policy-check; mcp-atlassian is the Jira/Confluence MCP
# server talos skills call (mcp.json).
USER root
RUN UV_TOOL_DIR=/opt/uv-tools UV_TOOL_BIN_DIR=/usr/local/bin \
    sh -c 'uv tool install semgrep==1.182.1 && uv tool install mcp-atlassian==0.23.1' \
    && npm install -g @openai/codex

# Headless Chromium for UI review, driven by the Playwright MCP server (mcp.json).
# Browsers live outside any home dir so the run user can read them; the install
# uses the playwright the MCP package pins, so the MCP finds the build it expects.
# The path is set here and in mcp.json only, not image-wide, so other Playwright
# installs in the container (fides-qa's cardea) keep their own.
RUN export PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright \
    && npm install -g @playwright/mcp@0.0.86 \
    && node "$(npm root -g)/@playwright/mcp/node_modules/playwright/cli.js" install --no-shell --with-deps chromium \
    && chmod -R a+rX /opt/ms-playwright \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/*

# sisyphus loads every plugin under /opt/sisyphus/plugins and the MCP servers in
# /opt/sisyphus/mcp.json (see claudeScript). staged/ is `make reviewer-plugins`.
COPY plugins/ staged/ /opt/sisyphus/plugins/
COPY mcp.json /opt/sisyphus/mcp.json

# Claude refuses to skip permission prompts as root; the base's user isn't.
USER ${RUN_USER}

# Language servers for plugins/lsp. typescript 5: typescript-language-server
# drives tsserver, which TypeScript 7 no longer ships.
RUN npm install -g pyright@1.1.414 typescript-language-server@6.0.2 typescript@5.9.3
