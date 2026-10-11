FROM mcr.microsoft.com/devcontainers/base:trixie

ARG GO_VERSION=1.27.2
ARG NODE_MAJOR=24

RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 xz-utils \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -xz -C /usr/local
ENV PATH=/usr/local/go/bin:/root/go/bin:/home/vscode/go/bin:${PATH}

RUN NODE_VERSION=$(curl -fsSL https://nodejs.org/dist/index.json \
        | python3 -c "import json,sys; print(next(r['version'] for r in json.load(sys.stdin) if r['version'].startswith('v${NODE_MAJOR}.')))") \
    && curl -fsSL "https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-linux-x64.tar.xz" \
        | tar -xJ -C /usr/local --strip-components=1 \
    && corepack enable pnpm
