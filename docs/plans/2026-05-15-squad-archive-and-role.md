# Squad Archive Dialog & Role Combobox Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace native `confirm()` archive flow with shadcn `AlertDialog` showing leader name + issue count, and replace inline `<Input>` `RoleEditor` with a shadcn Combobox (Command + Popover) backed by existing roles in the squad.

**Architecture:** Two parallel tracks. (1) Backend adds an `issue_count` field to `SquadResponse`, sourced from a new `CountIssuesForSquad` sqlc query that counts all issues currently assigned to the squad. The existing `TransferSquadAssignees` (count-all, transfer-all) is unchanged. `DeleteSquad` is rewritten to run transfer + archive inside a single pgx transaction so the v3 invariant ("after archive no issue points to the squad") is durably enforced rather than best-effort. (2) Frontend extends the `Squad` type + zod schema (defensive parse), replaces `confirm()` with `ArchiveSquadConfirmDialog`, and rewrites `RoleEditor` as a Command-inside-Popover combobox that aggregates `members.map(m => m.role).filter(Boolean)` for suggestions.

**Tech Stack:** Go (chi, sqlc, pgx), TypeScript, React, TanStack Query, shadcn (Base UI primitives), zod, Vitest, Playwright.

**Files in scope:**
- `server/pkg/db/queries/issue.sql` — new query
- `server/internal/handler/squad.go` — extend `SquadResponse`, populate `issue_count` in `GetSquad`, rewrite `DeleteSquad` to run transfer + archive in one tx
- `server/internal/handler/squad_test.go` (new or extend) — handler tests (count, transfer-all, transactional rollback)
- `packages/core/types/squad.ts` — extend `Squad`
- `packages/core/api/schemas.ts` (and/or `schema.ts` usage in client) — squad zod schema + `parseWithFallback` in `getSquad`
- `packages/core/api/client.ts:1458` — `getSquad` parses the response
- `packages/views/squads/components/squad-detail-page.tsx:209` — header archive button
- `packages/views/squads/components/squad-detail-page.tsx:649-699` — `RoleEditor`
- `packages/views/squads/components/archive-squad-confirm-dialog.tsx` — new file
- `packages/views/locales/en/squads.json` and `packages/views/locales/zh-Hans/squads.json` — new keys
- `packages/views/squads/components/squad-detail-page.test.tsx` (new) — view tests

**Out of scope:** Workspace-wide role library, server-side role enum, redesign of any other inspector control.

---

## API Compatibility Notes (for plan reviewer)

