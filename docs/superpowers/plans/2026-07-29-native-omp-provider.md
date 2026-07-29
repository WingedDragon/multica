# Native OMP Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 Oh My Pi 作为 Multica 的原生 `omp` provider 接入，支持 headless JSON 执行、模型发现、session 恢复、自动探测和 workspace skill 注入。

**Architecture:** 原生 `omp` provider 用现有 `piBackend` 的两种固定 Pi-compatible runtime mode 执行；`pi.go` 原位保留 JSONL 扫描循环，只增加 session event、runtime mode 与 launch hook。新增 `omp.go` 只承载 OMP 的 argv/session helpers：`--session-dir`、`--resume <id>`、完整 selector `--model` 和 `--thinking`。模型发现仍单独执行 `omp models --json`；daemon 复用现有 built-in runtime 探测和任务调度路径，不增加 custom-profile 特殊分支。

**Tech Stack:** Go、Chi daemon、`server/pkg/agent` backend 接口、JSONL stdout events、PostgreSQL migrations、React/Next.js runtime model picker、pnpm/Vitest。

## Global Constraints

- `omp` 必须是独立的 provider key；原有 Pi provider 和 Pi custom runtime profile 必须继续工作。
- 不修改 `server/internal/daemon/daemon.go` 的通用任务调度或 custom-profile launch 流程。
- `pi.go` 的 JSONL 扫描循环不得移动到新文件，也不得复制到 OMP 文件；只增加 Pi/OMP 两个固定 runtime mode、OMP `session` event 的 `id` 捕获与小型 launch hook。
- `omp.go` 只定义固定 OMP argv/session helper；不得成为任意 CLI、协议或 flag 的用户可配置 runner。
- OMP headless invocation 固定使用 `omp -p --mode json --session-dir <dir> [--resume <id>] [--model <provider/model>] [--thinking <level>] <prompt>`，prompt 必须作为最后一个 argv，不得经过 shell 拼接。
- 模型发现固定使用 `omp models --json`；保存和传回 `provider/model` selector 时不得拆掉 provider 前缀。
- OMP 仍标记为 `ResumeRejectionUndetectable`，直到存在已验证的结构化 rejection 信号；不得用 Pi 错误文字猜测 `Result.ResumeRejected`。
- 不把 OMP TUI、RPC、ACP、扩展主题或模型 catalog 编辑纳入本次实现。
- 所有 Go 测试使用 fake executable 或 fixture，不依赖真实 OMP、账号、网络或本机配置。
- 新增 migration 使用编号 `233`，不增加外键或级联动作。
- 不对共享 provider 文件做全文件格式化或无关重排；共享文件只添加最小注册分支。
- 每个任务先写可失败的行为测试，再写最小实现；跳过 formatter、linter 和项目级全量测试，最后统一验证。

## File Map

### Agent execution

- Modify: `server/pkg/agent/pi.go` — 增加私有 Pi/OMP fixed runtime mode，原位保留 JSONL 执行主体并捕获 OMP `session` event ID。
- Create: `server/pkg/agent/omp.go` — OMP 固定 argv builder、blocked args 和 session-directory helper；不包含事件循环。
- Modify: `server/pkg/agent/agent.go` — 注册 `omp`、更新 supported type 与 resume-detection capability 文案。
- Create: `server/pkg/agent/omp_test.go` — fake OMP executable 的命令、argv、事件、session 和 capability 测试。
- Modify: `server/pkg/agent/pi_test.go` — 仅在 runtime mode 影响既有 Pi contract 时扩展回归断言。

### Model discovery

- Create: `server/pkg/agent/omp_models.go` — OMP catalog JSON schema、解析和命令发现。
- Modify: `server/pkg/agent/models.go` — 增加 `omp` dispatch 和 discovery cache key。
- Create: `server/pkg/agent/omp_models_test.go` — selector、thinking、去重和错误边界测试。

### Daemon and runtime profile

