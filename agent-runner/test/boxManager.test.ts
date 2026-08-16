import assert from 'node:assert/strict';
import { test } from 'node:test';

import { BoxManager } from '../src/boxManager.js';
import type { SandboxClient } from '../src/sandboxClient.js';

class FakeClient {
  boxes: string[] = [];
  created = 0;
  async createBox(req: unknown): Promise<{ boxId: string; workspacePath: string }> {
    this.created += 1;
    const id = `sbx_${this.created}`;
    this.boxes.push(id);
    return { boxId: id, workspacePath: `/tmp/w${id}` };
  }
  async deleteBox(boxId: string): Promise<void> {
    this.boxes = this.boxes.filter((b) => b !== boxId);
  }
}

test('BoxManager reuses the same box for a session and deletes on release', async () => {
  const client = new FakeClient() as unknown as SandboxClient;
  const manager = new BoxManager(client);
  const req = { systemPrompt: '# ZGI' } as never;

  const first = await manager.ensure('s1', req);
  const second = await manager.ensure('s1', req);
  assert.equal(first.boxId, second.boxId);
  assert.equal(client.created, 1);

  manager.release('s1');
  await manager.closeAll();
  assert.equal(client.boxes.length, 0);
});
