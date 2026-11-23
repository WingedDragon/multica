import { readFileSync, writeFileSync, renameSync, existsSync } from 'node:fs';
import { resolve } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { Loop } from './loop.mjs';
import { Multica } from './multica.mjs';
import { sync } from './sync.mjs';

const config = JSON.parse(readFileSync(process.argv[2], 'utf8'));
const stateFiles = config.bindings.map(binding => resolve(binding.stateFile));
if (new Set(stateFiles).size !== stateFiles.length) throw new Error('Each binding must have a separate state file');
const bindings = config.bindings.map(binding => {
  const identity = JSON.stringify([binding.loop.baseUrl, binding.loop.workspaceId, binding.multica.baseUrl, binding.multica.workspaceId]);
  const state = existsSync(binding.stateFile)
    ? JSON.parse(readFileSync(binding.stateFile, 'utf8'))
    : { identity, tasks: {}, pending: null };
  if (state.identity !== identity) throw new Error(`State file belongs to different instances or workspaces: ${binding.stateFile}`);
  const save = () => {
    const temp = `${binding.stateFile}.tmp`;
    writeFileSync(temp, JSON.stringify(state, null, 2), { mode: 0o600 });
    renameSync(temp, binding.stateFile);
  };
  return {
    loop: new Loop({ ...binding.loop, token: process.env[binding.loop.tokenEnv] }),
    multica: new Multica({ ...binding.multica, token: process.env[binding.multica.tokenEnv] }),
    mappings: binding.mappings,
    state,
    save,
    name: `${binding.loop.workspaceId} -> ${binding.multica.workspaceId}`,
  };
});
while (true) {
  for (const binding of bindings) {
    try {
      await sync(binding);
    } catch (error) {
      console.error(`Binding ${binding.name}:`, error);
    }
  }
  await sleep(config.intervalSeconds * 1000);
}
