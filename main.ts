// Entry launcher: exec the vendored static Go binary `./forge` next to this file.
// The Go program owns the whole lifecycle (listen on an ephemeral port, register with
// $MAKEFUN_ENV_URL, serve until /done) and the whole forge (git smart HTTP, REST v3,
// GraphQL v4, MCP). This file only forwards stdio, the environment and signals, and
// exits with the binary's exit code.

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const forge = fileURLToPath(new URL('./forge', import.meta.url));

const child = spawn(forge, process.argv.slice(2), {
  stdio: 'inherit',
  env: process.env,
});

const forward = (signal: NodeJS.Signals): void => {
  try {
    child.kill(signal);
  } catch {
    // The child is already gone; nothing to forward to.
  }
};
process.on('SIGTERM', () => forward('SIGTERM'));
process.on('SIGINT', () => forward('SIGINT'));

child.on('error', (err: Error) => {
  process.stderr.write(`[entry] cannot start ./forge: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code: number | null, signal: NodeJS.Signals | null) => {
  if (signal !== null) {
    // Killed by a signal: die the same way so the harness sees the signal, not a 0.
    try {
      process.kill(process.pid, signal);
    } catch {
      process.exit(1);
    }
    return;
  }
  process.exit(code ?? 1);
});