- Modify: `server/internal/daemon/config.go` — `omp` PATH/model env、probe、login-shell command list和错误提示。
- Modify: `server/internal/daemon/config_test.go` — 环境变量和 probe 覆盖测试；只在现有 helper 必须扩展时改动。
- Create: `server/internal/daemon/omp_config_test.go` — OMP probe 的独立 fake executable 测试，减少共享 config_test 冲突。
- Create: `server/migrations/233_runtime_profile_add_omp.up.sql` — runtime profile CHECK 增加 `omp`。
- Create: `server/migrations/233_runtime_profile_add_omp.down.sql` — 恢复不含 `omp` 的 CHECK。
- Create: `server/internal/handler/runtime_profile_omp_test.go` — profile protocol family 接受 `omp` 的 DB/handler 回归测试（按现有测试数据库约定）。

### Skill injection and UI

- Modify: `server/internal/daemon/execenv/context.go` — `omp` explicitly maps to `.omp/skills/`, while `pi` remains `.pi/skills/`; reuse the existing sidecar manifest/cleanup mechanism.
- Create: `server/internal/daemon/execenv/omp_skill_test.go` — skill writing, `.pi` non-write, existing-content protection and cleanup tests.
- Modify: `server/internal/daemon/daemon.go:3873-3877` — add only the `omp` friendly display-name entry; do not touch generic scheduling.
- Modify: `packages/core/runtimes/display.ts:43-46` — mirror the daemon display-name mapping with `omp: "OMP"`.
- Modify: `packages/core/runtimes/display.test.ts` — assert `providerDisplayName("omp")` and aliased runtime labels.
- Modify: `apps/docs/content/docs/providers.mdx` — English capability matrix and OMP section.
- Modify: `apps/docs/content/docs/providers.zh.mdx` — Chinese capability matrix and OMP section.
- Modify: `apps/docs/content/docs/providers.ja.mdx` — Japanese capability matrix and OMP section.
- Modify: `apps/docs/content/docs/providers.ko.mdx` — Korean capability matrix and OMP section.

---

### Task 1: Add OMP through a bounded Pi-compatible runtime mode

**Files:**
- Modify: `server/pkg/agent/pi.go`
- Create: `server/pkg/agent/omp.go`
- Modify: `server/pkg/agent/agent.go`
- Create: `server/pkg/agent/omp_test.go`
- Test: `server/pkg/agent/pi_test.go` and `server/pkg/agent/omp_test.go`

**Interfaces:**
- Consumes: existing `piBackend`, `buildPiArgs`, `newPiSessionPath`, `ensurePiSessionFile`, `Config`, `ExecOptions`, `Session`, `Message`, `Result`, `TokenUsage`, and `filterCustomArgs`.
- Produces: private fixed Pi/OMP runtime mode selected by `newPiBackend(Config) *piBackend` and `newOMPCompatibleBackend(Config) *piBackend`; `buildOMPArgs(prompt, sessionDir string, opts ExecOptions, logger *slog.Logger) []string`; `ompSessionDir() (string, error)`; `New("omp", cfg)` returns the OMP mode. Zero-value `piBackend` retains Pi’s existing defaults.

- [ ] **Step 1: Write fake-executable OMP behavior tests.**

  Add a temporary executable named `omp` on PATH that records argv and emits deterministic Pi-compatible JSONL, including:

  ```json
  {"type":"session","id":"omp-session-1"}
  ```

  Add:

  ```go
  func TestOMPBackend_UsesOMPDefaultExecutableAndJSONProtocol(t *testing.T)
  func TestOMPBackend_PassesSelectorThinkingAndBlockedArgs(t *testing.T)
  func TestOMPBackend_MapsStreamSessionIDAndResume(t *testing.T)
  func TestOMPProviderResumeRejectionIsUndetectable(t *testing.T)
  ```

  Assert `New("omp", Config{})` starts `omp`; argv includes `-p --mode json --session-dir <dir>`; selector `openai-codex/gpt-5.6-luna` is passed unchanged as one `--model` value with no `--provider`; `ThinkingLevel: "high"` becomes `--thinking high`; `ResumeSessionID: "prior-1"` becomes `--resume prior-1`; custom args cannot replace `-p`, `--mode`, `--session-dir`, `--resume`, `--model`, `--thinking` or `--provider`; prompt stays last; the stream `session.id` becomes `Result.SessionID`; a resumed run with no replacement session event retains `prior-1`; and `ResumeRejectionUndetectable("omp")` is true.

