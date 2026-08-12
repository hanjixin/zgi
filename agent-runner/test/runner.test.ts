import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import type { Server } from 'node:http';

import { createApp, runs } from '../src/app.js';
import { parseRunRequest } from '../src/protocol.js';

let server: Server;
let baseUrl: string;

function parseSSE(body: string): Array<Record<string, unknown>> {
  return body
    .split('\n\n')
    .filter((line) => line.startsWith('data: '))
    .map((line) => JSON.parse(line.slice(6)));
}

before(async () => {
  process.env.AGENT_RUNNER_STUB = '1';
  server = createApp().listen(0);
  await new Promise<void>((resolve) => server.once('listening', resolve));
  const addr = server.address();
  assert.ok(addr && typeof addr === 'object');
  baseUrl = `http://127.0.0.1:${addr.port}`;
});

after(() => {
  server.close();
  runs.clear();
});

describe('parseRunRequest', () => {
  it('accepts a valid claude request', () => {
    const req = parseRunRequest({ agent_type: 'claude', prompt: 'hi', session_id: 's1', cwd: '/tmp', env: { ANTHROPIC_API_KEY: 'k' } });
    assert.equal(req.agentType, 'claude');
    assert.equal(req.prompt, 'hi');
    assert.equal(req.sessionId, 's1');
    assert.equal(req.env.ANTHROPIC_API_KEY, 'k');
  });

  it('parses system_prompt and mcp_servers', () => {
    const req = parseRunRequest({
      agent_type: 'claude',
      prompt: 'hi',
      system_prompt: 'You are a senior engineer.',
      mcp_servers: [
        { name: 'zgi-tools', type: 'http', url: 'http://zgi-api/mcp', headers: { Authorization: 'Bearer x' } },
        { name: 'local', type: 'stdio', command: 'npx', args: ['-y', 'some-mcp'] },
      ],
    });
    assert.equal(req.systemPrompt, 'You are a senior engineer.');
    assert.equal(req.mcpServers?.length, 2);
    assert.equal(req.mcpServers?.[0].name, 'zgi-tools');
    assert.equal(req.mcpServers?.[0].type, 'http');
    assert.equal(req.mcpServers?.[0].url, 'http://zgi-api/mcp');
    assert.deepEqual(req.mcpServers?.[1].args, ['-y', 'some-mcp']);
  });

  it('rejects an unknown agent_type', () => {
    assert.throws(() => parseRunRequest({ agent_type: 'foo', prompt: 'x' }), /unsupported agent_type/);
  });

  it('rejects an empty prompt', () => {
    assert.throws(() => parseRunRequest({ agent_type: 'codex', prompt: '' }), /prompt is required/);
  });
});

describe('HTTP API', () => {
  it('reports health', async () => {
    const res = await fetch(`${baseUrl}/health`);
    assert.equal(res.status, 200);
    assert.deepEqual(await res.json(), { status: 'ok' });
  });

  it('rejects an invalid run body with 400', async () => {
    const res = await fetch(`${baseUrl}/v1/agents/run`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ agent_type: 'nope', prompt: 'x' }),
    });
    assert.equal(res.status, 400);
  });

  it('streams the normalized event sequence for a run', async () => {
    const res = await fetch(`${baseUrl}/v1/agents/run`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ agent_type: 'claude', prompt: 'do a thing', session_id: 'seq-1', cwd: '/tmp', ask_timeout_ms: 2000 }),
    });
    assert.equal(res.status, 200);
    assert.match(res.headers.get('content-type') || '', /text\/event-stream/);
    const body = await res.text();
    const events = parseSSE(body);
    const types = events.map((e) => e.type);
    assert.ok(types.includes('session_started'), `missing session_started: ${types}`);
    assert.ok(types.includes('text'));
    assert.ok(types.includes('tool_use'));
    assert.ok(types.includes('permission_request'));
    assert.ok(types.includes('permission_result'));
    assert.ok(types.includes('done'));
    const started = events.find((e) => e.type === 'session_started');
    assert.equal(started?.session_id, 'seq-1');
  });

  it('rejects a duplicate active run for the same session', async () => {
    // A long ask timeout keeps the stub run active (waiting on permission) so a
    // second request for the same session id must be rejected with 409.
    const body = JSON.stringify({ agent_type: 'claude', prompt: 'x', session_id: 'dup-1', cwd: '/tmp', ask_timeout_ms: 20000 });
    const first = await fetch(`${baseUrl}/v1/agents/run`, { method: 'POST', headers: { 'content-type': 'application/json' }, body });
    await new Promise((r) => setTimeout(r, 200));
    const second = await fetch(`${baseUrl}/v1/agents/run`, { method: 'POST', headers: { 'content-type': 'application/json' }, body });
    assert.equal(second.status, 409);
    // Stop the first run and drain its stream so the registry is cleaned up.
    await fetch(`${baseUrl}/v1/agents/dup-1/stop`, { method: 'POST' });
    await first.text();
  });

  it('resolves a pending approval via the permission endpoint', async () => {
    const res = await fetch(`${baseUrl}/v1/agents/run`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ agent_type: 'claude', prompt: 'x', session_id: 'perm-1', cwd: '/tmp', ask_timeout_ms: 5000 }),
    });
    const bodyPromise = res.text();
    // Wait for the stub to emit the permission_request, then approve it.
    await new Promise((r) => setTimeout(r, 300));
    const perm = await fetch(`${baseUrl}/v1/agents/perm-1/permission`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ correlation_id: 'stub-corr-1', decision: 'approve', reason: 'ok' }),
    });
    assert.equal(perm.status, 200);
    const body = await bodyPromise;
    const events = parseSSE(body);
    const permResult = events.find((e) => e.type === 'permission_result');
    assert.equal(permResult?.denied, false);
  });

  it('returns 404 for stop on an unknown session', async () => {
    const res = await fetch(`${baseUrl}/v1/agents/unknown-xyz/stop`, { method: 'POST' });
    assert.equal(res.status, 404);
  });

  it('stops a running agent', async () => {
    const res = await fetch(`${baseUrl}/v1/agents/run`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ agent_type: 'claude', prompt: 'x', session_id: 'stop-1', cwd: '/tmp', ask_timeout_ms: 5000 }),
    });
    const bodyPromise = res.text();
    await new Promise((r) => setTimeout(r, 200));
    const stop = await fetch(`${baseUrl}/v1/agents/stop-1/stop`, { method: 'POST' });
    assert.equal(stop.status, 200);
    const body = await bodyPromise;
    const events = parseSSE(body);
    const done = events.find((e) => e.type === 'done');
    assert.equal(done?.subtype, 'cancelled');
  });
});
