# sisyphus-base:ops: the base for repos without a devcontainer (kairos, fidesinfra,
# terraform-modules, internal-helm-charts). Node for kairos, terraform/tflint for
# the terraform repos, helm/yq for the charts, and what review runs expect of a
# base: bash, git, gh, uv, sudo for the run user, and Claude Code.
# Versions follow the repos' CI (.nvmrc, tool-versions.yml, setup-helm, tflint).
FROM node:26-trixie-slim

ARG CLAUDE_CODE_VERSION=latest
ARG TERRAFORM_VERSION=1.14.5
ARG TFLINT_VERSION=0.59.1
ARG HELM_VERSION=3.17.0
ARG YQ_VERSION=4.47.2
ARG HADOLINT_VERSION=2.12.0
ARG UV_VERSION=0.9.5
# kairos' packageManager; node 26 ships no corepack.
ARG PNPM_VERSION=10.30.1

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
         bash ca-certificates curl git gh jq just make openssh-client ripgrep fd-find \
         shellcheck shfmt sudo unzip python3 \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

RUN set -eux; arch="$(dpkg --print-architecture)"; \
    case "$arch" in arm64) had=arm64 ;; amd64) had=x86_64 ;; esac; \
    cd /tmp; \
    curl -fsSLo tf.zip "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_${arch}.zip"; \
    curl -fsSLo tfl.zip "https://github.com/terraform-linters/tflint/releases/download/v${TFLINT_VERSION}/tflint_linux_${arch}.zip"; \
    unzip -o tf.zip terraform -d /usr/local/bin; unzip -o tfl.zip tflint -d /usr/local/bin; \
    curl -fsSL "https://get.helm.sh/helm-v${HELM_VERSION}-linux-${arch}.tar.gz" | tar -xz --strip-components=1 -C /usr/local/bin "linux-${arch}/helm"; \
    curl -fsSLo /usr/local/bin/yq "https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_${arch}"; \
    curl -fsSLo /usr/local/bin/hadolint "https://github.com/hadolint/hadolint/releases/download/v${HADOLINT_VERSION}/hadolint-Linux-${had}"; \
    curl -fsSL "https://astral.sh/uv/${UV_VERSION}/install.sh" | env UV_INSTALL_DIR=/usr/local/bin INSTALLER_NO_MODIFY_PATH=1 sh; \
    chmod +x /usr/local/bin/*; rm -f /tmp/*.zip

# node (uid 1000) is the run user: passwordless sudo, as review runs chown their volumes,
# and it owns npm's global dirs, as reviewer/Dockerfile installs into them as that user.
RUN echo "node ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/node && chmod 0440 /etc/sudoers.d/node \
    && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" pnpm@${PNPM_VERSION} && npm cache clean --force \
    && chown -R node:node /usr/local/lib/node_modules /usr/local/bin

USER node
ENV USER=node