- [ ] **Step 2: Run the new OMP tests to verify they fail.**

  Run from `server/`:

  ```bash
  go test ./pkg/agent -run 'TestOMP' -count=1
  ```

  Expected: FAIL because `New("omp", ...)`, OMP argv/session helpers, OMP stream session handling and the OMP capability declaration do not exist.

- [ ] **Step 3: Add OMP-only helpers in `omp.go`.**

  Implement `ompSessionDir` as `~/.multica/omp-sessions` and create that directory before launch. Define `ompBlockedArgs` for `-p`, `--print`, `--mode`, `--session`, `--session-dir`, `--continue`, `--resume`, `--model`, `--thinking` and `--provider`. Implement `buildOMPArgs` in this exact order: `-p`, `--mode json`, `--session-dir <dir>`, optional `--resume <ResumeSessionID>`, optional `--model <Model>`, optional `--thinking <ThinkingLevel>`, filtered custom args, prompt. Do not split model selectors and do not add a shell layer.

- [ ] **Step 4: Add the fixed runtime mode inside `pi.go`.**

  Add private Pi and OMP constructors and a private fixed runtime mode selector. Keep the existing scanner loop in `pi.go`. Route launch preparation to Pi’s existing file-path session plus `buildPiArgs`, or OMP’s directory/ID session plus `buildOMPArgs`. Extend the existing event struct:

  ```go
  ID string `json:"id,omitempty"`
  ```

  Add one `case "session"` that, only for OMP mode, captures the emitted ID and sends a running `MessageStatus` with that ID. For OMP, `Result.SessionID` is the captured stream ID, falling back to `ResumeSessionID`; Pi continues returning the file path. Update default executable, error text, stderr prefix, start/finish log label and timeout/retry wording from the fixed mode without changing Pi’s zero-value behavior.

- [ ] **Step 5: Register OMP without an alias.**

  In `agent.go`, use `newPiBackend(c)` for `case "pi"` and `newOMPCompatibleBackend(c)` for `case "omp"`. Add `omp` to `SupportedTypes`, the unknown-provider text and `resumeRejectionUndetectable`. Do not add a public generic backend type and do not copy the JSONL loop.

- [ ] **Step 6: Run Pi and OMP regression tests.**

  ```bash
  go test ./pkg/agent -run 'Test(OMP|Pi|Supported|ResumeRejection)' -count=1
  ```

  Expected: PASS. Pi’s executable, file-path session, parser and fixtures remain unchanged; OMP uses its native session directory/ID, model selector and thinking flags while sharing the original JSONL loop.

- [ ] **Step 7: Commit the merge-safe native backend.**

  ```bash
  git add server/pkg/agent/pi.go server/pkg/agent/pi_test.go server/pkg/agent/omp.go server/pkg/agent/omp_test.go server/pkg/agent/agent.go
  git commit -m "feat(agent): add native OMP backend"
  ```

### Task 2: Implement OMP model discovery

**Files:**
- Create: `server/pkg/agent/omp_models.go`
- Modify: `server/pkg/agent/models.go`
- Create: `server/pkg/agent/omp_models_test.go`

**Interfaces:**
- Consumes: `Model`, `ModelThinking`, `ThinkingLevel`, `cachedDiscovery`, `discoveryCacheKey`.
- Produces: `discoverOMPModels(context.Context, string) ([]Model, error)` and `parseOMPModels([]byte) ([]Model, error)`; `ListModels("omp", executable)` calls the discoverer using an executable-aware cache key.

