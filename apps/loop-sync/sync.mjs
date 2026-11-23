async function write(state, save, operation, fn) {
  state.pending = operation;
  save();
  const result = await fn();
  state.pending = null;
  return result;
}

export async function sync({ loop, multica, mappings, state, save }) {
  if (state.pending) throw new Error(`Unconfirmed write requires reconciliation: ${JSON.stringify(state.pending)}`);
  for (const mapping of mappings) {
    const tasks = await loop.listOpenTasks(mapping.loopMemberId);
    for (const item of tasks) {
      const source = await loop.getTask(item.task_id);
      let record = state.tasks[source.task_id];
      if (!record) {
        const fields = { title: source.title, description: source.description, status: 'backlog', assignee_type: 'member', assignee_id: mapping.multicaMemberId };
        const created = await write(state, save, { operation: 'createIssue', loopId: source.task_id }, () => multica.createIssue(fields));
        record = state.tasks[source.task_id] = { multicaId: created.id, title: source.title, description: source.description, loopComments: {}, multicaComments: {} };
        save();
      }
      const target = await multica.getIssue(record.multicaId);
      const changes = {};
      for (const field of ['title', 'description']) {
        if (source[field] !== record[field]) {
          if (target[field] !== record[field] && target[field] !== source[field]) throw new Error(`Conflict on ${source.task_id}: ${field}`);
          if (target[field] !== source[field]) changes[field] = source[field];
        }
      }
      if (Object.keys(changes).length) {
        await write(state, save, { operation: 'updateIssue', loopId: source.task_id, multicaId: record.multicaId, changes }, () => multica.updateIssue(record.multicaId, changes));
      }
      record.title = source.title;
      record.description = source.description;
      save();
      for (const comment of await loop.listComments(source.task_id)) {
        if (record.loopComments[comment.comment_id] || !comment.content) continue;
        const result = await write(state, save, { operation: 'createMulticaComment', loopId: source.task_id, commentId: comment.comment_id }, () => multica.createComment(record.multicaId, comment.content));
        record.loopComments[comment.comment_id] = result.id;
        record.multicaComments[result.id] = comment.comment_id;
        save();
      }
    }
  }
  for (const [loopId, record] of Object.entries(state.tasks)) {
    for (const comment of await multica.listComments(record.multicaId)) {
      if (record.multicaComments[comment.id] || comment.author_type !== 'agent' || !comment.content) continue;
      const preview = await loop.previewComment(loopId, comment.content);
      if (preview.agents.length) throw new Error(`Loop comment triggers an expert: ${loopId}/${comment.id}`);
      const result = await write(state, save, { operation: 'createLoopComment', loopId, commentId: comment.id }, () => loop.createComment(loopId, comment.content));
      record.multicaComments[comment.id] = result.comment_id;
      record.loopComments[result.comment_id] = comment.id;
      save();
    }
  }
}
