# 原生 OMP Provider 设计规格

- 日期：2026-07-29
- 状态：修订待用户审查
- 范围：将 Oh My Pi（OMP）作为 Multica 的一等 agent provider 接入

## 1. 背景与问题

当前 Multica 将 Pi-compatible CLI 通过 `pi` provider 处理。OMP 可以使用与 Pi 类似的非交互 JSON 事件流，但它不是 Pi 的别名：

- OMP 的可执行文件名是 `omp`；
- OMP 的模型目录命令是 `omp models --json`，不是 `pi --list-models`；
- OMP 的模型标识使用 `provider/model` selector；
- OMP 有自己的 provider 身份、版本和配置边界。

现有 `pi` custom runtime profile 可以临时执行 OMP，但模型发现、自动探测、skill 路径和 provider 展示都无法表达 OMP 的真实能力。

## 2. 决策

新增独立的原生 `omp` provider。执行层不抽取、不搬迁或大范围重构 `pi.go`：现有 `piBackend` 只增加一个私有且固定的 Pi-compatible runtime profile，用于在 Pi 与 OMP 两个已验证的 JSONL 协议实现间选择命令、session 策略、日志/错误 provider 名和结果 session ID。

OMP 17.1.8 的实测 CLI 契约与 Pi 不同：

- OMP 使用 `--session-dir <directory>` 与 `--resume <session-id>`，不使用 Pi 的 `--session <file>`；
- OMP 以完整 `provider/model` selector 传给 `--model`，不拆成 Pi 的 `--provider` / `--model`；
- OMP 原生支持 `--thinking <level>`。

核心边界：

```text
omp provider
├── 独立命令探测：omp
├── 独立模型发现：omp models --json
├── 独立 provider 注册和能力声明
├── OMP argv/session helper（新增 omp.go）
└── 原位复用 pi.go 的 JSONL 事件循环（不移动主体）
```

`omp` 不是 `pi` 的注册别名：daemon/runtime/model catalog/数据库 protocol family 仍使用独立的 `omp`。共享事件循环只是内部实现细节。原有 `pi` provider 和已有 Pi custom runtime profile 继续工作，不迁移、不删除、不增加兼容别名。

## 3. 目标

1. daemon 能自动发现 PATH 上的 `omp`。
2. 支持 `MULTICA_OMP_PATH` 和 `MULTICA_OMP_MODEL` 覆盖默认路径和模型。
3. Agent 创建、runtime 注册和任务派发可以直接使用 `omp` provider。
4. OMP 任务使用非交互 JSON 模式，并将事件映射为 Multica 的消息、状态、结果和 usage。
5. UI 的 runtime 模型列表展示 OMP 返回的模型，支持 `selector` 和 thinking levels。
6. 支持 OMP session ID 恢复，并正确保留不可检测 resume-rejection 的能力边界。
7. Multica 分配的 skills 写入 OMP 实际扫描的工作区目录。
8. 新增回归测试，且不依赖开发机上真实安装的 OMP。
9. 改动可拆分、可重放，尽量降低与 upstream provider 代码的合并冲突。

## 4. 非目标

- 不重构所有 agent backend 为新的通用框架，也不抽取或移动 `pi.go` 的 JSON 事件循环。
- 不把 Pi backend 变成可运行任意 CLI 的通用 runner；私有配置只允许固定的 Pi 与 OMP 两个已验证协议 profile。
- 不支持 OMP TUI、RPC、ACP 或交互式终端模式；Multica daemon 只使用一次性 JSON 执行模式。
- 不在本次接入 OMP 专属的扩展、主题、工作流或模型 catalog 编辑功能。
- 不改变 React Query、Zustand 或 runtime 模型请求协议。
- 不删除现有 Pi custom profiles。

## 5. Backend 设计

### 5.1 Provider 注册

在 `server/pkg/agent/agent.go` 中：

- 将 `omp` 加入 `SupportedTypes`；
- 在 `New` 中返回配置为 OMP 的 Pi-compatible backend；
- 更新 supported-type 注释和未知 provider 错误文本；
- 将 OMP 加入 `resumeRejectionUndetectable`：当前协议没有可验证的拒绝 resume 结构化信号，必须 fail-closed，不得把 `false` 误读为“确认可恢复”。

不新增 `ompBackend` 或公共通用 executor。`pi.go` 的 `piBackend` 增加一个私有固定 runtime profile；`omp.go` 提供 OMP 专属的 profile factory、argv builder 与 session-directory helper。profile 只能选择以下已验证行为：

- Pi：默认 executable `pi`，Pi file-path session，既有 `buildPiArgs`；
- OMP：默认 executable `omp`，`--session-dir` / `--resume <session-id>`，`buildOMPArgs`；
- provider 名：`pi` 或 `omp`，用于日志、错误与结果归因。