- [ ] **Step 1: Add catalog fixtures and parser tests.**

  Use a fixture containing `provider`, `id`, `selector`, `name`, and `thinking`, plus records with a missing selector, duplicate selector, missing name, empty thinking list and one malformed record. Add tests:

  ```go
  func TestParseOMPModels_MapsSelectorsAndThinking(t *testing.T)
  func TestParseOMPModels_DeduplicatesSelectors(t *testing.T)
  func TestParseOMPModels_FallsBackToProviderAndID(t *testing.T)
  func TestParseOMPModels_RejectsMalformedTopLevelJSON(t *testing.T)
  func TestDiscoverOMPModels_ReturnsCommandErrors(t *testing.T)
  func TestListModels_OMPIsolatedByExecutablePath(t *testing.T)
  ```

- [ ] **Step 2: Run parser tests before implementation.**

  ```bash
  go test ./pkg/agent -run 'Test(ParseOMP|DiscoverOMP|ListModels_OMP)' -count=1
  ```
  Expected: FAIL because the OMP parser and dispatch do not exist.

- [ ] **Step 3: Implement strict top-level catalog parsing.**

  Decode `{ "models": [...] }` into a private OMP catalog struct. Use `selector` when non-empty; otherwise require non-empty provider and id and construct `provider/id`. Preserve selector bytes semantically without normalization. Convert thinking values to `ThinkingLevel{Value: value, Label: title-case(value)}`; use nil thinking when the list is empty. Skip malformed individual entries, deduplicate by final ID and preserve first-seen order.

- [ ] **Step 4: Implement command discovery.**

  Resolve empty executable to `omp`, return a wrapped error when the executable is not found, and otherwise run `omp models --json` with a 15-second child context. Capture stdout and stderr for diagnostics; return a non-nil error on malformed top-level JSON, command failure or non-zero exit. Add the `omp` branch to `ListModels` with `discoveryCacheKey("omp", executablePath)`. The daemon/UI may still render the existing manual-entry fallback after the request reports discovery failure.

- [ ] **Step 5: Run OMP and existing discovery tests.**

  ```bash
  go test ./pkg/agent -run 'Test(ParseOMP|DiscoverOMP|ListModels_OMP|Model)' -count=1
  ```

  Expected: PASS; existing Pi discovery tests remain green and Pi cache entries are not reused for OMP.

- [ ] **Step 6: Commit model discovery.**

  ```bash
  git add server/pkg/agent/omp_models.go server/pkg/agent/omp_models_test.go server/pkg/agent/models.go
  git commit -m "feat(agent): discover OMP models"
  ```

### Task 3: Register and persist OMP daemon runtimes

**Files:**
- Modify: `server/internal/daemon/config.go`
- Create: `server/internal/daemon/omp_config_test.go`
- Create: `server/migrations/233_runtime_profile_add_omp.up.sql`
- Create: `server/migrations/233_runtime_profile_add_omp.down.sql`
- Create: `server/internal/handler/runtime_profile_omp_test.go`

**Interfaces:**
- Consumes: existing `probe`, `AgentEntry`, `defaultAgentCommandNames`, runtime profile handler and migration test helpers.
- Produces: config discovery entry `agents["omp"]`, environment overrides `MULTICA_OMP_PATH`/`MULTICA_OMP_MODEL`, and a database CHECK constraint accepting protocol family `omp`.

- [ ] **Step 1: Add config behavior tests with fake binaries.**

  Add tests:

  ```go
  func TestLoadConfig_DiscoversOMP(t *testing.T)
  func TestLoadConfig_OMPPathOverrideWins(t *testing.T)
  func TestLoadConfig_OMPModelOverrideIsStored(t *testing.T)
  func TestDefaultAgentCommandNamesIncludesOMP(t *testing.T)
  ```

  Pin all unrelated provider paths to missing files, place a fake `omp` on PATH, and assert the returned entry has provider `omp`, command `omp`, resolved executable path and configured model.

- [ ] **Step 2: Run daemon config tests to verify they fail.**

  ```bash
  go test ./internal/daemon -run 'Test(LoadConfig_OMP|DefaultAgentCommandNames)' -count=1
  ```

  Expected: FAIL because the probe loop and command-name list do not include OMP.

