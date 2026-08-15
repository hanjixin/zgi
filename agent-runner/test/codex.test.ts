import assert from 'node:assert/strict';
import { test } from 'node:test';

import { buildGatewayProviderConfig, buildMcpServersConfig } from '../src/adapters/codex.js';

// Regression: Codex's `--config` parser rejects an inline-table RHS
// (`mcp_servers.X.http_headers={ Authorization = "…" }`) as a string and aborts
// at startup. The @openai/codex-sdk flattens nested plain objects into dotted
// paths (`mcp_servers.X.http_headers.Authorization="…"`), so the map fields
// MUST stay plain objects here — never a pre-serialized table string.

test('codex mcp config: http servers get url + dotted http_headers map', () => {
  const out = buildMcpServersConfig([
    {
      name: 'zgi-tools',
      type: 'http',
      url: 'http://zgi/mcp',
      headers: { Authorization: 'Bearer tok', 'X-Zgi-Session': 's1' },
    },
  ]);
  assert.deepEqual(out, {
    'zgi-tools': {
      url: 'http://zgi/mcp',
      http_headers: { Authorization: 'Bearer tok', 'X-Zgi-Session': 's1' },
    },
  });
});

test('codex mcp config: stdio servers get command/args/env map', () => {
  const out = buildMcpServersConfig([
    { name: 'local', type: 'stdio', command: 'npx', args: ['-y', 'x'], env: { LOG_LEVEL: 'debug' } },
  ]);
  assert.deepEqual(out, {
    local: { command: 'npx', args: ['-y', 'x'], env: { LOG_LEVEL: 'debug' } },
  });
});

test('codex mcp config: http servers drop env (rejected on streamable_http)', () => {
  const out = buildMcpServersConfig([
    { name: 'zgi-tools', type: 'http', url: 'http://zgi/mcp', env: { SECRET: 'x' } },
  ]);
  assert.deepEqual(out, { 'zgi-tools': { url: 'http://zgi/mcp' } });
});

test('codex gateway: model provider points at gateway /v1 responses', () => {
  const cfg = buildGatewayProviderConfig('http://127.0.0.1:2670/');
  assert.deepEqual(cfg, {
    zgi: {
      name: 'ZGI LLM Gateway',
      base_url: 'http://127.0.0.1:2670/v1',
      wire_api: 'responses',
      env_key: 'OPENAI_API_KEY',
    },
  });
});

test('codex mcp config: servers without url/command are omitted', () => {
  const out = buildMcpServersConfig([
    { name: 'bad', type: 'http' },
    { name: 'no-command', type: 'stdio' },
  ]);
  assert.deepEqual(out, {});
});
