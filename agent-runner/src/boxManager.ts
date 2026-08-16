// Per-session registry of zgi-sandbox agent boxes. Each control-plane session
// gets at most one box (created on first ensure, seeded with the workspace
// memory file), which is reused across runs. Boxes are disposed explicitly via
// release() or swept on server shutdown via closeAll().
import type { Box, ProcessHandle, SandboxClient } from './sandboxClient.js';
import type { RunRequest } from './protocol.js';

export class BoxManager {
  private boxes = new Map<string, Box>();

  constructor(private client: SandboxClient) {}

  async ensure(sessionId: string, req: RunRequest): Promise<Box> {
    const existing = this.boxes.get(sessionId);
    if (existing) return existing;
    const box = await this.client.createBox({
      ttlSeconds: undefined,
      networkEnabled: true,
      workspaceSeed: seedFrom(req.systemPrompt),
    });
    this.boxes.set(sessionId, box);
    return box;
  }

  openProcess(box: Box, req: { command: string; args: string[]; env?: Record<string, string> }): Promise<ProcessHandle> {
    return this.client.openProcess(box.boxId, req);
  }

  release(sessionId: string): void {
    const box = this.boxes.get(sessionId);
    if (box) void this.client.deleteBox(box.boxId).catch(() => {});
    this.boxes.delete(sessionId);
  }

  async closeAll(): Promise<void> {
    const ids = [...this.boxes.keys()];
    for (const sessionId of ids) {
      const box = this.boxes.get(sessionId);
      if (box) await this.client.deleteBox(box.boxId).catch(() => {});
    }
    this.boxes.clear();
  }
}

function seedFrom(systemPrompt?: string): Record<string, string> {
  if (!systemPrompt || !systemPrompt.trim()) return {};
  return { 'CLAUDE.md': `# ZGI Coding Agent\n\n## Runtime Instructions\n\n${systemPrompt}\n` };
}
