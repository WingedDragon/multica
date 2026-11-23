import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

test('one process syncs each workspace using its own token and state', { timeout: 10000 }, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'loop-sync-test-'));
  const created = [];
  const requests = [];
  const server = createServer(async (req, res) => {
    let body = ''; for await (const part of req) body += part;
    const path = new URL(req.url, 'http://localhost').pathname;
    const workspace = req.headers['x-workspace-id'];
    requests.push({ path, workspace, token: req.headers.authorization });
    let result;
    if (path === '/fleet/api/v1/tasks') result = { data: [{ id: 'same-task-id' }], pagination: { page: 1, page_size: 100, total: 1 } };
    else if (path === '/fleet/api/v1/tasks/same-task-id') result = { data: { id: 'same-task-id', title: workspace, description: 'Work' } };
    else if (path === '/fleet/api/v1/tasks/same-task-id/comments') result = { data: [], pagination: { page: 1, page_size: 100, total: 0 } };
    else if (path === '/api/issues' && req.method === 'POST') { const fields = JSON.parse(body); created.push({ workspace, ...fields }); result = { id: `issue-${workspace}`, ...fields }; }
    else if (path.startsWith('/api/issues/issue-') && path.endsWith('/comments')) result = [];
    else if (path.startsWith('/api/issues/issue-')) result = { id: `issue-${workspace}`, title: workspace === 'multica-a' ? 'loop-a' : 'loop-b', description: 'Work' };
    else { res.writeHead(404); res.end(); return; }
    res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(result));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const baseUrl = `http://127.0.0.1:${server.address().port}`;
  const configFile = join(dir, 'config.json');
  const bindings = ['a', 'b'].map(name => ({
    loop: { baseUrl, workspaceId: `loop-${name}`, tokenEnv: `LOOP_${name.toUpperCase()}` },
    multica: { baseUrl, workspaceId: `multica-${name}`, tokenEnv: `MULTICA_${name.toUpperCase()}` },
    mappings: [{ loopMemberId: `person-${name}`, multicaMemberId: `member-${name}` }],
    stateFile: join(dir, `${name}.json`),
  }));
  writeFileSync(configFile, JSON.stringify({ bindings, intervalSeconds: 60 }));
  const child = spawn(process.execPath, ['apps/loop-sync/main.mjs', configFile], { env: { ...process.env, LOOP_A: 'loop-token-a', LOOP_B: 'loop-token-b', MULTICA_A: 'multica-token-a', MULTICA_B: 'multica-token-b' } });
  const exited = new Promise(resolve => child.once('exit', resolve));
  let stderr = ''; child.stderr.on('data', chunk => { stderr += chunk; });
  try {
    const deadline = Date.now() + 5000;
    while (created.length < 2 && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 20));
    assert.equal(created.length, 2, stderr);
    for (const name of ['a', 'b']) {
      assert.ok(requests.some(r => r.workspace === `loop-${name}` && r.token === `Bearer loop-token-${name}`));
      assert.ok(requests.some(r => r.workspace === `multica-${name}` && r.token === `Bearer multica-token-${name}`));
      assert.equal(created.find(c => c.workspace === `multica-${name}`).assignee_id, `member-${name}`);
      assert.equal(created.find(c => c.workspace === `multica-${name}`).status, 'backlog');
      const deadline = Date.now() + 1000;
      let state;
      while (Date.now() < deadline) {
        try { state = JSON.parse(readFileSync(join(dir, `${name}.json`))); if (state.tasks['same-task-id']) break; } catch {}
        await new Promise(resolve => setTimeout(resolve, 10));
      }
      assert.equal(state.tasks['same-task-id'].multicaId, `issue-multica-${name}`);
    }
  } finally {
    child.kill();
    await exited;
    await new Promise(resolve => server.close(resolve));
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a failed binding does not stop another binding', { timeout: 10000 }, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'loop-sync-failure-test-'));
  let secondReached = false;
  const server = createServer((req, res) => {
    res.setHeader('content-type', 'application/json');
    if (req.headers['x-workspace-id'] === 'broken') {
      res.writeHead(403); res.end(JSON.stringify({ error: { message: 'denied' } }));
    } else {
      secondReached = true;
      res.end(JSON.stringify({ data: [], pagination: { page: 1, page_size: 100, total: 0 } }));
    }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const baseUrl = `http://127.0.0.1:${server.address().port}`;
  const bindings = ['broken', 'healthy'].map(workspaceId => ({
    loop: { baseUrl, workspaceId, tokenEnv: 'LOOP_TOKEN' },
    multica: { baseUrl, workspaceId, tokenEnv: 'MULTICA_TOKEN' },
    mappings: [{ loopMemberId: 'member', multicaMemberId: 'member' }],
    stateFile: join(dir, `${workspaceId}.json`),
  }));
  const configFile = join(dir, 'config.json');
  writeFileSync(configFile, JSON.stringify({ bindings, intervalSeconds: 60 }));
  const child = spawn(process.execPath, ['apps/loop-sync/main.mjs', configFile], { env: { ...process.env, LOOP_TOKEN: 'loop', MULTICA_TOKEN: 'multica' } });
  const exited = new Promise(resolve => child.once('exit', resolve));
  let stderr = ''; child.stderr.on('data', chunk => { stderr += chunk; });
  try {
    const deadline = Date.now() + 5000;
    while (!secondReached && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 20));
    assert.equal(secondReached, true, stderr);
    assert.match(stderr, /broken.*denied/s);
  } finally {
    child.kill(); await exited;
    await new Promise(resolve => server.close(resolve));
    rmSync(dir, { recursive: true, force: true });
  }
});
