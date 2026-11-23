# Loop task sync

This process synchronizes tasks assigned to mapped Loop members into a Multica workspace. Imported issues are assigned to the mapped Multica member in `backlog`; the service does not assign agents or start runs. Loop statuses and assignees are never changed.

## Configuration

Create a JSON configuration outside the repository. The Loop member ID is the Project member UID used by the Loop API, not the person's display name. The Multica member ID is the Multica user UUID.

```json
{
  "loop": { "baseUrl": "https://loop.example.com", "workspaceId": "loop-workspace-uuid" },
  "multica": { "baseUrl": "https://multica.example.com", "workspaceId": "multica-workspace-uuid" },
  "mappings": [
    { "loopMemberId": "loop-member-uid", "multicaMemberId": "multica-user-uuid" }
  ],
  "stateFile": "/var/lib/loop-sync/state.json",
  "intervalSeconds": 60
}
```

The state file's parent directory must exist. Keep the file: it holds task and comment associations and sync baselines. Run only one process per state file. The service refuses to use the state with a different pair of instances or workspaces.

Set `LOOP_TOKEN` and `MULTICA_TOKEN` in the process environment to credentials with access to the configured workspaces, then run:

```sh
node apps/loop-sync/main.mjs /path/to/config.json
```

The first scan includes every open Loop task assigned to each mapped person. Existing Loop comments are imported as well. Later scans sync title, description and new comments. Agent comments on the Multica issue are posted back to Loop after checking that the comment would not trigger an expert. Agent run transcripts are not copied. A field changed independently on both sides is reported as a conflict and is not overwritten.

## Recovery

Errors are printed to stderr. A failed write may have reached the other server even when the response was lost. In that case the state file retains a `pending` operation and synchronization stops rather than sending it a second time. Stop the process, inspect the affected task or comments on both systems, and reconcile the state file with the actual result before clearing `pending` and restarting. Back up the state file before editing it. Conflicts require reconciling the field on the two issues before the next scan can proceed.
