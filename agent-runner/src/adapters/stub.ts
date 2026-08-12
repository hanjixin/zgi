// Stub adapter — deterministic event sequence for tests and local demos without
// a real Agent CLI / API key. Selected when AGENT_RUNNER_STUB=1.
import type { AdapterDeps, PermissionDecision, RunRequest } from '../protocol.js';

export async function runStub(req: RunRequest, deps: AdapterDeps): Promise<void> {
  const { emit, emitSession, abortController, pending } = deps;

  emitSession(req.sessionId || 'stub-session');
  emit('text', { text: `stub agent received: ${req.prompt}` });
  emit('tool_use', { id: 'stub-tool-1', tool: 'Bash', input: { command: 'echo hi' } });

  // Exercise the interactive approval path deterministically.
  const correlationId = 'stub-corr-1';
  emit('permission_request', {
    correlation_id: correlationId,
    tool: 'Bash',
    input: { command: 'git push origin main' },
    reason: 'stub approval required',
  });
  const decision = await new Promise<PermissionDecision | null>((resolve) => {
    const timer = setTimeout(() => resolve(null), req.askTimeoutMs || 5000);
    const onAbort = (): void => {
      clearTimeout(timer);
      resolve(null);
    };
    abortController.signal.addEventListener('abort', onAbort, { once: true });
    pending.set(correlationId, (value) => {
      clearTimeout(timer);
      abortController.signal.removeEventListener('abort', onAbort);
      resolve(value);
    });
  });
  emit('permission_result', {
    tool: 'Bash',
    denied: !decision || decision.decision !== 'approve',
    reason: decision?.reason || 'timeout/denied',
  });

  emit('tool_result', { id: 'stub-tool-1', tool: 'Bash', output: 'hi', is_error: false });
  emit('done', { subtype: abortController.signal.aborted ? 'cancelled' : 'success', usage: null });
}
