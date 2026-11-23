# Loop task sync

One process synchronizes multiple Loop-to-Multica workspace bindings. Imported issues are assigned to the mapped Multica member in `backlog`; the service does not assign agents or start runs. Loop statuses and assignees are never changed.

## Configuration

Create a JSON configuration outside the repository. Each binding has its own Loop token, workspaces, member mappings and state file. The Loop member ID is the Project member UID used by the Loop API, not the person's display name. The Multica member ID is the Multica user UUID. `tokenEnv` names environment variables; it is not the token itself.

```json
{
  "bindings": [
    {
      "loop": { "baseUrl": "https://loop.example.com", "workspaceId": "loop-workspace-uuid", "tokenEnv": "LOOP_TOKEN_FIRST" },
      "multica": { "baseUrl": "https://multica.example.com", "workspaceId": "multica-workspace-uuid", "tokenEnv": "MULTICA_TOKEN_FIRST" },
      "mappings": [
        { "loopMemberId": "loop-member-uid", "multicaMemberId": "multica-user-uuid" }
      ],
      "stateFile": "/var/lib/loop-sync/first.json"
    }
  ],
  "intervalSeconds": 60
}
```

Each state file's parent directory must exist. Keep these files: they hold task and comment associations and sync baselines. Each binding must use a distinct state file. Run only one process per state file. The service refuses to use a state file with a different pair of instances or workspaces.

Set the environment variables named by each binding's `tokenEnv` to credentials with access to its configured workspaces. They may reference the same Multica credential where appropriate. Then run:

```sh
node apps/loop-sync/main.mjs /path/to/config.json
```

The first scan includes every open Loop task assigned to each mapped person. Existing Loop comments are imported as well. Later scans sync title, description and new comments. Agent comments on the Multica issue are posted back to Loop after checking that the comment would not trigger an expert. Agent run transcripts are not copied. A field changed independently on both sides is reported as a conflict and is not overwritten. A failing binding is reported and does not prevent the other bindings from running.

## Recovery

Errors are printed to stderr. A failed write may have reached the other server even when the response was lost. In that case the state file retains a `pending` operation and synchronization stops rather than sending it a second time. Stop the process, inspect the affected task or comments on both systems, and reconcile the state file with the actual result before clearing `pending` and restarting. Back up the state file before editing it. Conflicts require reconciling the field on the two issues before the next scan can proceed.
