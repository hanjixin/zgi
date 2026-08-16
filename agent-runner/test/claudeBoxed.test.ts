import assert from 'node:assert/strict';
import { Readable, Writable } from 'node:stream';
import { test } from 'node:test';

import { toSpawnedProcess } from '../src/adapters/claude.js';
import type { ProcessHandle } from '../src/sandboxClient.js';

function fakeHandle(overrides: Partial<ProcessHandle> = {}): ProcessHandle {
  return {
    pid: 'p1',
    stdin: new Writable({ write(_c, _e, cb) { cb(); } }),
    stdout: Readable.from(['out']),
    stderr: Readable.from(['err']),
    exitCode: null,
    exited: Promise.resolve(null),
    kill() {},
    ...overrides,
  };
}

test('toSpawnedProcess exposes the boxed streams and forwards kill', async () => {
  let killed = 0;
  const handle = fakeHandle({ kill: () => { killed += 1; } });
  const spawned = toSpawnedProcess(handle);

  assert.equal(spawned.stdin, handle.stdin);
  assert.equal(spawned.stdout, handle.stdout);
  assert.equal(spawned.killed, false);
  spawned.kill('SIGKILL');
  assert.equal(killed, 1);
  assert.equal(spawned.exitCode, null);

  const exitCodes: Array<number | null> = [];
  spawned.on('exit', (code) => exitCodes.push(code));
  spawned.exitCode = 0;
  spawned.emit('exit', 0, null);
  assert.deepEqual(exitCodes, [0]);
});
