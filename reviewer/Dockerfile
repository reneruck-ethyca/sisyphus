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

# Claude refuses to skip permission prompts as root; the devcontainer user isn't.
USER fidesuser
