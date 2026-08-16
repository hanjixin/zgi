FROM node:22-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends curl ca-certificates git \
  && rm -rf /var/lib/apt/lists/*

# Codex CLI (statically linked linux x86_64 binary)
RUN curl -fsSL -o /tmp/codex.tar.gz "https://github.com/openai/codex/releases/download/rust-v0.147.0/codex-x86_64-unknown-linux-musl.tar.gz" \
  && tar -xzf /tmp/codex.tar.gz -C /usr/local/bin codex-x86_64-unknown-linux-musl \
  && mv /usr/local/bin/codex-x86_64-unknown-linux-musl /usr/local/bin/codex \
  && chmod +x /usr/local/bin/codex \
  && rm -f /tmp/codex.tar.gz

# Claude Code CLI
RUN npm install -g @anthropic-ai/claude-code

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm install
COPY src ./src
COPY tsconfig.json ./

ENV AGENT_RUNNER_PORT=3101
EXPOSE 3101
CMD ["npm", "start"]
