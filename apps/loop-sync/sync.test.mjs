import test from 'node:test';
import assert from 'node:assert/strict';
import { sync } from './sync.mjs';

function fixture() {
  const loopTask = { task_id: 'l1', title: 'Original', description: 'Details' };
  const issue = { id: 'm1', title: 'Original', description: 'Details' };
  const created = [];
  const updated = [];
  const state = { tasks: {}, pending: null };
  const loop = {
    async listOpenTasks() { return [loopTask]; },
    async getTask(id) { assert.equal(id, 'l1'); return loopTask; },
    async listComments() { return []; },
  };
  const multica = {
    async createIssue(fields) { created.push(fields); return { ...issue, ...fields }; },
    async getIssue() { return issue; },
    async updateIssue(id, fields) { updated.push(fields); Object.assign(issue, fields); },
    async listComments() { return []; },
  };
  return { loop, multica, state, loopTask, issue, created, updated };
}

test('imports mapped tasks once without starting an agent', async () => {
  const f = fixture();
  const options = { ...f, mappings: [{ loopMemberId: 'person', multicaMemberId: 'member' }], save() {} };
  await sync(options);
  await sync(options);
  assert.equal(f.created.length, 1);
  assert.equal(f.created[0].status, 'backlog');
  assert.equal(f.created[0].assignee_type, 'member');
  assert.equal(f.created[0].assignee_id, 'member');
});

test('detects concurrent edits rather than overwriting local work', async () => {
  const f = fixture();
  const options = { ...f, mappings: [{ loopMemberId: 'person', multicaMemberId: 'member' }], save() {} };
  await sync(options);
  f.issue.title = 'Local';
  f.loopTask.title = 'Remote';
  await assert.rejects(sync(options), /conflict/i);
  assert.equal(f.updated.length, 0);
});

test('an unconfirmed create is not attempted again', async () => {
  const f = fixture();
  let attempts = 0;
  f.multica.createIssue = async () => { attempts++; throw new Error('connection lost'); };
  const options = { ...f, mappings: [{ loopMemberId: 'person', multicaMemberId: 'member' }], save() {} };
  await assert.rejects(sync(options), /connection lost/);
  await assert.rejects(sync(options), /reconciliation/);
  assert.equal(attempts, 1);
});

test('copies comments once without echoing them back', async () => {
  const f = fixture();
  const sourceComments = [{ comment_id: 'lc1', content: 'Requirement' }];
  const targetComments = [{ id: 'mc1', author_type: 'agent', content: 'Result' }];
  f.loop.listComments = async () => sourceComments;
  f.multica.listComments = async () => targetComments;
  f.multica.createComment = async (_id, content) => {
    const c = { id: 'mc2', author_type: 'user', content };
    targetComments.push(c);
    return c;
  };
  f.loop.previewComment = async () => ({ agents: [] });
  f.loop.createComment = async (_id, content) => {
    const c = { comment_id: 'lc2', content };
    sourceComments.push(c);
    return c;
  };
  const options = { ...f, mappings: [{ loopMemberId: 'person', multicaMemberId: 'member' }], save() {} };
  await sync(options);
  await sync(options);
  assert.equal(sourceComments.length, 2);
  assert.equal(targetComments.length, 2);
  assert.equal(sourceComments[1].content, 'Result');
  assert.equal(targetComments[1].content, 'Requirement');
});

test('does not write a Loop comment that would trigger an expert', async () => {
  const f = fixture();
  f.multica.listComments = async () => [{ id: 'mc1', author_type: 'agent', content: 'Result' }];
  f.loop.previewComment = async () => ({ agents: [{ id: 'expert' }] });
  const options = { ...f, mappings: [{ loopMemberId: 'person', multicaMemberId: 'member' }], save() {} };
  await assert.rejects(sync(options), /triggers an expert/);
});