- [ ] **Step 3: Add OMP to the probe loop and error text.**

  Insert `probe("MULTICA_OMP_PATH", "omp", "MULTICA_OMP_MODEL")`, add `omp` to `defaultAgentCommandNames`, and update the no-agent installation hint. Do not modify resolve/self-heal algorithms.

- [ ] **Step 4: Add migration files.**

  `233_runtime_profile_add_omp.up.sql` must drop the current `runtime_profile_protocol_family_check` and recreate it with all existing values plus `omp`. The down migration must recreate the prior accepted set without `omp`. Keep this a constraint-only migration; do not add indexes, FKs or data cleanup.

- [ ] **Step 5: Add profile persistence regression coverage.**

  Follow the existing runtime profile test helper to insert `protocol_family="omp"`, assert creation succeeds, and assert read/update returns `omp`. Keep the migration test focused on the exact up/down SQL shape used by migration 202: drop the named CHECK, recreate the complete provider list with `omp` on up and without `omp` on down, and use `NOT VALID` so historical rows remain compatible.

- [ ] **Step 6: Run focused daemon and handler tests.**

  ```bash
  go test ./internal/daemon -run 'Test(LoadConfig_OMP|DefaultAgentCommandNames)' -count=1
  go test ./internal/handler -run 'Test.*RuntimeProfile.*OMP|Test.*RuntimeProfile' -count=1
  ```

  Expected: PASS; existing provider probe and profile tests remain green.

- [ ] **Step 7: Commit daemon and migration support.**

  ```bash
  git add server/internal/daemon/config.go server/internal/daemon/omp_config_test.go server/migrations/233_runtime_profile_add_omp.up.sql server/migrations/233_runtime_profile_add_omp.down.sql server/internal/handler/runtime_profile_omp_test.go
  git commit -m "feat(daemon): detect and persist OMP runtimes"
  ```

### Task 4: Inject OMP workspace skills

**Files:**
- Modify: `server/internal/daemon/execenv/context.go`
- Create: `server/internal/daemon/execenv/omp_skill_test.go`

**Interfaces:**
- Consumes: `resolveSkillsDir`, `skillsDirPath`, `sidecarManifest`, `CleanupSidecars`, existing Pi skill tests.
- Produces: `skillsDirPath(workDir, "omp") == filepath.Join(workDir, ".omp", "skills")`; Pi remains `filepath.Join(workDir, ".pi", "skills")`.

- [ ] **Step 1: Add skill path and cleanup tests.**

  Assert an assigned skill is written to `<workdir>/.omp/skills/<slug>/SKILL.md`, no Multica skill exists at `<workdir>/.pi/skills/<slug>/SKILL.md`, pre-existing `.omp` files are preserved, manifest cleanup removes only Multica-managed files/directories, and the OMP path is distinct from the fallback `.agent_context/skills` path.

- [ ] **Step 2: Run the focused skill tests to verify they fail.**

  ```bash
  go test ./internal/daemon/execenv -run 'Test.*OMP.*Skill|Test.*Skill.*Cleanup' -count=1
  ```

  Expected: FAIL because `omp` currently maps to `.pi/skills`.

- [ ] **Step 3: Separate the explicit `omp` switch case.**

  Return `.pi/skills` only for `pi`; return `.omp/skills` only for `omp`. Do not dual-write, change any other provider mapping, or alter manifest behavior.

- [ ] **Step 4: Run Pi and OMP skill tests.**

  ```bash
  go test ./internal/daemon/execenv -run 'Test.*(Pi|OMP).*Skill|Test.*Skill.*Cleanup' -count=1
  ```

  Expected: PASS with unchanged Pi behavior and OMP injection only below `.omp/skills`.

- [ ] **Step 5: Commit skill injection.**

  ```bash
  git add server/internal/daemon/execenv/context.go server/internal/daemon/execenv/omp_skill_test.go
  git commit -m "fix(execenv): inject OMP workspace skills"
  ```

### Task 5: Add provider copy and documentation

