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
