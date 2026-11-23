import { readFileSync, writeFileSync, renameSync, existsSync } from 'node:fs';
import { setTimeout as sleep } from 'node:timers/promises';
import { Loop } from './loop.mjs';
import { Multica } from './multica.mjs';
import { sync } from './sync.mjs';

const config = JSON.parse(readFileSync(process.argv[2], 'utf8'));
const identity = JSON.stringify([config.loop.baseUrl, config.loop.workspaceId, config.multica.baseUrl, config.multica.workspaceId]);
const state = existsSync(config.stateFile)
  ? JSON.parse(readFileSync(config.stateFile, 'utf8'))
  : { identity, tasks: {}, pending: null };
if (state.identity !== identity) throw new Error('State file belongs to different instances or workspaces');
const save = () => {
  const temp = `${config.stateFile}.tmp`;
  writeFileSync(temp, JSON.stringify(state, null, 2), { mode: 0o600 });
  renameSync(temp, config.stateFile);
};
const loop = new Loop({ ...config.loop, token: process.env.LOOP_TOKEN });
const multica = new Multica({ ...config.multica, token: process.env.MULTICA_TOKEN });
while (true) {
  try {
    await sync({ loop, multica, mappings: config.mappings, state, save });
  } catch (error) {
    console.error(error);
  }
  await sleep(config.intervalSeconds * 1000);
}
