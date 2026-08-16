import assert from 'node:assert/strict';
import http from 'node:http';
import { test } from 'node:test';

import { SandboxClient } from '../src/sandboxClient.js';

function listen(fn: (req: http.IncomingMessage, res: http.ServerResponse) => void): Promise<{ port: number; close: () => Promise<void> }> {
  return new Promise((resolve) => {
    const server = http.createServer(fn);
    server.listen(0, '127.0.0.1', () => {
      const addr = server.address() as { port: number };
      resolve({
        port: addr.port,
        close: () => new Promise((res) => server.close(() => res())),
      });
    });
  });
}

test('SandboxClient.createBox posts to /v1/agent-boxes and parses the envelope', async () => {
  const fake = await listen((req, res) => {
    assert.equal(req.method, 'POST');
    assert.equal(req.url, '/v1/agent-boxes');
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ data: { box_id: 'sbx_1', workspace_path: '/tmp/workspace' } }));
  });
  try {
    const client = new SandboxClient({ baseUrl: `http://127.0.0.1:${fake.port}` });
    const box = await client.createBox({ ttlSeconds: 120, networkEnabled: true, workspaceSeed: { 'CLAUDE.md': '# ZGI' } });
    assert.equal(box.boxId, 'sbx_1');
    assert.equal(box.workspacePath, '/tmp/workspace');
  } finally {
    await fake.close();
  }
});

// ending the stdin handle must relay EOF to the boxed process so CLIs like
// codex (which read their prompt from stdin until EOF) unblock and start.
test('SandboxClient stdin end relays EOF via /stdin/close after flushing writes', async () => {
  const calls: string[] = [];
  let body = '';
  const fake = await listen((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    if (req.url === '/v1/agent-boxes/sbx_1/process' && req.method === 'POST') {
      res.setHeader('Content-Type', 'text/event-stream');
      res.write('data: {"type":"started","pid":"ap_1"}\n\n');
      res.write('data: {"type":"exit","code":0}\n\n');
      res.end();
      return;
    }
    if (req.url?.startsWith('/v1/agent-boxes/sbx_1/process/ap_1/stdin/close')) {
      calls.push(`close:${req.method}`);
      res.end(JSON.stringify({ data: { ok: true } }));
      return;
    }
    if (req.url === '/v1/agent-boxes/sbx_1/process/ap_1/stdin') {
      calls.push('write');
      req.on('data', (c) => (body += c));
      req.on('end', () => res.end(JSON.stringify({ data: { ok: true } })));
      return;
    }
    res.statusCode = 404;
    res.end(JSON.stringify({ error: 'unexpected ' + req.url }));
  });
  try {
    const client = new SandboxClient({ baseUrl: `http://127.0.0.1:${fake.port}` });
    const handle = await client.openProcess('sbx_1', { command: 'codex', args: ['exec'] });
    await new Promise((r) => setTimeout(r, 20)); // let the started frame land so pid is known
    handle.stdin.write(Buffer.from('hi'));
    await new Promise<void>((resolve, reject) => {
      handle.stdin.end(() => resolve());
      handle.stdin.on('error', reject);
    });
    assert.equal(body, 'hi');
    assert.deepEqual(calls, ['write', 'close:POST']);
  } finally {
    await fake.close();
  }
});
