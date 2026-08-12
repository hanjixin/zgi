# Agent OS Kernel 架构设计

> 本文档定义 ZGI 从"业务 Agent Runtime"演进为统一"Agent OS Kernel"的目标形态与迁移路径。
> 目标：既保持现有 Business Agent 完整可用，又为 Coding / Research / Custom 等多类 Agent Runtime 提供统一的内核与接入协议。

## 1. 设计原则

1. **Existing agents keep working.** 既有的 Business Agent 行为、API、SSE 事件、数据库结构均保持零破坏。
2. **New agents take the new path.** 新建 Agent 通过 `runtime_type` 选择新内核。
3. **Protocol over SDK.** 开发者通过 HTTP / gRPC / MCP / A2A 协议访问运行时；SDK 仅作为 DX 层。
4. **Driver pattern.** 每类运行时（Business / Coding / Research / Custom）封装为一个 Driver。
5. **Sandbox profiles.** `zgi-sandbox` 以 `lite / session / interactive` 三档 profile 承载不同隔离等级。
6. **Gradual migration.** 双引擎路由 + Feature Flag + 数据回填（CDC）+ 灰度放量。

## 2. 分层架构

```
┌─────────────────────────────────────────────────────────────┐
│ Developer Experience Layer                                  │
│  SDK / UI / Agent 配置 / Prompt 编辑器 / Workflow 编排       │
└─────────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ Access Protocol Layer                                       │
│  HTTP / gRPC / MCP / A2A / Agent Runtime Protocol (ARP)     │
└─────────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ Runtime Layer (Agent OS Kernel)                             │
│  ┌───────────────────┐  ┌───────────────────┐              │
│  │  BusinessDriver   │  │   CodexDriver     │              │
│  │  (既有 skillloop) │  │  (Task Loop)      │              │
│  └───────────────────┘  └───────────────────┘              │
│  ┌─────────────────────────────────────────────────────┐   │
│  │ Router · Session · MemoryBridge · WorkspaceBridge   │   │
│  │ ToolBridge(MCP) · KnowledgeBridge · State · Checkpoint│  │
│  └─────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ Execution Layer                                             │
│  zgi-sandbox (lite/session/interactive) · zgi-runner        │
│  Container / Process / MicroVM · Egress / Seccomp / TTL     │
└─────────────────────────────────────────────────────────────┘
```

## 3. 目标代码组织

```
api/
  internal/
    capabilities/
      agentruntime/              ← Kernel 主包（新增）
        types.go                 RuntimeType、Driver、SessionState、Checkpoint
        router.go                按 agent.runtime_type 分发
        business_driver.go       适配现有 chatruntime/service
        codex_driver.go          Codex Driver 主入口
        module.go                Fx 构造
        codex/
          loop.go                Plan→Execute→Observe→Retry
          state.go               State + Snapshot/Restore
          stream.go              Codex event → SSE 映射
          prompt.go              系统提示模板
        codex/
          tools/
            registry.go          工具注册表
            sandbox_adapter.go   转发到 sandbox
            builtin_bridge.go    复用 ToolEngine
            approval.go          治理审批
        workspace/
          service.go             workspace 生命周期
          git.go                 Git 操作
          repository.go          GORM 持久化
```

## 4. 数据库演进

在保持既有表结构不变的前提下：

```sql
ALTER TABLE agents ADD COLUMN runtime_type  varchar(32) NOT NULL DEFAULT 'business';
ALTER TABLE agents ADD COLUMN runtime_config jsonb     NOT NULL DEFAULT '{}'::jsonb;

CREATE TABLE codex_workspaces (
  id              uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
  agent_id        uuid NOT NULL,
  tenant_id       uuid NOT NULL,
  account_id      uuid NOT NULL,
  git_repo        text,
  git_branch      text,
  workspace_path  text,
  sandbox_id      text,
  status          varchar(32) NOT NULL DEFAULT 'active',
  metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE codex_sessions (
  id               uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
  agent_id         uuid NOT NULL,
  workspace_id     uuid NOT NULL,
  conversation_id  uuid NOT NULL,
  runtime_type     varchar(32) NOT NULL,
  runtime_state    jsonb NOT NULL DEFAULT '{}'::jsonb,
  last_checkpoint  jsonb NOT NULL DEFAULT '{}'::jsonb,
  status           varchar(32) NOT NULL DEFAULT 'active',
  last_active_at   timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE codex_command_logs (
  id          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
  session_id  uuid NOT NULL,
  command     text NOT NULL,
  args        jsonb NOT NULL DEFAULT '[]'::jsonb,
  exit_code   int,
  stdout      text,
  stderr      text,
  duration_ms bigint,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE codex_tool_calls (
  id          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
  session_id  uuid NOT NULL,
  tool_name   varchar(64) NOT NULL,
  arguments   jsonb NOT NULL DEFAULT '{}'::jsonb,
  result      jsonb NOT NULL DEFAULT '{}'::jsonb,
  status      varchar(32) NOT NULL DEFAULT 'pending',
  started_at  timestamptz,
  finished_at timestamptz
);
```

