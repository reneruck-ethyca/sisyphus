# waterboy-reviewer: the repo's own devcontainer image (built as BASE by
# `make reviewer-image`) plus what headless review runs need. waterboy supplies
# the command, mounts and env at run time.
ARG BASE=waterboy-base:fidesplus
FROM ${BASE}

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
    sh -c 'uv tool install semgrep==1.182.1 && uv tool install mcp-atlassian==0.23.1'

# waterboy loads every plugin under /opt/waterboy/plugins and the MCP servers in
# /opt/waterboy/mcp.json (see claudeScript). staged/ is `make reviewer-plugins`.
COPY plugins/ staged/ /opt/waterboy/plugins/
COPY mcp.json /opt/waterboy/mcp.json

# Claude refuses to skip permission prompts as root; the devcontainer user isn't.
USER fidesuser

# Language servers for plugins/lsp. typescript 5: typescript-language-server
# drives tsserver, which TypeScript 7 no longer ships.
RUN npm install -g pyright@1.1.414 typescript-language-server@6.0.2 typescript@5.9.3