**Files:**
- Modify: `server/internal/daemon/daemon.go:3873-3877`
- Modify: `packages/core/runtimes/display.ts:43-46`
- Modify: `apps/docs/content/docs/providers.mdx`
- Modify: `apps/docs/content/docs/providers.zh.mdx`
- Modify: `apps/docs/content/docs/providers.ja.mdx`
- Modify: `apps/docs/content/docs/providers.ko.mdx`

**Interfaces:**
- Consumes: backend provider name `omp`, model selector contract and skill path from Tasks 1–4.
- Produces: user-visible OMP label, capability matrix row, installation/configuration instructions and no stale claim that OMP is merely Pi.

- [ ] **Step 1: Add provider display-name regression coverage.**

  Extend `packages/core/runtimes/display.test.ts` so `providerDisplayName("omp")` returns `OMP` and `runtimeDisplayLabel` appends `"(OMP)"` for a custom runtime alias. Add the matching `omp: "OMP"` entry to `server/internal/daemon/daemon.go` and keep the two maps byte-for-byte equivalent for this provider.

- [ ] **Step 2: Update the four existing provider pages.**

  Modify `apps/docs/content/docs/providers.mdx`, `providers.zh.mdx`, `providers.ja.mdx`, and `providers.ko.mdx`. Add OMP to each capability matrix and provider section with `omp models --json`, `MULTICA_OMP_PATH`, `MULTICA_OMP_MODEL`, `provider/model` selectors, `--session-dir` / `--resume <session-id>`, `.pi/skills/`, and headless JSON mode. Do not create a new `providers/` directory.

- [ ] **Step 3: Run the focused docs and display tests.**

  ```bash
  pnpm -C apps/docs typecheck
  pnpm -C packages/core test -- display.test.ts
  ```

  Expected: PASS with the four existing docs pages compiling and OMP display labels covered.

- [ ] **Step 4: Commit documentation and display copy separately.**

  ```bash
  git add server/internal/daemon/daemon.go packages/core/runtimes/display.ts packages/core/runtimes/display.test.ts apps/docs/content/docs/providers.mdx apps/docs/content/docs/providers.zh.mdx apps/docs/content/docs/providers.ja.mdx apps/docs/content/docs/providers.ko.mdx
  git commit -m "docs: document native OMP runtime"
  ```

### Task 6: Verify the end-to-end contract and merge boundary

**Files:**
- Modify only if focused verification exposes a contract failure in Tasks 1–5.
- Test: all files changed by Tasks 1–5.

**Interfaces:**
- Consumes: registered `omp` provider, fake CLI fixtures, model catalog parser, daemon probe and skill sidecar.
- Produces: evidence that the native OMP path works end-to-end without modifying daemon generic scheduling.

- [ ] **Step 1: Run all focused Go tests for the changed packages.**

  ```bash
  cd server
  go test ./pkg/agent ./internal/daemon ./internal/daemon/execenv ./internal/handler -count=1
  ```

  Expected: PASS.

- [ ] **Step 2: Run the repository’s permanent feature checks.**

  ```bash
  pnpm typecheck
  pnpm test
  make test
  ```

  Expected: PASS; if an existing environment-dependent suite is unavailable, report the exact command and failure rather than substituting a narrower claim.

- [ ] **Step 3: Perform an OMP smoke test without changing tracked files.**

  With a temporary workspace and temporary `--session-dir`, execute one headless JSON prompt and verify: process exits successfully, JSONL contains a `session` event and start/update/end events, the stream has a non-empty session ID, the temporary session directory receives OMP state, and `omp models --json` contains at least one selector. Do not alter the user's session files or global OMP configuration.

- [ ] **Step 4: Review the final diff for merge-safe scope.**

  Confirm no changes touch `server/internal/daemon/daemon.go` generic scheduling, no Pi custom profile alias was added, no full provider-file formatting occurred, migrations contain no FK/cascade/index changes, and commits remain independently cherry-pickable.

- [ ] **Step 5: Commit only verification-driven corrections.**

  If a correction is required, make one focused commit using `feat(agent):`, `fix(agent):`, `fix(daemon):`, or `test(agent):` as appropriate. Do not squash the intentionally separated provider, discovery, daemon, skill, migration and docs commits.
