import assert from 'node:assert/strict';
import { PassThrough } from 'node:stream';
import { test } from 'node:test';

import { parseCodexBridgeArgs } from '../src/codexBridge.mjs';

test('parseCodexBridgeArgs strips node and script, keeps exec flags', () => {
  const args = parseCodexBridgeArgs(['node', '/zgi/codexBridge.mjs', 'exec', '--experimental-json', '--cd', '/tmp/workspace']);
  assert.deepEqual(args, ['exec', '--experimental-json', '--cd', '/tmp/workspace']);
});

test('parseCodexBridgeArgs handles a bare exec command', () => {
  assert.deepEqual(parseCodexBridgeArgs(['node', 'bridge', 'exec']), ['exec']);
});
