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

test('toSpawnedProcess bridges a resolved handle.exited to the exit event', async () => {
  const handle = fakeHandle({ exited: Promise.resolve(0) });
  const spawned = toSpawnedProcess(handle);

  const exitEvents: Array<[number | null, unknown]> = [];
  spawned.on('exit', (code, signal) => exitEvents.push([code, signal]));

  await new Promise((r) => setImmediate(r));

  assert.deepEqual(exitEvents, [[0, null]]);
  assert.equal(spawned.exitCode, 0);
});

test('toSpawnedProcess bridges a rejected handle.exited to the error event', async () => {
  const boom = new Error('boom');
  const handle = fakeHandle({ exited: Promise.reject(boom) });
  const spawned = toSpawnedProcess(handle);

  const errors: unknown[] = [];
  spawned.on('error', (err) => errors.push(err));

  await new Promise((r) => setImmediate(r));

  assert.equal(errors.length, 1);
  assert.equal(errors[0], boom);
});