`New("pi")` 与零值 `piBackend` 保持当前 Pi 行为；`New("omp")` 使用 OMP profile。两者继续原位共用 `pi.go` 中已验证的 JSON line 扫描、`agent_start`、`turn_start`、`message_update`、tool event、`turn_end`、文本清洗、usage 汇总和进程生命周期实现。OMP stream 的 `session` 事件只新增一个窄分支，用其 `id` 作为 `Result.SessionID` 和早期 status 的 session ID。不会搬迁扫描器、复制 Pi backend，或加入可由用户配置的 protocol/flag 模板。

### 5.2 OMP 启动命令

默认启动形态：

```bash
omp -p --mode json --session-dir <session-directory> [--resume <session-id>] [--model <provider/model>] [--thinking <level>] <prompt>
```

实现要求：

- executable 优先使用 `Config.ExecutablePath`，为空时使用 OMP profile 的 `omp`；
- 使用 `exec.LookPath` 验证 executable；
- prompt 作为位置参数传递，不通过 shell 拼接；
- 继续使用工作目录、超时、环境变量、窗口隐藏和进程树回收约定；
- `--session-dir` 始终为 `~/.multica/omp-sessions`，由 daemon 在启动前创建；
- `ResumeSessionID` 非空时原样传给 `--resume`；
- `--model` 接收完整 `provider/model` selector，不拆分；
- `ThinkingLevel` 非空时原样传给 `--thinking`，为空时不传，保留 OMP 默认值；
- `-p`、`--mode`、`--session-dir`、`--resume`、`--model`、`--thinking` 与 Pi 遗留 `--provider` 全部是 blocked custom args。

OMP 事件中间输出继续进入 `Session.Messages`，最终答案只进入 `Result.Output`，遵守现有 channel/issue-comment 的最终答案边界。

### 5.3 Session 恢复

- 新任务启动前确保 `~/.multica/omp-sessions/` 存在；OMP 自己在该目录生成 session；
- OMP JSONL 的 `session` 事件提供 `id`；backend 保存这个 ID 为 `Result.SessionID`，后续运行将它原样传给 `--resume`；
- resumed run 若未重新发出 session 事件，保留请求的 `ResumeSessionID` 作为结果 session ID；
- 因当前 OMP JSONL 协议没有已验证的 resume-rejection 事件或错误码，`ResumeRejectionUndetectable("omp")` 必须为 true；不得以 Pi 文字错误或猜测标记 `Result.ResumeRejected`；
- 当后续 OMP 提供稳定的拒绝信号时，再单独加入精确检测和 fresh-session fallback 测试。

## 6. 模型发现

在 `server/pkg/agent/models.go` 中新增 OMP 专属 discovery：

```bash
omp models --json
```

解析规则：

- 顶层读取 `models` 数组；
- `selector` 非空时优先作为 Multica `Model.ID`；
- 没有 `selector` 时使用 `provider/id` 组合；
- `name` 映射到 `Model.Label`，为空时回退到 selector 或 id；
- `provider` 映射到 `Model.Provider`；
- `thinking` 映射到 `ThinkingLevel` 列表；
- 目录中重复 selector 去重，保持首次出现顺序；
- 无法解析的单条模型跳过并记录可诊断信息；
- 顶层 JSON 损坏、命令失败或退出非零返回 discovery error，让 daemon/UI 按现有失败路径处理；
- 使用现有 discovery cache 约定，但 cache key 必须包含 provider 和 executable path，避免 Pi 与 OMP 或多个 OMP 安装相互污染。

OMP 返回的 selector 示例：

```text
openai-codex/gpt-5.6-luna
```

Multica 不拆分 selector，也不把它规范化为仅保留模型 id；保存和传递时保持 OMP 原始选择器。

## 7. Daemon 自动探测

在 `server/internal/daemon/config.go` 中新增：

```text
MULTICA_OMP_PATH
MULTICA_OMP_MODEL
```

默认命令名为 `omp`，并加入 `defaultAgentCommandNames`，保证 GUI/LaunchAgent 启动的 daemon 也能通过登录 shell fallback 找到 OMP。

注册结果使用现有 `AgentEntry` 结构，包含：

- provider=`omp`；
- command=`omp`；
- resolved executable path；
- version；
- configured default model。

原生 OMP runtime 走已有 built-in runtime 路径，不引入 custom profile launch override，不修改 daemon 的通用任务启动或模型列表流程。

## 8. Runtime profile 与数据库

`runtime_profile.protocol_family` 的 CHECK constraint 必须包含 `omp`，并与 `agent.SupportedTypes` 和 `agent.New` 保持一致。