- `SquadResponse.issue_count` is **additive**. Old desktop clients ignore unknown fields, so adding it is backwards compatible.
- The field is **only present on `GET /api/squads/{id}`**. `ListSquads`, `CreateSquad`, and `UpdateSquad` deliberately omit it (would be N+1 for list; semantically irrelevant for create/update which return the just-written row). `omitempty` on a nil `*int64` keeps the field absent from those responses.
- The TS field is declared **optional** (`issue_count?: number | null`) — see Task 3 rationale. This matches the wire truth: only the detail endpoint carries it. Making it required `number | null` would lie about list/create/update shapes.
- The archive dialog falls back to "any assigned issues" copy when the value is `undefined` or `null`, so an older server (no field) and a transient count error (null) collapse to the same safe rendering.
- All response parsing for `getSquad` goes through `parseWithFallback` per `CLAUDE.md → API Response Compatibility`. List/create/update remain raw `this.fetch` to stay consistent with the rest of the client (no precedent for wrapping them, and they don't touch the archive dialog).
- No DB migration required — the count is computed on read.

## Archive semantics decision (resolved — v3)

**Decision: count and transfer ALL issues assigned to the squad, regardless of status.** This reverts plan-v2's "active-only" carve-out.

v1 → v2 → v3 history:
- **v1**: count all + transfer all. Matched the pre-existing `TransferSquadAssignees` behavior in `server/pkg/db/queries/squad.sql:71`.
- **v2**: count active-only + transfer active-only. Argument was "closed issues should reflect who owned them at close time, not inherit the squad's leftovers years later."
- **v3 (this plan)**: revert to count all + transfer all. v2's product argument was sound *if free*, but plan-review round 2 surfaced two structural costs that were missed:

  1. **Stale name resolution for archived squad assignees.** `packages/core/workspace/hooks.ts:11,23` powers `useActorName`, which reads only from `squadListOptions` (calls `ListSquads`). `server/pkg/db/queries/squad.sql:13` filters `archived_at IS NULL`. A closed issue still pointing at an archived squad would render its assignee as "Unknown Squad". Fixing that under v2 semantics required *one* of: (i) an `--include-archived` parameter on `ListSquads` plus changes to every consumer that doesn't want archived squads in pickers, (ii) a new `GetSquadName` endpoint and a name-only client cache, or (iii) merging archived squads into the existing query but having every picker / dropdown filter them back out client-side. All three paths exist *only* to support v2's choice.

  2. **Reopen invariant break.** `server/internal/handler/issue.go:1453-1496,1563-1570` only re-runs `validateAssigneePair` when the caller touches `assignee_type` or `assignee_id`. A status-only update (the normal "reopen" path — `packages/views/issues/components/issue-detail.tsx:953,959` exposes status and assignee as independent controls) bypasses validation, so a `done`/`cancelled` issue pointing at an archived squad can be reopened to `in_progress` and become an active issue with an archived-squad assignee — directly violating the invariant enforced at `issue.go:1718-1720` ("cannot assign to an archived squad"). Closing this gap under v2 required *one* of: (i) auto-rewrite assignee to leader on reopen, (ii) block reopen until assignee is changed, or (iii) extend `validateAssigneePair` to run on every update. Each option introduces a new special case and a new regression test.

v3 makes both problems disappear by construction: after `ArchiveSquad` runs, **no issue points to this squad anymore**, so name resolution and reopen behave identically to the no-archived-squad world. No new flags, no new endpoints, no new validation paths.

> **Important:** "After `ArchiveSquad` runs, no issue points to the squad" is an invariant on the *whole* `DeleteSquad` handler, not just on the SQL `TransferSquadAssignees` query. Today's handler at `server/internal/handler/squad.go:292-309` runs transfer and archive as two best-effort steps: transfer errors only `slog.Warn` and execution continues into archive, and a transfer that succeeds is not rolled back if the subsequent archive fails. Both failure modes reproduce the v2-era problems (Unknown Squad name, reopen-into-archived-assignee, half-archived squad with active issue pointers). Task 2b makes the handler transactional so the invariant is durable, not best-effort.

**Product trade-off acknowledged.** A closed issue's historical "Assigned to" badge is rewritten from `<squad>` to `<leader-agent>`. The squad row stays in the DB (`archived_at` set, name preserved) — only the *per-issue assignee pointer* moves. This is consistent with existing agent-level reassignment patterns (when an agent leaves a project, their open + closed issues get reassigned with no special history-preservation). The leader is a reasonable proxy for "who inherited responsibility for this squad's work". Dialog copy is honest about this: "{leader} will take over all {N} issues currently assigned to this squad." — count and action operate on identical sets.

**Impact of v3:**
- `TransferSquadAssignees` SQL is **unchanged** (already transfers all). The plan no longer modifies `squad.sql`.
- A new Go test asserts the invariant: "after archive, no issue (active or terminal) has `assignee_type='squad', assignee_id=<archived squad>` — all transferred to leader."
- `CountIssuesForSquad` counts every assigned issue (no status filter); dialog count and SQL transfer set are identical.
- No frontend archived-squad name-resolution work.
- No reopen-guard logic.
- Closed historical issues whose assignee was the squad now show the leader agent's badge instead of the squad's name. Acceptable per the product trade-off above.

## Index assumption (resolved)

The previous draft (now removed) claimed an existing `(workspace_id, assignee_id)` index. Re-checked against `server/migrations/001_init.up.sql:168-170` — the actual issue indexes are:

```sql
CREATE INDEX idx_issue_workspace ON issue(workspace_id);
CREATE INDEX idx_issue_assignee  ON issue(assignee_type, assignee_id);
CREATE INDEX idx_issue_status    ON issue(workspace_id, status);
```

Real plan for `CountIssuesForSquad`:

```
WHERE workspace_id = $1
  AND assignee_type = 'squad'
  AND assignee_id   = $2;
```

Postgres will use `idx_issue_assignee` (the `(assignee_type, assignee_id)` composite), narrowing first by `assignee_type='squad'`, then by `assignee_id=<squad uuid>`. The output of that index probe is **all issues currently assigned to this one squad**. `workspace_id` is applied as a heap recheck on that small set.

**Cardinality estimate:** squad rosters are 2-10 entities (per `CLAUDE.md` project context), and squads are workspace-internal. Realistic upper bound for issues ever assigned to a single squad in a workspace: **low-hundreds**. The heap recheck over ≤ a few hundred rows is sub-millisecond — no measurable difference from a covering or partial index. Even a worst-case workspace (thousands of issues, dozens of squads) keeps each per-squad probe in the same band because the index restricts by squad first.

**Conclusion: no new index.** `idx_issue_assignee` is sufficient. If profiling later shows this query becomes hot on a large workspace, the right follow-up is:

```sql
CREATE INDEX CONCURRENTLY idx_issue_squad_assignee
  ON issue(assignee_id)
  WHERE assignee_type = 'squad';
```

That is a partial index keyed on `assignee_id` alone, filtering `assignee_type` at index time. Don't add it speculatively — the current index handles the realistic load.

---

## Task 1: Backend — `CountIssuesForSquad` sqlc query + archive-transfer regression test

**Files:**
- Modify: `server/pkg/db/queries/issue.sql`
- `server/pkg/db/queries/squad.sql` is **unchanged** (existing `TransferSquadAssignees` already counts all + transfers all, matching v3 semantics).

**Step 1: Write the failing test (Go integration test)**

In `server/internal/handler/squad_test.go` (extend existing or create), add:

```go
func TestGetSquadIncludesIssueCount(t *testing.T) {
    ts := newTestServer(t)
    ws, owner := ts.createWorkspace(t)
    leader := ts.createAgent(t, ws.ID)
    squad := ts.createSquad(t, ws.ID, owner.UserID, leader.ID)

    // Mix of statuses — count is ALL issues regardless of status (v3 semantics).
    ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "todo")
    ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "in_progress")
    ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "done")
    ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "cancelled")

    resp := ts.getSquad(t, ws.ID, squad.ID)
    require.NotNil(t, resp.IssueCount, "GetSquad must populate issue_count")
    require.Equal(t, int64(4), *resp.IssueCount)
}
```

`IssueCount` is `*int64` (see Task 2). Compare `int64(4)` against the dereferenced pointer, and assert non-nil first so a future regression that drops the field surfaces as a clear failure instead of a nil-pointer panic.

**Step 2: Run and confirm it fails**

```bash
cd server && go test ./internal/handler/ -run TestGetSquadIncludesIssueCount
```

Expected: FAIL — field `IssueCount` does not exist yet.

**Step 3: Add the sqlc query**

Append to `server/pkg/db/queries/issue.sql`:

```sql
-- name: CountIssuesForSquad :one
-- Count all issues currently assigned to a squad. No status filter:
-- archive transfers every assigned issue to the leader (see TransferSquadAssignees
-- in squad.sql), so count and transfer operate on identical sets. This avoids
-- leaving archived-squad pointers in the DB, which would otherwise break
-- name resolution (useActorName reads ListSquads which filters archived_at IS NULL)
-- and the "no active issue can be assigned to an archived squad" invariant
-- enforced by validateAssigneePair.
SELECT COUNT(*)::bigint AS count
FROM issue
WHERE workspace_id = $1
  AND assignee_type = 'squad'
  AND assignee_id = $2;
```

**Step 4: Regenerate sqlc**

```bash
make sqlc
```

Expected: no errors. `server/pkg/db/generated/issue.sql.go` now exposes `CountIssuesForSquad`.

**Step 5: Archive-transfer regression test (no SQL change required)**

Add a regression test in `server/internal/handler/squad_test.go` (alongside the count test). The intent is to lock the v3 invariant: **after archive, no issue points to the archived squad — terminal-state issues are reassigned to the leader along with active ones.** This guards against a future "optimization" that accidentally reintroduces v2's active-only filter.

```go
func TestDeleteSquadTransfersAllIssuesIncludingTerminalToLeader(t *testing.T) {
    ts := newTestServer(t)
    ws, owner := ts.createWorkspace(t)
    leader := ts.createAgent(t, ws.ID)
    squad := ts.createSquad(t, ws.ID, owner.UserID, leader.ID)

    active    := ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "in_progress")
    done      := ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "done")
    cancelled := ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "cancelled")

    ts.deleteSquad(t, ws.ID, squad.ID, owner.UserID)

    for _, c := range []struct {
        name string
        id   uuid.UUID
    }{
        {"active", active.ID},
        {"done (terminal)", done.ID},
        {"cancelled (terminal)", cancelled.ID},
    } {
        t.Run(c.name, func(t *testing.T) {
            got := ts.getIssue(t, c.id)
            require.Equalf(t, "agent", got.AssigneeType,
                "v3 invariant: no archived-squad pointers may remain after archive (%s)", c.name)
            require.Equalf(t, leader.ID.String(), got.AssigneeID,
                "%s issue must be reassigned to squad leader", c.name)
        })
    }
}
```

**Step 6: Regenerate sqlc and run both tests**

```bash
make sqlc
cd server && go test ./internal/handler/ -run "TestGetSquadIncludesIssueCount|TestDeleteSquadTransfersAllIssuesIncludingTerminalToLeader" -v
```

Expected: PASS (after Task 2 wires the handler).

**Step 7: Commit**

```bash
git add server/pkg/db/queries/ server/pkg/db/generated/
git commit -m "feat(squad): count all assigned issues for archive dialog"
```

---

## Task 2: Backend — extend `SquadResponse` with `issue_count`

**Files:**
- Modify: `server/internal/handler/squad.go:18-31` (response type) and `:193-199` (`GetSquad` handler)

**Step 1: Extend `SquadResponse`**

Add field at the bottom of the struct (after `ArchivedBy`) — keeps JSON ordering stable for any consumers that depend on it:

```go
type SquadResponse struct {
    // ... existing fields ...
    ArchivedBy *string `json:"archived_by"`
    IssueCount *int64  `json:"issue_count,omitempty"`
}
```

`*int64` + `omitempty` because:
- `ListSquads` doesn't populate it (would be N+1) — must serialize as absent, not `0`.
- A pointer makes "not populated" distinguishable from "zero issues".

**Step 2: Leave `squadToResponse` unchanged**

`squadToResponse(s db.Squad)` stays returning `IssueCount: nil`. Counting per row in the list endpoint would be N+1; only `GetSquad` needs the count.

**Step 3: Populate in `GetSquad`**

Replace `squad.go:193-199`:

```go
func (h *Handler) GetSquad(w http.ResponseWriter, r *http.Request) {
    squad, _, ok := h.loadSquadInWorkspace(w, r)
    if !ok {
        return
    }
    resp := squadToResponse(squad)

    count, err := h.Queries.CountIssuesForSquad(r.Context(), db.CountIssuesForSquadParams{
        WorkspaceID: squad.WorkspaceID,
        AssigneeID:  squad.ID, // see note below on param shape after sqlc gen
    })
    if err != nil {
        // Non-fatal: log and continue with nil. The UI degrades to "leader only" copy.
        slog.Warn("count squad issues failed", "squad_id", uuidToString(squad.ID), "error", err)
    } else {
        resp.IssueCount = &count
    }
    writeJSON(w, http.StatusOK, resp)
}
```

> Note: verify `CountIssuesForSquadParams` param shape after `make sqlc`. `assignee_id` is a nullable UUID column in `issue`, so sqlc will likely generate `pgtype.UUID` directly; pass `squad.ID` directly if so, otherwise wrap. **Adjust to match generated code** — do not invent helper names.

**Step 4: Run the test from Task 1**

```bash
cd server && go test ./internal/handler/ -run TestGetSquadIncludesIssueCount -v
```

Expected: PASS.

**Step 5: Run full handler test suite**

```bash
cd server && go test ./internal/handler/...
```

Expected: all pass — no regressions.

**Step 6: Commit**

```bash
git add server/internal/handler/squad.go server/internal/handler/squad_test.go
git commit -m "feat(squad): include issue_count in GetSquad response"
```

---

## Task 2b: Backend — make `DeleteSquad` transactional (transfer + archive atomic)

**Files:**
- Modify: `server/internal/handler/squad.go:276-316` (`DeleteSquad`)
- Modify: `server/internal/handler/squad_test.go` (add transactional regression test)

**Why this task exists.** Plan-review round 3 flagged that the v3 invariant ("after `ArchiveSquad`, no issue points to this squad") is asserted by the new Task 1 test on the *happy path*, but the current `DeleteSquad` handler does not enforce it under failure. Two concrete failure modes:

1. **Forward partial write:** `TransferSquadAssignees` fails (DB hiccup, ctx cancellation, deadlock with another writer). Current code at `server/internal/handler/squad.go:296-298` only `slog.Warn`s and continues into `ArchiveSquad`. Result: squad is archived (`archived_at` set), but issues still have `assignee_id = <archived squad>`. This reintroduces the exact v2-era bugs (Unknown Squad name in `useActorName`, reopen-into-archived-squad via status-only update) that v3 was designed to prevent.
2. **Reverse partial write:** `TransferSquadAssignees` succeeds. `ArchiveSquad` then fails (uniqueness on `archived_at`, ctx cancellation, etc.). Current code returns 500 to the caller, but `TransferSquadAssignees` has already committed. Result: active squad with all of its issues reassigned away. UX-wise the user thinks "archive failed, nothing happened" but their squad is silently emptied.

Both reduce to the same fix: run both write queries inside a single pgx transaction. On any error, roll back and return 5xx; only on commit do we publish the `EventSquadDeleted` event and return 204.

**Step 1: Write the failing test (Go integration test)**

Append to `server/internal/handler/squad_test.go`:

```go
// Lock the v3 invariant under failure: if the transfer step fails for any reason,
// the squad must NOT be archived. This guards against regressing back to the
// best-effort behavior where slog.Warn allowed half-completed archives.
func TestDeleteSquadFailedTransferLeavesSquadActive(t *testing.T) {
    ts := newTestServer(t)
    ws, owner := ts.createWorkspace(t)
    leader := ts.createAgent(t, ws.ID)
    squad := ts.createSquad(t, ws.ID, owner.UserID, leader.ID)
    issue := ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "in_progress")

    // Force the transfer step to fail. Simplest realistic trigger:
    //   - Wrap h.TxStarter / h.Queries so TransferSquadAssignees returns an
    //     error in this one test, OR
    //   - Run the request with a cancelled context so the first query errors.
    // Use whichever the existing test harness already supports (look for
    // existing tests that exercise tx-rollback paths in handler_test.go /
    // project_test.go and mirror the pattern).
    status, _ := ts.deleteSquadExpectingError(t, ws.ID, squad.ID, owner.UserID)
    require.GreaterOrEqual(t, status, 500, "transfer failure must surface as 5xx")

    // Invariant: squad is NOT archived.
    fresh := ts.getSquadRaw(t, ws.ID, squad.ID)
    require.False(t, fresh.ArchivedAt.Valid, "squad must remain active when transfer fails")

    // Invariant: issue assignee is unchanged.
    got := ts.getIssue(t, issue.ID)
    require.Equal(t, "squad", got.AssigneeType, "transfer must roll back: assignee_type still 'squad'")
    require.Equal(t, squad.ID.String(), got.AssigneeID, "transfer must roll back: assignee_id still the squad")
}

// Symmetric: if archive fails after a successful transfer, the transfer must
// also roll back. Locks the reverse partial-write scenario.
func TestDeleteSquadFailedArchiveRollsBackTransfer(t *testing.T) {
    ts := newTestServer(t)
    ws, owner := ts.createWorkspace(t)
    leader := ts.createAgent(t, ws.ID)
    squad := ts.createSquad(t, ws.ID, owner.UserID, leader.ID)
    issue := ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "in_progress")

    // Force the archive step (and ONLY the archive step) to fail. Use the same
    // harness-level fault injection as the test above, but target ArchiveSquad.
    status, _ := ts.deleteSquadExpectingError(t, ws.ID, squad.ID, owner.UserID)
    require.GreaterOrEqual(t, status, 500)

    fresh := ts.getSquadRaw(t, ws.ID, squad.ID)
    require.False(t, fresh.ArchivedAt.Valid, "squad must remain active when archive fails")

    got := ts.getIssue(t, issue.ID)
    require.Equal(t, "squad", got.AssigneeType, "transfer rollback: assignee_type still 'squad'")
    require.Equal(t, squad.ID.String(), got.AssigneeID, "transfer rollback: assignee_id still the squad")
}
```

> **Note on fault injection.** `server/internal/handler/squad_test.go` does not currently exercise tx-rollback paths. Before writing the failing test, grep for an existing pattern: `grep -rn "Begin.*error\|tx.*rollback\|fault.*inject" server/internal/handler/` and reuse whatever the project already does (likely a `Queries` interface wrapper or a context-cancellation trick). If no pattern exists yet, the lightest path is a wrapping `txStarter` in the test harness that returns an error on the Nth call. Do not invent a new fault-injection framework just for this — escalate to leader if `make sqlc` produced an interface that's awkward to mock and there's no existing precedent in the test suite.

**Step 2: Run and confirm both tests fail**

```bash
cd server && go test ./internal/handler/ -run "TestDeleteSquadFailedTransferLeavesSquadActive|TestDeleteSquadFailedArchiveRollsBackTransfer" -v
```

Expected: FAIL. The current handler does not roll back on transfer error and does not undo a successful transfer when archive fails.

**Step 3: Rewrite `DeleteSquad` to be transactional**

Replace `server/internal/handler/squad.go:276-316` with:

```go
func (h *Handler) DeleteSquad(w http.ResponseWriter, r *http.Request) {
    workspaceID := workspaceIDFromURL(r, "workspaceId")
    if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
        return
    }

    squad, _, ok := h.loadSquadInWorkspace(w, r)
    if !ok {
        return
    }

    if squad.ArchivedAt.Valid {
        writeError(w, http.StatusBadRequest, "squad is already archived")
        return
    }

    userID := requestUserID(r)
    userUUID, ok := parseUUIDOrBadRequest(w, userID, "user_id")
    if !ok {
        return
    }

    // Transactional: transfer assigned issues to the leader, then archive the
    // squad. Either both happen or neither happens. This enforces the v3
    // invariant ("after archive, no issue points to this squad") even under
    // partial-failure scenarios. See plan §"Archive semantics decision (v3)".
    tx, err := h.TxStarter.Begin(r.Context())
    if err != nil {
        writeError(w, http.StatusInternalServerError, "failed to start transaction")
        return
    }
    defer tx.Rollback(r.Context())
    qtx := h.Queries.WithTx(tx)

    if err := qtx.TransferSquadAssignees(r.Context(), db.TransferSquadAssigneesParams{
        AssigneeID:   squad.ID,
        AssigneeID_2: squad.LeaderID,
    }); err != nil {
        slog.Error("transfer squad assignees failed", "squad_id", uuidToString(squad.ID), "error", err)
        writeError(w, http.StatusInternalServerError, "failed to transfer squad issues")
        return
    }

    if _, err := qtx.ArchiveSquad(r.Context(), db.ArchiveSquadParams{
        ID:         squad.ID,
        ArchivedBy: userUUID,
    }); err != nil {
        slog.Error("archive squad failed", "squad_id", uuidToString(squad.ID), "error", err)
        writeError(w, http.StatusInternalServerError, "failed to archive squad")
        return
    }

    if err := tx.Commit(r.Context()); err != nil {
        writeError(w, http.StatusInternalServerError, "failed to commit squad archive")
        return
    }

    // Publish only after commit succeeds — events must never describe state
    // that was rolled back.
    h.publish(protocol.EventSquadDeleted, workspaceID, "member", userID, map[string]any{
        "squad_id":  uuidToString(squad.ID),
        "leader_id": uuidToString(squad.LeaderID),
    })
    w.WriteHeader(http.StatusNoContent)
}
```

Key changes vs. the existing handler:

1. **`slog.Warn` → `slog.Error` + 500.** A transfer failure is no longer non-fatal; it aborts the request and rolls back.
2. **`h.Queries.X(...)` → `qtx.X(...)`.** All writes go through `h.Queries.WithTx(tx)` so they share the same transaction. The pgx pattern mirrors `server/internal/handler/project.go:266-314` (`CreateProject` + `CreateProjectResource` in one tx) — reuse, don't invent.
3. **`defer tx.Rollback(r.Context())`** — pgx makes `Rollback` a no-op after `Commit`, so the unconditional defer is the standard pattern and is safe.
4. **`h.publish` moves below `Commit`.** Real-time consumers must never see "squad deleted" for a squad that's still in the DB. Pre-commit publish would broadcast a state the next-read can contradict.
5. **`parseUUIDOrBadRequest` ok-check.** The current code silently discards the bool from `parseUUIDOrBadRequest(w, userID, "user_id")` (`squad.go:301`). With a transaction in flight, an invalid user_id needs to abort the request *before* the transfer, not pass a zero UUID into `ArchivedBy`. This is also a latent correctness fix carried over from the per-CLAUDE.md "Backend Handler UUID Parsing Convention" — see the project CLAUDE.md note about #1661.

**Step 4: Run the two failure-mode tests**

```bash
cd server && go test ./internal/handler/ -run "TestDeleteSquadFailedTransferLeavesSquadActive|TestDeleteSquadFailedArchiveRollsBackTransfer" -v
```

Expected: PASS.

**Step 5: Re-run the happy-path test from Task 1**

```bash
cd server && go test ./internal/handler/ -run TestDeleteSquadTransfersAllIssuesIncludingTerminalToLeader -v
```

Expected: PASS — wrapping the two queries in a tx must not change happy-path behavior (both writes still happen, count-all + transfer-all semantics preserved).

**Step 6: Run full handler test suite**

```bash
cd server && go test ./internal/handler/...
```

Expected: all pass — no regressions. In particular, any existing test that asserted a 2xx response on `DeleteSquad` must still pass; the transaction is invisible to a happy-path caller.

**Step 7: Commit**

```bash
git add server/internal/handler/squad.go server/internal/handler/squad_test.go
git commit -m "fix(squad): run DeleteSquad transfer + archive in one transaction"
```

---

## Task 3: Frontend — extend `Squad` type + zod schema

**Files:**
- Modify: `packages/core/types/squad.ts:5-18`
- Modify: `packages/core/api/schemas.ts` (add `SquadSchema`) and `packages/core/api/client.ts:1458` (parse with fallback)

**Step 1: Extend the TS type — field is OPTIONAL**

```ts
export interface Squad {
  // ... existing fields ...
  archived_by: string | null;
  /**
   * Total issues currently assigned to this squad (all statuses).
   * Only present on `GET /api/squads/{id}` responses; absent (`undefined`)
   * on list/create/update responses, and absent on older servers that
   * predate the field. Treat `undefined` and `null` identically as "unknown".
   */
  issue_count?: number | null;
}
```

**Why optional, not required `number | null`:** the field is genuinely only emitted by `GetSquad`. Server-side `squadToResponse` in `server/internal/handler/squad.go:44-59` is the converter for *all four* Squad-returning endpoints (`ListSquads`, `CreateSquad`, `UpdateSquad`, `GetSquad`), and only `GetSquad` overrides `resp.IssueCount` after calling it. Combined with `omitempty` on a nil `*int64`, list/create/update responses do not contain the JSON key at all. Declaring the TS field as required `number | null` would lie about those three shapes — TypeScript would happily let consumers read `squad.issue_count` from a `listSquads()` result and get `undefined` at runtime with no warning. Optional makes the contract honest and forces callers that need the value (the archive dialog) to handle the missing case, which it already does.

**Step 2: Add `SquadSchema` in `schemas.ts` (with `.loose()` per local convention)**

```ts
export const SquadSchema = z.object({
  id: z.string(),
  workspace_id: z.string(),
  name: z.string().catch(""),
  description: z.string().catch(""),
  instructions: z.string().catch(""),
  avatar_url: z.string().nullable().catch(null),
  leader_id: z.string(),
  creator_id: z.string(),
  created_at: z.string(),
  updated_at: z.string(),
  archived_at: z.string().nullable().catch(null),
  archived_by: z.string().nullable().catch(null),
  issue_count: z.number().nullable().optional().catch(null),
}).loose();
```

Two convention points enforced here:
1. `.loose()` follows `packages/core/api/schemas.ts:25-31` — without it, zod 4 would silently strip any unknown server-side field a future PR adds; with it, unknown fields pass through unchanged.
2. `issue_count` is `.nullable().optional().catch(null)` — accepts `number`, `null`, or **missing**, and falls back to `null` on any malformed value. This is what makes the schema compatible with all four endpoint shapes: missing on list/create/update, present-with-number on `GetSquad`, present-with-null when the server's count query errors.

**Schema/normalization scope:** wrap **only `getSquad`** with `parseWithFallback` (Step 3). `listSquads`/`createSquad`/`updateSquad` stay as raw `this.fetch` — they are not consumed by any code that reads `issue_count`, and wrapping them now would create a precedent (no other list/create/update method in `client.ts` is schema-wrapped) without a concrete defensive payoff. If we later need defensive parsing for list, we add it then.

**Step 3: Use `parseWithFallback` in `getSquad`**

In `packages/core/api/client.ts:1458`:

```ts
async getSquad(id: string): Promise<Squad> {
  const raw = await this.fetch(`/api/squads/${id}`);
  return parseWithFallback(raw, SquadSchema, raw as Squad, {
    endpoint: `GET /api/squads/${id}`,
  });
}
```

**Step 4: Add a schema test**

In `packages/core/api/schema.test.ts`, add:

```ts
const baseSquad = {
  id: "x", workspace_id: "y", name: "n", description: "", instructions: "",
  avatar_url: null, leader_id: "l", creator_id: "c", created_at: "t",
  updated_at: "t", archived_at: null, archived_by: null,
};

it("SquadSchema accepts a response missing issue_count (old server / list endpoint)", () => {
  const result = parseWithFallback(baseSquad, SquadSchema, null as never, { endpoint: "test" });
  // Field is optional — accept either undefined or null on read; both mean "unknown".
  expect(result.issue_count ?? null).toBeNull();
});

it("SquadSchema parses a response with issue_count: number", () => {
  const result = parseWithFallback(
    { ...baseSquad, issue_count: 3 },
    SquadSchema, null as never, { endpoint: "test" }
  );
  expect(result.issue_count).toBe(3);
});

it("SquadSchema parses issue_count: null (server-side count error path)", () => {
  const result = parseWithFallback(
    { ...baseSquad, issue_count: null },
    SquadSchema, null as never, { endpoint: "test" }
  );
  expect(result.issue_count).toBeNull();
});

it("SquadSchema preserves unknown fields via .loose()", () => {
  const result = parseWithFallback(
    { ...baseSquad, future_field: "x" },
    SquadSchema, null as never, { endpoint: "test" }
  );
  expect((result as Record<string, unknown>).future_field).toBe("x");
});
```

**Step 5: Run and confirm green**

```bash
pnpm --filter @multica/core exec vitest run api/schema.test.ts
```

Expected: PASS.

**Step 6: Typecheck**

```bash
pnpm typecheck
```

Expected: PASS — including `packages/views/squads/components/squad-detail-page.tsx` (existing usage of `Squad` does not destructure `issue_count`).

**Step 7: Commit**

```bash
git add packages/core/types/squad.ts packages/core/api/schemas.ts packages/core/api/client.ts packages/core/api/schema.test.ts
git commit -m "feat(squad): parse issue_count via SquadSchema in getSquad"
```

---

## Task 4: i18n — add `squads.archive_dialog.*` keys

**Files:**
- Modify: `packages/views/locales/en/squads.json`
- Modify: `packages/views/locales/zh-Hans/squads.json`

**Step 1: English keys**

Add a top-level `"archive_dialog"` block to `en/squads.json`:

```json
"archive_dialog": {
  "title": "Archive squad \"{{name}}\"?",
  "description_with_count_one": "{{leader}} will take over {{count}} issue currently assigned to this squad (including closed ones). The squad will no longer appear in the workspace list, and this cannot be undone.",
  "description_with_count_other": "{{leader}} will take over all {{count}} issues currently assigned to this squad (including closed ones). The squad will no longer appear in the workspace list, and this cannot be undone.",
  "description_no_count": "{{leader}} will take over all issues currently assigned to this squad (including closed ones). The squad will no longer appear in the workspace list, and this cannot be undone.",
  "cancel": "Cancel",
  "confirm": "Archive",
  "archiving": "Archiving…"
}
```

**Step 2: Chinese keys** (`zh-Hans/squads.json`)

```json
"archive_dialog": {
  "title": "归档小队「{{name}}」?",
  "description_with_count_one": "{{leader}} 将接管该小队当前 {{count}} 个 issue(含已关闭)。归档后小队不再出现在工作区列表中,该操作不可撤销。",
  "description_with_count_other": "{{leader}} 将接管该小队当前全部 {{count}} 个 issue(含已关闭)。归档后小队不再出现在工作区列表中,该操作不可撤销。",
  "description_no_count": "{{leader}} 将接管该小队当前的全部 issue(含已关闭)。归档后小队不再出现在工作区列表中,该操作不可撤销。",
  "cancel": "取消",
  "confirm": "归档",
  "archiving": "归档中…"
}
```

> Copy intent: be explicit that closed issues are also reassigned, so the user isn't surprised when they later open a closed issue and see the leader's badge instead of the (now archived) squad. See *Archive semantics decision (v3) → Product trade-off acknowledged*.

**Step 3: Add Role combobox keys**

Add to both locales under a new `"role_editor"` block:

```json
// en
"role_editor": {
  "add_role": "+ Add role",
  "search_placeholder": "Type or pick a role…",
  "no_suggestions": "No existing roles. Press Enter to add."
}
// zh-Hans
"role_editor": {
  "add_role": "+ 添加角色",
  "search_placeholder": "输入或选择角色…",
  "no_suggestions": "尚无已用角色,按 Enter 添加"
}
```

**Step 4: Typecheck (i18n key types are generated)**

```bash
pnpm typecheck
```

Expected: PASS — locale type generation picks up new keys automatically (see `packages/views/i18n/`).

**Step 5: Commit**

```bash
git add packages/views/locales/
git commit -m "feat(squad): add i18n keys for archive dialog and role editor"
```

---

## Task 5: Frontend — `ArchiveSquadConfirmDialog`

**Files:**
- Create: `packages/views/squads/components/archive-squad-confirm-dialog.tsx`
- Test: `packages/views/squads/components/archive-squad-confirm-dialog.test.tsx`

**Step 1: Write the failing test**

```tsx
// archive-squad-confirm-dialog.test.tsx
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { ArchiveSquadConfirmDialog } from "./archive-squad-confirm-dialog";

describe("ArchiveSquadConfirmDialog", () => {
  it("shows leader name and count when count is provided", () => {
    render(
      <ArchiveSquadConfirmDialog
        open
        squadName="Squirtle"
        leaderName="Squirtle-Leader"
        issueCount={3}
        onCancel={() => {}}
        onConfirm={async () => {}}
        pending={false}
      />
    );
    expect(screen.getByText(/Squirtle-Leader/)).toBeInTheDocument();
    expect(screen.getByText(/3/)).toBeInTheDocument();
  });

  it("falls back to no-count copy when issueCount is null", () => {
    render(
      <ArchiveSquadConfirmDialog
        open squadName="S" leaderName="L"
        issueCount={null}
        onCancel={() => {}} onConfirm={async () => {}}
        pending={false}
      />
    );
    expect(screen.getByText(/all issues currently assigned/i)).toBeInTheDocument();
  });

  it("disables confirm and cancel while pending", () => {
    render(
      <ArchiveSquadConfirmDialog
        open squadName="S" leaderName="L"
        issueCount={1}
        onCancel={() => {}} onConfirm={async () => {}}
        pending
      />
    );
    expect(screen.getByRole("button", { name: /archiving/i })).toBeDisabled();
  });

  it("calls onConfirm when user clicks Archive", async () => {
    const onConfirm = vi.fn().mockResolvedValue(undefined);
    render(
      <ArchiveSquadConfirmDialog
        open squadName="S" leaderName="L"
        issueCount={1}
        onCancel={() => {}} onConfirm={onConfirm}
        pending={false}
      />
    );
    await userEvent.click(screen.getByRole("button", { name: /^archive$/i }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });
});
```

**Step 2: Run — verify it fails**

```bash
pnpm --filter @multica/views exec vitest run squads/components/archive-squad-confirm-dialog.test.tsx
```

Expected: FAIL — module not found.

**Step 3: Implement the dialog**

```tsx
"use client";

import { Loader2 } from "lucide-react";
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useT } from "../../i18n";

export function ArchiveSquadConfirmDialog({
  open, squadName, leaderName, issueCount,
  onCancel, onConfirm, pending,
}: {
  open: boolean;
  squadName: string;
  leaderName: string;
  issueCount: number | null;
  onCancel: () => void;
  onConfirm: () => Promise<void> | void;
  pending: boolean;
}) {
  const { t } = useT("squads");
  const description =
    issueCount == null
      ? t(($) => $.archive_dialog.description_no_count, { leader: leaderName })
      : t(
          ($) => issueCount === 1
            ? $.archive_dialog.description_with_count_one
            : $.archive_dialog.description_with_count_other,
          { leader: leaderName, count: issueCount }
        );

  return (
    <AlertDialog open={open} onOpenChange={(v) => { if (!v && !pending) onCancel(); }}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            {t(($) => $.archive_dialog.title, { name: squadName })}
          </AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending}>
            {t(($) => $.archive_dialog.cancel)}
          </AlertDialogCancel>
          <AlertDialogAction
            onClick={() => void onConfirm()}
            disabled={pending}
            className="bg-destructive text-white hover:bg-destructive/90"
          >
            {pending ? (
              <><Loader2 className="size-3.5 mr-1 animate-spin" />{t(($) => $.archive_dialog.archiving)}</>
            ) : (
              t(($) => $.archive_dialog.confirm)
            )}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
```

**Step 4: Verify tests pass**

```bash
pnpm --filter @multica/views exec vitest run squads/components/archive-squad-confirm-dialog.test.tsx
```

Expected: PASS.

**Step 5: Commit**

```bash
git add packages/views/squads/components/archive-squad-confirm-dialog.tsx packages/views/squads/components/archive-squad-confirm-dialog.test.tsx
git commit -m "feat(squad): add ArchiveSquadConfirmDialog component"
```

---

## Task 6: Frontend — wire the dialog into `SquadDetailPage`

**Files:**
- Modify: `packages/views/squads/components/squad-detail-page.tsx:209` (header button) and add state at top of `SquadDetailPage`

**Step 1: Add `archiveOpen` state**

Inside `SquadDetailPage()` near the other `useState` calls (e.g. near `showAddMember`):

```tsx
const [archiveOpen, setArchiveOpen] = useState(false);
```

**Step 2: Replace the header button**

Replace line 209:

```tsx
<Button
  size="sm"
  variant="ghost"
  className="text-destructive hover:text-destructive"
  onClick={() => setArchiveOpen(true)}
>
  <Trash2 className="size-3.5 mr-1" />
  {t(($) => $.inspector.archive_button)}
</Button>
```

**Step 3: Render the dialog**

Below the existing dialogs (after `showCreateAgent` block, before the closing `</div>` of the page root):

```tsx
<ArchiveSquadConfirmDialog
  open={archiveOpen}
  squadName={squad.name}
  leaderName={getEntityName("agent", squad.leader_id)}
  // `issue_count` is `number | null | undefined` (optional). Coerce
  // `undefined` (older server / list-shaped data) to `null` so the dialog's
  // "no count" branch covers both cases identically.
  issueCount={squad.issue_count ?? null}
  pending={deleteMut.isPending}
  onCancel={() => setArchiveOpen(false)}
  onConfirm={async () => {
    await deleteMut.mutateAsync();
    setArchiveOpen(false);
  }}
/>
```

**Step 4: Update `deleteMut` to support `mutateAsync`**

Verify in the existing mutation block — `useMutation` always exposes `mutateAsync`. If `deleteMut` is currently typed without `await` callers, no change needed.

**Step 5: Manual verification**

```bash
make dev
```

- Navigate to a squad detail page → click `Archive` → AlertDialog opens with leader name and count
- Click `Cancel` → closes, no mutation
- Click `Archive` → button shows `Loader2`, disabled; after success, redirects to squads list (existing `deleteMut` `onSuccess` behavior)
- Open DevTools → simulate `issue_count: null` response (older-server scenario) → dialog shows fallback copy
- Open a previously-closed issue that had been assigned to the now-archived squad → "Assigned to" badge shows the squad's former leader agent (v3 invariant: no archived-squad pointers remain)

**Step 6: Run typecheck + view tests**

```bash
pnpm typecheck && pnpm --filter @multica/views test
```

**Step 7: Commit**

```bash
git add packages/views/squads/components/squad-detail-page.tsx
git commit -m "feat(squad): wire ArchiveSquadConfirmDialog into squad detail header"
```

---

## Task 7: Frontend — extract `RoleEditor` into Combobox (Command + Popover)

**Files:**
- Modify: `packages/views/squads/components/squad-detail-page.tsx:649-699` (`RoleEditor` implementation) and `:1093-1096` (render call site, add `suggestions` prop)
- Test: extend `packages/views/squads/components/role-editor.test.tsx` (new file)

**Step 1: Write the failing test**

```tsx
// role-editor.test.tsx
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { RoleEditor } from "./squad-detail-page";

describe("RoleEditor (combobox)", () => {
  it("commits the typed role on Enter", async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<RoleEditor value="" suggestions={[]} onSave={onSave} />);
    await userEvent.click(screen.getByRole("button", { name: /add role/i }));
    const input = screen.getByPlaceholderText(/type or pick/i);
    await userEvent.type(input, "Reviewer{Enter}");
    expect(onSave).toHaveBeenCalledWith("Reviewer");
  });

  it("commits a suggestion on click", async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<RoleEditor value="" suggestions={["Reviewer", "Implementer"]} onSave={onSave} />);
    await userEvent.click(screen.getByRole("button", { name: /add role/i }));
    await userEvent.click(screen.getByText("Reviewer"));
    expect(onSave).toHaveBeenCalledWith("Reviewer");
  });

  it("does NOT commit on blur", async () => {
    const onSave = vi.fn();
    render(<RoleEditor value="" suggestions={[]} onSave={onSave} />);
    await userEvent.click(screen.getByRole("button", { name: /add role/i }));
    await userEvent.type(screen.getByPlaceholderText(/type or pick/i), "Partial");
    // Click outside
    await userEvent.click(document.body);
    expect(onSave).not.toHaveBeenCalled();
  });

  it("shows Loader2 while saving", () => {
    render(<RoleEditor value="Reviewer" suggestions={[]} saving onSave={() => Promise.resolve()} />);
    expect(screen.getByTestId("role-editor-saving")).toBeInTheDocument();
  });

  it("renders Pencil icon as a persistent affordance when not saving", () => {
    render(<RoleEditor value="Reviewer" suggestions={[]} onSave={() => Promise.resolve()} />);
    expect(screen.getByTestId("role-editor-pencil")).toBeVisible();
  });
});
```

> Note: `RoleEditor` is currently not exported. **Export it** to allow the test to import it directly (it's an internal helper so export is local — no public API impact).

**Step 2: Run — verify it fails**

```bash
pnpm --filter @multica/views exec vitest run squads/components/role-editor.test.tsx
```

Expected: FAIL — missing `suggestions` prop, no Pencil icon, etc.

**Step 3: Rewrite `RoleEditor`**

Replace lines 645-699:

```tsx
import {
  Command, CommandInput, CommandList, CommandEmpty, CommandGroup, CommandItem,
} from "@multica/ui/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { Pencil, Loader2 } from "lucide-react";

export function RoleEditor({
  value,
  suggestions,
  saving = false,
  onSave,
}: {
  value: string;
  suggestions: string[];
  saving?: boolean;
  onSave: (next: string) => Promise<void>;
}) {
  const { t } = useT("squads");
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");

  // Dedup, drop empty, drop the current value to avoid showing it as a "suggestion".
  const uniqueSuggestions = useMemo(() => {
    const set = new Set<string>();
    for (const s of suggestions) {
      const v = s.trim();
      if (v && v !== value) set.add(v);
    }
    return Array.from(set).sort((a, b) => a.localeCompare(b));
  }, [suggestions, value]);

  const commit = async (next: string) => {
    const trimmed = next.trim();
    if (trimmed === value.trim()) { setOpen(false); return; }
    try {
      await onSave(trimmed);
    } finally {
      setOpen(false);
      setQuery("");
    }
  };

  const trigger = (
    <button
      type="button"
      className="group/role inline-flex items-center gap-1 text-xs text-muted-foreground mt-0.5 text-left hover:text-foreground transition-colors"
      aria-label={value || t(($) => $.role_editor.add_role)}
    >
      <span>{value || t(($) => $.role_editor.add_role)}</span>
      {saving ? (
        <Loader2 data-testid="role-editor-saving" className="size-3 animate-spin" />
      ) : (
        <Pencil data-testid="role-editor-pencil" className="size-3 opacity-60" />
      )}
    </button>
  );

  return (
    <Popover open={open} onOpenChange={(v) => { if (!saving) setOpen(v); }}>
      <PopoverTrigger render={trigger} />
      <PopoverContent className="p-0 w-56" align="start">
        <Command>
          <CommandInput
            value={query}
            onValueChange={setQuery}
            placeholder={t(($) => $.role_editor.search_placeholder)}
            onKeyDown={(e) => {
              if (isImeComposing(e)) return;
              if (e.key === "Enter") { e.preventDefault(); void commit(query); }
              else if (e.key === "Escape") { setOpen(false); setQuery(""); }
            }}
            autoFocus
          />
          <CommandList>
            <CommandEmpty>{t(($) => $.role_editor.no_suggestions)}</CommandEmpty>
            <CommandGroup>
              {uniqueSuggestions.map((s) => (
                <CommandItem key={s} value={s} onSelect={() => void commit(s)}>
                  {s}
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
```

Key differences from original:
- **Blur no longer commits.** Closing the Popover discards the draft.
- **Enter commits the raw query** (the typed text), enabling free-text entry.
- **Click on a `CommandItem` commits that suggestion.**
- **Pencil icon is always visible** next to the role label — addresses "no edit affordance" gap.
- **During `saving`, the Pencil swaps to `Loader2`** and the Popover is locked (open state can't change).
- **IME guard preserved** for Enter.
- **Empty state**: the trigger shows the translated `+ Add role` string instead of the previous italic placeholder.

**Step 4: Wire suggestions in `MembersTab` (render site at :1093-1096)**

Compute the suggestion list once per render of `MembersTab`:

```tsx
const roleSuggestions = useMemo(
  () => members.map((m) => m.role).filter((r): r is string => !!r),
  [members]
);
```

Then pass to each `RoleEditor`:

```tsx
<RoleEditor
  value={m.role ?? ""}
  suggestions={roleSuggestions}
  onSave={async (next) => { await onUpdateRole(m, next); }}
/>
```

> `saving` is **per-member**. We don't currently have a per-member loading state — `updateRoleMut` is shared. Either:
> (a) Leave `saving` always `false` and rely on optimistic mutation feedback (current behavior).
> (b) Track the in-flight member ID in component state and pass `saving={savingMemberId === m.id}`.
>
> **Recommend (b)** — minimal extra state, addresses the gap-report "no spinner during save" point. Implement in `MembersTab`:
>
> ```tsx
> const [savingMemberId, setSavingMemberId] = useState<string | null>(null);
> // wrap onUpdateRole:
> onSave={async (next) => {
>   setSavingMemberId(m.id);
>   try { await onUpdateRole(m, next); }
>   finally { setSavingMemberId(null); }
> }}
> // pass:
> saving={savingMemberId === m.id}
> ```

**Step 5: Run unit tests**

```bash
pnpm --filter @multica/views exec vitest run squads/components/role-editor.test.tsx
```

Expected: PASS.

**Step 6: Run full views test suite + typecheck**

```bash
pnpm typecheck && pnpm --filter @multica/views test
```

Expected: PASS.

**Step 7: Manual verification (UX checks the gap report called out)**

```bash
make dev
```

- Hover a member row → Pencil is **visible without hover** (persistent affordance)
- Click role → Popover opens with current roles as suggestions
- Type "Reviewer", press Enter → commits, Loader2 flashes on the editing row only
- Type partial text, click elsewhere → Popover closes, **no commit**
- Empty role member → trigger shows `+ Add role`
- IME (Chinese): switch to Pinyin input, type partial pinyin → Enter does NOT commit half-word

**Step 8: Commit**

```bash
git add packages/views/squads/components/squad-detail-page.tsx packages/views/squads/components/role-editor.test.tsx
git commit -m "feat(squad): rewrite RoleEditor as Combobox with suggestions"
```

---

## Task 8: E2E coverage

**Files:**
- Create: `e2e/tests/squad-archive-and-role.spec.ts`

**Step 1: Write the E2E spec**

```ts
import { test, expect } from "@playwright/test";
import { loginAsDefault, createTestApi } from "../helpers";
import type { TestApiClient } from "../fixtures";

let api: TestApiClient;

test.beforeEach(async ({ page }) => {
  api = await createTestApi();
  await loginAsDefault(page);
});

test.afterEach(async () => { await api.cleanup(); });

test("archive dialog shows leader name + total assigned issue count (all statuses)", async ({ page }) => {
  const squad = await api.createSquadWithLeader("Test Squad");
  await api.createIssue({ assignee_type: "squad", assignee_id: squad.id, title: "i1", status: "todo" });
  await api.createIssue({ assignee_type: "squad", assignee_id: squad.id, title: "i2", status: "in_progress" });
  await api.createIssue({ assignee_type: "squad", assignee_id: squad.id, title: "i3", status: "done" });

  await page.goto(`/${api.workspaceSlug}/squads/${squad.id}`);
  await page.getByRole("button", { name: /archive/i }).first().click();
  await expect(page.getByRole("alertdialog")).toContainText(squad.leaderName);
  await expect(page.getByRole("alertdialog")).toContainText("3"); // all statuses counted (v3)
});

test("role combobox commits on Enter and surfaces existing roles", async ({ page }) => {
  const squad = await api.createSquadWithLeader("S");
  const m = await api.addAgentToSquad(squad.id, { role: "Reviewer" });
  await api.addAgentToSquad(squad.id, { role: "" });

  await page.goto(`/${api.workspaceSlug}/squads/${squad.id}`);
  await page.getByRole("button", { name: /add role/i }).click();
  // Reviewer should appear as a suggestion (aggregated from other members)
  await expect(page.getByText("Reviewer")).toBeVisible();
  await page.getByRole("combobox").fill("Implementer");
  await page.keyboard.press("Enter");
  await expect(page.getByText("Implementer")).toBeVisible();
});
```

**Step 2: Run**

```bash
pnpm exec playwright test e2e/tests/squad-archive-and-role.spec.ts
```

Expected: PASS (with backend + frontend running).

**Step 3: Commit**

```bash
git add e2e/tests/squad-archive-and-role.spec.ts
git commit -m "test(squad): e2e for archive dialog and role combobox"
```

---

## Task 9: Full verification

**Step 1: Full check**

```bash
make check
```

Expected: all green (typecheck, unit, Go, E2E).

**Step 2: Final commit if any cleanup**

No expected cleanup. If anything, run `pnpm lint --fix` and commit.

---

## Risks / Reviewer attention points

1. **Param shape after `make sqlc`** — `CountIssuesForSquadParams.AssigneeID` may be `pgtype.UUID` directly; do not assume a pointer helper. Verify and adjust handler call.
2. **`issue_count` is `*int64` + `omitempty` on the wire; `number | null | undefined` (optional) in TS** — see *API Compatibility Notes* and Task 3 for the rationale. Old-server (no field) and count-error (null) collapse to the same "fallback copy" path in the dialog.
3. **`RoleEditor` export change** — internal helper, no consumers outside this file. Verify with `grep -rn "RoleEditor" packages/`.
4. **Per-member saving state** — recommended approach adds local state to `MembersTab`. If the team prefers truly stateless, fall back to "always false" and rely on optimistic mutation behavior.
5. **Archive is count-all + transfer-all (v3, see *Archive semantics decision*), and the invariant is enforced transactionally (see Task 2b).** After a successful `DeleteSquad`, no issue (active or terminal) retains `assignee_id=<archived squad>` — `TransferSquadAssignees` and `ArchiveSquad` commit together or not at all. This eliminates two structural issues that v2 left open: (a) `useActorName` rendering "Unknown Squad" for closed issues whose squad was archived (`packages/core/workspace/hooks.ts:11,23` + `squad.sql:13`), and (b) reopening a done/cancelled issue resurrecting an archived-squad assignee through the status-only update path (`issue.go:1453-1496,1563-1570`). It also eliminates the half-archived / half-transferred partial-write states the previous best-effort handler allowed (`squad.go:292-309` pre-Task-2b — `slog.Warn` on transfer error, archive failure after a committed transfer). Product trade-off: a closed issue's historical "Assigned to" badge now shows the leader instead of the (archived) squad. Acceptable — see decision section for rationale.
6. **DeleteSquad fault-injection harness** — Task 2b's two regression tests require a way to force `TransferSquadAssignees` or `ArchiveSquad` to fail mid-handler. The current `squad_test.go` doesn't exercise tx-rollback paths. Reuse an existing fault-injection or interface-wrapping pattern from elsewhere in `server/internal/handler/*_test.go` (search for `Begin.*error`, `tx.*rollback`, or interface-typed `Queries` test doubles). If no precedent exists, escalate before inventing a new mocking framework just for these two tests — a lighter alternative (e.g. context cancellation after the first query) may be sufficient to drive the failure paths.
7. **No new DB index** (see *Index assumption*). `idx_issue_assignee (assignee_type, assignee_id)` is sufficient for `CountIssuesForSquad` at realistic squad cardinality (low-hundreds of issues per squad). If profiling later shows the query becomes hot, add the partial index suggested in that section.
8. **API-compat checklist:**
   - [x] `issue_count` is additive (old clients ignore it)
   - [x] TS field declared optional — list/create/update responses do not lie about shape
   - [x] Schema is `.loose()` (matches `schemas.ts:25` convention) and tolerates missing / null / numeric values
   - [x] `parseWithFallback` wired in `getSquad`; list/create/update intentionally not wrapped (no consumer)
   - [x] Schema tests cover: missing field, present `null`, present number, unknown future field passthrough
   - [x] Enum drift n/a (no enums in this schema)
   - [x] No reliance on a single boolean for UI affordance — dialog has fallback copy when count is `null` or `undefined`
