// Executable that the codex SDK spawns when codexPathOverride is set. The SDK
// invokes this as: <bridge> exec --experimental-json … — we forward those argv
// into a `codex` process running inside the zgi-sandbox agent box and bridge
// stdio (SDK stdin -> box stdin; box stdout/stderr -> our stdout/stderr).
import { createSandboxClient } from './sandboxClient.js';
import { pathToFileURL } from 'node:url';
import { Writable } from 'node:stream';

export function parseCodexBridgeArgs(argv) {
  // argv = [node, bridgePath, ...codexArgs]
  return argv.slice(2);
}

async function main() {
  const baseUrl = process.env.ZGI_SANDBOX_URL;
  const apiKey = process.env.ZGI_SANDBOX_API_KEY;
  const boxId = process.env.ZGI_BOX_ID;
  if (!baseUrl || !boxId) {
    process.stderr.write('codexBridge: ZGI_SANDBOX_URL and ZGI_BOX_ID are required\n');
    process.exit(2);
  }
  const client = createSandboxClient({ baseUrl, apiKey });
  const args = parseCodexBridgeArgs(process.argv);
  const handle = await client.openProcess(boxId, { command: 'codex', args });

  const stdinWriter = new Writable({
    write(chunk, _enc, cb) {
      handle.stdin.write(chunk, cb);
    },
  });
  process.stdin.pipe(stdinWriter);
  handle.stdout.pipe(process.stdout);
  handle.stderr.pipe(process.stderr);
  process.on('SIGTERM', () => handle.kill());
  process.on('SIGINT', () => handle.kill());

  const code = await handle.exited;
  process.exit(code ?? 1);
}

// Only run the bridge when this module is the entry point (spawned by the
// codex SDK). Guarding on process.argv[1] keeps `import`ing the module for
// tests from kicking off a boxed process.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((err) => {
    process.stderr.write(`codexBridge: ${err instanceof Error ? err.message : String(err)}\n`);
    process.exit(1);
  });
}