新增独立 up/down migration：

- up：删除旧 CHECK，添加包含 `omp` 的新 CHECK；
- down：恢复不包含 `omp` 的 CHECK；
- 不增加外键或级联动作；
- 不改表结构之外的运行时数据。

## 9. Skill 注入

OMP 源于 Pi，但 skill 目录必须以 OMP 实际行为为准。规格采用以下契约：

```text
<workdir>/.pi/skills/
```

在 `skillsDirPath` 中为 `omp` 增加显式分支，避免依赖 default fallback 或隐式 provider alias。实现前用 OMP smoke test 验证工作区 skill 被发现；若当前 OMP 版本的实际目录不同，必须以该版本官方行为为准同步调整设计和测试，不允许静默写入未经验证的目录。

Sidecar manifest、清理和 skill 文件内容沿用现有 Pi 规则；不复制全局用户 skill，只写入 Multica 分配的项目级 skill。

## 10. UI 与文档

### UI

模型 dropdown 已经通过 runtime model API 获取 provider catalog，因此不新增 OMP 专属 UI 状态。只需：

- 增加 OMP provider 展示名和必要能力文案；
- 确保 provider/model selector 在输入、保存和回显时不被截断；
- 保持模型发现失败时的手动输入能力。

### 文档

更新 provider 能力对照，说明：

- 安装与 `omp` 命令要求；
- `MULTICA_OMP_PATH`、`MULTICA_OMP_MODEL`；
- 模型选择使用 `provider/model` selector；
- `--session-dir` / `--resume <session-id>` 恢复边界；
- `.pi/skills/` skill 路径；
- Multica 不启动 OMP 的 TUI、RPC 或 ACP 入口，而使用 daemon headless JSON 模式。

文档改动与 backend 改动保持独立 commit，减少 upstream 合并时的无关冲突。

## 11. 测试策略

### Agent backend

使用 fake executable 验证：

- 正确 argv 顺序和 prompt 位置；
- 完整 model selector、thinking level、session directory 与 resume session ID 传递；
- OMP JSON event 到消息、stream session ID 和最终结果的映射；
- usage 汇总；
- 取消、超时、非零退出；
- session resume，以及 OMP 的 resume-rejection capability 保持 undetectable；
- 不会把中间 narration 写入最终结果。

### Model discovery

使用固定 JSON fixture 验证：

- selector/provider/name/thinking 映射；
- 缺失 selector 的 fallback；
- 重复 selector 去重；
- 无效单条记录跳过；
- malformed JSON、命令失败和非零退出；
- executable path 不同导致 cache 隔离。

### Daemon/config

验证：

- `omp` 默认探测；
- `MULTICA_OMP_PATH` 优先级；
- `MULTICA_OMP_MODEL` 保存；
- shell fallback command name 覆盖；
- runtime registration 的 protocol family 为 `omp`。

### Skill 与 migration

验证：

- OMP skill 写入 `.pi/skills/`；
- sidecar cleanup 不删除用户已有内容；
- migration 后 `omp` 可作为 runtime profile protocol family；
- migration down 恢复旧约束。

所有测试默认使用 fake CLI 或 fixture，不执行开发机真实的 `omp`，避免账号、网络和本地配置导致不稳定。

## 12. Upstream 合并策略

改动拆成以下独立 commit：

```text
feat(agent): add native OMP backend
feat(agent): discover OMP models
feat(daemon): detect OMP runtime
feat(execenv): inject OMP skills
chore(db): allow omp runtime profiles
docs: document OMP runtime
```

合并原则：

1. 不格式化或重排共享 provider 文件；
2. 新增逻辑紧邻对应 provider 分支，保持小 hunk；
3. 不修改 `daemon.go` 通用调度路径；
4. 不引入与 OMP 无关的抽象重构；
5. upstream 更新后先同步，再逐个 cherry-pick/手工应用上述 commit；
6. 若 upstream 已抽出公共 JSON parser，只接入该 parser，不保留本地重复实现。

## 13. 验收标准

完成后必须满足：

1. 全新 daemon 在 PATH 发现 `omp` 后注册原生 `omp` runtime。
2. Agent 可以选择 `omp` provider 并执行一次 headless prompt。
3. OMP 模型列表通过 `omp models --json` 出现在 UI，selector 完整保留。
4. 选定模型、thinking level（若版本支持）和 session resume 行为有测试覆盖。
5. Multica skill 在 OMP 工作区被发现并在任务结束后正确清理。
6. 已有 Pi provider、Pi custom runtime profile 和其他 provider 的测试不受影响。
7. 迁移、类型检查、Go 测试和相关前端测试通过。
8. 代码变更可以按第 12 节拆分，且不需要合并 daemon 通用调度重构。