## 5. 双引擎路由与 Feature Flag

- `agents.runtime_type` 默认值为 `business`，历史 Agent 不动。
- 新建 Agent 时前端可选 `business` 或 `codex`（后续扩展 `research` / `custom`）。
- 路由逻辑：
  - `runtime_type = 'business'` → `BusinessDriver` → 现有 `chatruntime/service`
  - `runtime_type = 'codex'`    → `CodexDriver`    → `TaskLoop` + `ToolBridge` + `Workspace` + `Sandbox`
- 全局 Feature Flag `ZGI_CODEX_ENABLED`：关闭时 Codex 路由返回明确错误，不会静默回退到 business。

## 6. Codex Driver 定位（第一阶段）

第一阶段**完整支持 Codex 作为 Coding Agent**，能力覆盖：

1. 会话级 Task Loop（Plan → Execute → Observe → Retry）
2. 工具集：`files_read`、`files_write`、`files_edit`、`shell_run`、`grep`、`glob`、`codebase_search`、`web_fetch`、`image_gen`
3. Workspace + Git 操作，状态持久化到 `codex_workspaces` / `codex_sessions`
4. SSE 事件复用现有命名：`message_start` / `message` / `message_end` / `agent_progress` / `skill_call_start` / `skill_call_end`
5. 治理审批：沿用 `tool_governance` 流程
6. 老 Agent 行为 100% 一致

**第一阶段不做**：A2A 编排、多 Agent 对话、ResearchDriver、CustomDriver、MCP 动态注册。

## 7. 迁移四阶段

### Phase 1：数据盘点 + 架构冻结（本文档）
- 盘点既有业务 Agent 数量、Skill 调用分布、workflow 绑定数
- 冻结数据库演进脚本、API 兼容面、Feature Flag 语义
- 产出：本文档 + 迁移脚本草案

### Phase 2：Kernel 骨架 + BusinessDriver 接入
- 落地 `agentruntime/` Kernel 骨架与 `BusinessDriver`
- 路由层已就绪，但 `runtime_type` 仍默认 `business`
- 产出：零行为变更的可运行骨架 + 单测

### Phase 3：CodexDriver + Workspace + Sandbox（**第一阶段目标**）
- 落地 Codex Driver、TaskLoop、ToolBridge、Workspace
- 启用 `ZGI_CODEX_ENABLED` 后，新建 Codex Agent 可完整运行
- 产出：Codex 端到端可用 + 老 Agent 回归通过

### Phase 4：全量迁移 + 编排
- A2A / 多 Agent 编排
- ResearchDriver、CustomDriver
- 数据 CDC 回填、灰度切量
- 产出：Kernel 成为所有 Agent 的统一入口

## 8. 不变性约束

1. 既有 `chatruntime/service`、`skillloop/runner`、`tools/engine` 代码路径**不做行为修改**
2. `sandbox/docs/architecture.md`、`AGENTS.md`、`api/AGENTS.md`、`web/AGENTS.md` **保留不动**
3. `runner/` plugin runtime **保持不变**
4. 前端 SSE 事件名**保持不变**
5. 新增代码集中在：
   - `api/internal/capabilities/agentruntime/`
   - `api/internal/migrations/20260811000000_enable_codex_runtime.go`
   - `docs/architecture/agent-os-kernel.md`（本文档）
6. 既有数据库表的列**仅新增，不修改/删除**

## 9. 运行时配置（`api/config/config.go`）

| 环境变量 | 说明 | 默认值 |
| --- | --- | --- |
| `ZGI_CODEX_ENABLED` | 是否启用 Codex Driver | `false` |
| `ZGI_CODEX_PROFILE` | sandbox 依赖 profile | `codex-python-node` |
| `ZGI_CODEX_MODEL_PROVIDER` | 默认模型提供方 | 空（沿用 Agent 配置） |
| `ZGI_CODEX_MODEL_NAME` | 默认模型 | 空 |
| `ZGI_CODEX_MAX_STEPS` | 单次对话最大步数 | `80` |
| `ZGI_CODEX_DEFAULT_SANDBOX` | `session` / `interactive` | `session` |
