// Express app for the agent-runner. Kept separate from server.ts so tests can
// bind it to an ephemeral port. See src/server.ts for the entry point.
import { randomUUID } from 'node:crypto';

import express, { type Express, type Request, type Response } from 'express';

import { ev, parseRunRequest, sse, type PermissionDecision } from './protocol.js';
import { SandboxClient } from './sandboxClient.js';
import { BoxManager } from './boxManager.js';
import { runClaude } from './adapters/claude.js';
import { runCodex } from './adapters/codex.js';
import { runStub } from './adapters/stub.js';

export interface ActiveRun {
  controller: AbortController;
  pending: Map<string, (value: PermissionDecision | null) => void>;
  agentType: string;
}

// Active runs keyed by the control-plane session id.
export const runs = new Map<string, ActiveRun>();

// Per-session sandbox box registry; created lazily on the first sandboxed run
// and swept on server shutdown. Server exits the process, so a single registry
// (pointing at the shared zgi-sandbox) is sufficient.
export let boxManager: BoxManager | undefined;

function makeRunKey(sessionId: string): string {
  return sessionId || `run-${randomUUID()}`;
}

function setSSEHeaders(res: Response): void {
  res.setHeader('Content-Type', 'text/event-stream');
  res.setHeader('Cache-Control', 'no-cache, no-transform');
  res.setHeader('Connection', 'keep-alive');
  res.setHeader('X-Accel-Buffering', 'no');
  res.flushHeaders();
}

export function createApp(): Express {
  const app = express();
  app.use(express.json({ limit: '2mb' }));

  // ---- POST /v1/agents/run → SSE stream of normalized events ----
  app.post('/v1/agents/run', async (req: Request, res: Response) => {
    let runReq;
    try {
      runReq = parseRunRequest(req.body);
    } catch (err) {
      res.status(400).json({ code: -400, message: err instanceof Error ? err.message : String(err) });
      return;
    }

    // The Agent CLIs need a functional host environment (PATH, HOME, ...) to run
    // bash/python/git, so injected keys are merged on top of process.env rather
    // than replacing it.
    runReq.env = { ...(process.env as Record<string, string>), ...runReq.env };

    if (runReq.sandbox) {
      const client = new SandboxClient({ baseUrl: runReq.sandbox.url, apiKey: runReq.sandbox.api_key });
      boxManager = new BoxManager(client);
    }

    const runKey = makeRunKey(runReq.sessionId);
    if (runs.has(runKey)) {
      res.status(409).json({ code: -409, message: `run already active for session ${runKey}` });
      return;
    }

    const controller = new AbortController();
    const pending = new Map<string, (value: PermissionDecision | null) => void>();
    const run: ActiveRun = { controller, pending, agentType: runReq.agentType };
    runs.set(runKey, run);

    setSSEHeaders(res);

    const cleanup = (): void => {
      if (runs.get(runKey) === run) runs.delete(runKey);
      for (const [, resolve] of pending) resolve(null);
      pending.clear();
    };
    res.on('close', () => {
      controller.abort();
      cleanup();
    });

    let sessionEmitted = false;
    const emitSession = (agentSessionId: string | null): void => {
      if (sessionEmitted) return;
      sessionEmitted = true;
      res.write(sse(ev('session_started', { session_id: runKey, agent_session_id: agentSessionId })));
    };

    const emit = (type: string, payload: Record<string, unknown> = {}): void => {
      if (res.writableEnded) return;
      res.write(sse(ev(type, payload)));
    };

    const stubMode = process.env.AGENT_RUNNER_STUB === '1';
    const adapter = stubMode ? runStub : runReq.agentType === 'claude' ? runClaude : runCodex;
    try {
      await adapter(runReq, { emit, emitSession, abortController: controller, pending, boxManager });
    } catch (err) {
      emit('error', { message: err instanceof Error ? err.message : String(err) });
    }
    if (!res.writableEnded) res.end();
  });

  // ---- POST /v1/agents/:session_id/stop — abort a running agent ----
  app.post('/v1/agents/:session_id/stop', (req: Request, res: Response) => {
    const sessionId = String(req.params.session_id);
    const run = runs.get(sessionId);
    if (!run) {
      res.status(404).json({ code: -404, message: 'run not found' });
      return;
    }
    run.controller.abort();
    res.json({ code: 0, message: 'stopping' });
  });

  // ---- POST /v1/agents/:session_id/permission — resolve a pending approval ----
  app.post('/v1/agents/:session_id/permission', (req: Request, res: Response) => {
    const sessionId = String(req.params.session_id);
    const run = runs.get(sessionId);
    if (!run) {
      res.status(404).json({ code: -404, message: 'run not found' });
      return;
    }
    const correlationId = String(req.body?.correlation_id || '');
    const decision = String(req.body?.decision || '');
    if (!correlationId || (decision !== 'approve' && decision !== 'reject')) {
      res.status(400).json({ code: -400, message: 'correlation_id and decision (approve|reject) are required' });
      return;
    }
    const resolve = run.pending.get(correlationId);
    if (!resolve) {
      res.status(404).json({ code: -404, message: 'no pending approval for correlation_id' });
      return;
    }
    resolve({ decision: decision as PermissionDecision['decision'], reason: String(req.body?.reason || '') });
    res.json({ code: 0, message: 'resolved' });
  });

  // ---- health ----
  app.get('/health', (_req: Request, res: Response) => res.json({ status: 'ok' }));

  return app;
}
