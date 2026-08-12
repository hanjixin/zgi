// ZGI agent-runner entry point: embeds real Agent CLIs (Claude Code / Codex)
// via their official SDKs and streams normalized events to the ZGI control
// plane over SSE.
import http from 'node:http';

import { createApp, runs } from './app.js';

const PORT = Number(process.env.AGENT_RUNNER_PORT || 3001);

const server = http.createServer(createApp());
server.listen(PORT, () => {
  console.log(`[agent-runner] listening on :${PORT}`);
});

// Graceful shutdown: abort all active runs.
for (const sig of ['SIGINT', 'SIGTERM'] as const) {
  process.on(sig, () => {
    for (const run of runs.values()) run.controller.abort();
    server.close(() => process.exit(0));
  });
}
