# Agent OS Kernel · 第一阶段 Codex 完整可运行 实施规划

> 本文档基于 `docs/architecture/agent-os-kernel.md` 的总体架构，
> 明确 **Phase 1 = Codex 完整可运行** 的交付清单、验收标准与落地节奏。
>
> 更新时间：2026-08-13
> 状态：**✅ 核心已落地**（2026-08-12 起由「真 Agent CLI」架构取代 Go 手写假循环）

> ## 2026-08-12 架构更新
>
> 原计划中的「Go 进程内 Task Loop（codex/loop.go）」假循环已被否决并删除。
> Phase 1 现在通过 **Node agent-runner 服务**（`agent-runner/`，承载官方 SDK）驱动
> **真 Claude Code 与真 Codex**：
>
> - `runtime_type=codex` → 真 OpenAI Codex（`@openai/codex-sdk`）
> - `runtime_type=claude-code` → 真 Claude Code（`@anthropic-ai/claude-agent-sdk`）
> - Go 控制面 `agentruntime/cli` 驱动通过 SSE 与 runner 通信，把归一化事件映射回既有前端 SSE 事件。
> - 模型 / system prompt / 工具白名单 / MCP / 记忆文件（CLAUDE.md·AGENTS.md）均由控制面注入。
>
> 原「✅ 已完成 / 🔄 进行中 / ⏳ 待开始」清单中的 Task Loop / Sandbox Adapter / LLM 集成等项已随假循环一并移除。

---

## 1. Phase 1 目标

**在不影响任何既有 Business Agent 的前提下，让新建的 Codex Agent 具备完整的 Coding Agent 运行能力：会话级 Task Loop、Workspace+Sandbox、完整工具集、持久化会话、治理审批、SSE 事件兼容。**

### 核心交付标准
1. **Codex Agent 能真正跑起来**：`POST /agents/:id/chat` → SSE 流式 → Task Loop → Sandbox → 结果返回
2. **零侵入**：既有 Business Agent 行为 100% 不变
3. **可治理**：审批、配额、限流、stop、超时全部可用

---

## 2. 交付清单与当前状态

### ✅ 已完成

| 模块 | 文件 | 说明 |
|------|------|------|
| **迁移** | `migrations/20260811000000_enable_codex_runtime.go` | `agents.runtime_type/runtime_config` + 4 张新表（codex_workspaces, codex_sessions, codex_command_logs, codex_tool_calls） |
| **类型** | `agentruntime/types.go` | `RuntimeType`、`Driver` 接口、`SessionState`、`ChatRequest/ChatResponse`、`StreamEvent`、错误常量 |
| **路由** | `agentruntime/router.go` | 按 `agent.runtime_type` 分发 business / codex driver |
| **Business Driver** | `agentruntime/business_driver.go` | 适配既有 `runtimeservice.Service`，Stop/LoadSession/SaveSession |
| **Codex Driver** | `agentruntime/codex_driver.go` | 编排 workspace → loop.Run() → snapshot |
| **模块装配** | `agentruntime/module.go` | Fx-friendly 构造函数集合 |
| **Task Loop** | `codex/loop.go` | Plan→Execute→Observe→Retry 循环，budget/stop/cancel 完整支持 |
| **状态管理** | `codex/state.go` | State + Snapshot/Restore，文件变更/命令记录/工具调用追踪 |
| **事件流** | `codex/stream.go` | 9 种事件类型 → SSE 兼容映射 |
| **Prompt** | `codex/prompt.go` | SystemPrompt + Planning/Execution/Observe 模板 |
| **工具注册表** | `codex/tools/registry.go` | 9 个工具描述符 + 参数校验 + 需审批标记 |
| **Sandbox 适配器** | `codex/tools/sandbox_adapter.go` | HTTP 调用 `zgi-sandbox` 的 `HTTPSandboxClient` |
| **Builtin 桥接** | `codex/tools/builtin_bridge.go` | 复用 `tools.ToolEngine` 的 `BuiltinBridgeExecutor` + `FallbackExecutor` |
| **审批服务** | `codex/tools/approval.go` | `ApprovalService` 接口 + `InMemoryApprovalService` + `ApprovalGate` |
| **Workspace 仓储** | `workspace/repository.go` | GORM CRUD + Workspace/SessionSnapshot 模型 |
| **Workspace 服务** | `workspace/service.go` | EnsureWorkspace/SaveSessionSnapshot/LoadSessionSnapshot |
| **Git 客户端** | `workspace/git.go` | Clone/Checkout/Pull/Commit/Push 接口 |
| **Agent 模型** | `agents/model.go` | `Agent` 新增 `RuntimeType` + `RuntimeConfig` 字段 |
| **Handler 路由** | `agents/agents_handler.go` | `tryRouteToCodex()` 分派 + `SetRuntimeRouter()` 注入 + SSE 流式兼容 |

### 🔄 进行中（需完善）

| 模块 | 文件 | 待完成内容 |
|------|------|-----------|
| **Task Loop LLM** | `codex/loop.go` | `decideNextTool()` 接入真实 LLM 调用（当前为 stub），`observe()` 接入 LLM 总结，`ToolExecutor` 与 `Registry`/`FallbackExecutor` 绑定 |
| **Sandbox HTTP** | `codex/tools/sandbox_adapter.go` | `RunCommand()`/`RunCode()` 真实 POST 到 `zgi-sandbox` 服务，错误处理完善 |
| **配置集成** | `config/types.go` + `config/load.go` | 新增 `CodexConfig` 结构体 + 加载逻辑 + 6 个环境变量 |
| **路由注入** | `routes/v1/agents_routers.go` | 创建 `agentruntime.Router` 并注入 `AgentsHandler` |
| **LLM 集成** | `codex_driver.go` | 传入 `LLMClient` 给 `TaskLoop`，使 `decideNextTool`/`observe` 真实工作 |

### ⏳ 待开始

| 模块 | 文件 | 说明 |
|------|------|------|
| **单元测试** | `agentruntime/*_test.go` | router 选择、loop 循环、registry 校验、workspace 生命周期 |
| **集成测试** | `tests/codex_e2e_test.go` | 完整 ChatAgent 流程 |
| **治理对接** | `codex/tools/approval.go` → `tool_governance` | 将 `ApprovalService` 接到现有审批流 |

---

## 3. 详细技术规格

### 3.1 配置项（CodexConfig）

```go
// api/config/types.go
type CodexConfig struct {
    Enabled           bool   // ZGI_CODEX_ENABLED
    Profile           string // ZGI_CODEX_PROFILE: "lite" | "session" | "interactive"
    ModelProvider     string // ZGI_CODEX_MODEL_PROVIDER
    ModelName         string // ZGI_CODEX_MODEL_NAME
    MaxSteps          int    // ZGI_CODEX_MAX_STEPS (default: 80)
    DefaultSandbox    string // ZGI_CODEX_DEFAULT_SANDBOX
    SystemPrompt      string // ZGI_CODEX_SYSTEM_PROMPT (可选覆盖)
}
```

### 3.2 环境变量

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `ZGI_CODEX_ENABLED` | `false` | 总开关，关闭时 Codex 路由返回 `ErrRuntimeDisabled` |
| `ZGI_CODEX_PROFILE` | `session` | sandbox profile |
| `ZGI_CODEX_MODEL_PROVIDER` | `zgi` | LLM provider |
| `ZGI_CODEX_MODEL_NAME` | `codex-default` | LLM model |
| `ZGI_CODEX_MAX_STEPS` | `80` | Task Loop 最大步数 |
| `ZGI_CODEX_DEFAULT_SANDBOX` | `zgi-sandbox` | 默认 sandbox 标识 |

### 3.3 Tool Loop → LLM 对接

```
TaskLoop.Run(ctx, req)
  → planPhase()       // 生成初始 plan（当前为默认，后续可接 LLM）
  → for step := 1..MaxSteps:
      → decideNextTool()  // ★ 接 LLM：传入 systemPrompt + history + availableTools → 返回 LLMTool
      → executeTool()     // 通过 ToolExecutor.Execute() 调用
      → observe()         // ★ 接 LLM：判断是否完成，生成总结
      → 如果 shouldStop: terminate("completed")
  → terminate("max_steps")
```

### 3.4 ToolExecutor 链路

```
FallbackExecutor.Execute(name, args, state)
  → SandboxBackedExecutor (如果 sandbox 可用)
      → HTTPSandboxClient.RunCommand/RunCode
  → Registry (本地工具处理)
      → handleFilesRead/Write/Edit/ShellRun/Grep/Glob/CodebaseSearch/WebFetch/ImageGen
  → BuiltinBridgeExecutor (桥接既有 ToolEngine)
      → ToolEngine.InvokeForAgent(agentID, toolName, args)
```

### 3.5 路由分派逻辑

```
POST /agents/:id/chat
  → AgentsHandler.ChatAgent()
      → tryRouteToCodex()
          1. runtimeRouter == nil ? → 跳过
          2. findAgentByID(id) → Agent.RuntimeType
          3. RuntimeType != "codex" ? → 跳过
          4. Router.Route(ctx, descriptor) → CodexDriver
          5. driver.ChatStream(ctx, chatReq, onChunk, onEvent)
          6. 成功 → return（SSE 已输出）
          7. 失败 → 记录 warn log，回退到 business 流程
      ← 回退：既有 chatRuntimeService.PrepareConfiguredChat() 路径
```

---

## 4. 验收标准（Exit Criteria）

### 4.1 功能验收
- [ ] 新建 `runtime_type=codex` 的 Agent，`POST /agents/:id/chat` 可完整跑通（Task Loop 至少 3 步）
- [ ] `files_read`、`files_write`、`files_edit` 正常调用
- [ ] `shell_run` 能在 sandbox 中执行命令并返回 stdout/stderr
- [ ] `grep`、`glob`、`codebase_search` 可在 workspace 中检索
- [ ] `web_fetch`、`files_edit` 等需审批工具走 `tool_governance` 流程
- [ ] 超过 `ZGI_CODEX_MAX_STEPS` 自动终止，状态为 `max_steps`
- [ ] 用户 stop 对话后 `StopAgentRuntimeConversation` 返回 `cancelled`
- [ ] 会话刷新后（LoadSession/SaveSession）能恢复到上一次 checkpoint
- [ ] `CodexDriver.SetExecutor()` 能正确绑定 `FallbackExecutor`

### 4.2 兼容验收
- [ ] 既有 `runtime_type=business` 的 Agent 行为零变化
- [ ] 所有既有 SSE 事件命名、顺序、载荷结构保持不变
- [ ] `chatruntime/service`、`skillloop/runner`、`tools/engine` 代码路径零修改
- [ ] `AGENTS.md`、`api/AGENTS.md`、`web/AGENTS.md`、`sandbox/docs/architecture.md` 保持不动

### 4.3 工程验收
- [ ] `go build ./...` 通过
- [ ] `go test ./internal/capabilities/agentruntime/...` 全部通过
- [ ] 新增代码集中在 `api/internal/capabilities/agentruntime/`，不污染既有模块
- [ ] 迁移脚本幂等，可重复执行
- [ ] 关闭 `ZGI_CODEX_ENABLED` 时，Codex 路由返回 `ErrRuntimeDisabled`，不回退到 business

---

## 5. 执行节奏

| 阶段 | 任务 | 产出 | 状态 |
|------|------|------|------|
| **Phase 0** | 架构设计 | `agent-os-kernel.md` | ✅ 完成 |
| **Phase 1a** | 数据库迁移 | `20260811000000_enable_codex_runtime.go` | ✅ 完成 |
| **Phase 1b** | Kernel 骨架 | types/router/business_driver/codex_driver/module | ✅ 完成 |
| **Phase 1c** | Task Loop + State + Stream + Prompt | loop/state/stream/prompt | ✅ 完成 |
| **Phase 1d** | Tool Bridge | registry/sandbox_adapter/builtin_bridge/approval | ✅ 完成 |
| **Phase 1e** | Workspace Service | service/git/repository | ✅ 完成 |
| **Phase 1f** | Agent 模型扩展 + Handler 路由 | model.go + agents_handler.go tryRouteToCodex | ✅ 完成 |
| **Phase 2a** | 配置集成 | config/types.go CodexConfig + load.go | 🔄 进行中 |
| **Phase 2b** | 路由注入 | agents_routers.go 创建 Router 并注入 | 🔄 进行中 |
| **Phase 2c** | LLM 对接 | loop.go decideNextTool/observe 真实 LLM 调用 | ⏳ 待开始 |
| **Phase 2d** | Sandbox 对接 | sandbox_adapter.go 真实 HTTP 调用 | ⏳ 待开始 |
| **Phase 2e** | Executor 绑定 | codex_driver.go 绑定 FallbackExecutor | ⏳ 待开始 |
| **Phase 3** | 单元测试 | router/loop/registry/workspace 测试覆盖 | ⏳ 待开始 |
| **Phase 4** | go build + e2e 回归 | 编译通过 + 老 Agent 回归 100% | ⏳ 待开始 |

---

## 6. 不变性红线（必须严格遵守）

1. **不修改**既有 `chatruntime/service`、`skillloop/runner`、`tools/engine` 行为
2. **不删除** `AGENTS.md`、`api/AGENTS.md`、`web/AGENTS.md`、`sandbox/docs/architecture.md`
3. **不修改**前端 SSE 事件名
4. **不修改** `runner/` plugin runtime
5. **仅新增**数据库列与新表，不修改/删除既有列
6. **Codex 路由失败时必须回退**到 business 引擎，不能阻塞用户请求
7. **所有 Codex 配置通过 feature flag 控制**，关闭时 100% 透明

---

## 7. 整栈本地验证（2026-08-12 实测通过）

以下步骤在本地把 **API + agent-runner + 真 Claude Code + MCP** 完整跑通，
`POST /agents/:id/chat` 端到端返回真实 Claude 回答。用于团队复现与验收。

### 7.1 前置条件

- 本机已装 `claude` CLI（Claude Code）并有 `ANTHROPIC_API_KEY`
- 可用的 Postgres / Redis（本地 docker，如 `valuz-infra-postgres-1` / `redis-1`）
- Node >= 20

### 7.2 装配 env 与启动

```bash
# 1) 生成 api/.env（复制模板后按需覆盖）
cd api && cp .env.example .env

# 2) 关键覆盖（指向本机 DB/Redis，开启 agent runner）
#    DB_PORT / DB_USERNAME / DB_PASSWORD / DB_NAME   → 指向你的 Postgres
#    REDIS_PORT                                      → 指向你的 Redis
#    SECRET_KEY=…                                    → 随机强密钥
#    RESEND_API_KEY=dummy…                            → 占位（否则 PUBLIC_DEPLOYMENT_ENABLED=true 会启动校验失败）
#    ZGI_AGENT_RUNNER_URL=http://localhost:3101
#    ZGI_CLAUDE_CODE_ENABLED=true
#    ZGI_CODEX_ENABLED=false
#    ZGI_AGENT_MCP_URL=http://localhost:2670/console/api/agent-mcp
#    ZGI_ANTHROPIC_API_KEY=<你的 key>
#    ZGI_AGENT_RUNNER_WORKSPACE_ROOT=/tmp/zgi-agents

# 3) 迁移 + 启动 API
cd api && go run ./cmd/migrate up
go run ./cmd/server          # API :2670

# 4) 启动 agent-runner
cd agent-runner && npm install && npm run dev     # runner :3101
```

### 7.3 手插数据（本地无注册/无邮件时的最小集）

认证与权限需要以下行（uuid 自取）：

```sql
-- 账号（super admin 便于权限直通）
INSERT INTO accounts (id, name, email, status, is_super_admin, initialized_at, created_at, updated_at)
VALUES ('<ACCT>','Dev Admin','dev@zgi.ai','active',true,now(),now(),now());

-- 组织 / 工作区 / 成员(owner) —— 权限检查 `members` 表 role=owner 才放行
INSERT INTO organizations (id, name, status, billing_display_currency, usd_to_cny_rate, created_at, updated_at)
VALUES ('<ORG>','Dev Org','active','USD',7,now(),now());
INSERT INTO workspaces (id, name, plan, status, organization_id, created_at, updated_at)
VALUES ('<ORG>','Dev Workspace','basic','normal','<ORG>',now(),now());
INSERT INTO members (organization_id, account_id, role, name, status, created_at, updated_at)
VALUES ('<ORG>','<ACCT>','owner','Dev Admin','active',now(),now());

-- Setup 状态（`SetupRequired` 中间件）
INSERT INTO zgi_setups (version, setup_at) VALUES ('2026-08-12', now());

-- Agent：runtime_type=claude-code，created_by 指向账号
INSERT INTO agents (id, tenant_id, name, description, agent_type, web_app_id, web_app_status, enable_api,
                    runtime_type, runtime_config, created_by, created_at, updated_at)
VALUES ('<AGENT>','<ORG>','Test Codex Agent','dev agent','assistant','<WEBAPP>','active',false,
        'claude-code','{}','<ACCT>',now(),now());
```

> `agent.tenant_id` 同时充当权限检查的 workspaceID，与 JWT 的 `tenant_id` claim 保持一致（见 7.4）。

### 7.4 铸造 JWT

中间件从 JWT 的 `user_id`（账号）与 `tenant_id`（组织）claim 直接解析：

```json
{"alg":"HS256","typ":"JWT"} . {"user_id":"<ACCT>","tenant_id":"<ORG>","exp":<now+7200>,
 "iss":"SELF_HOSTED","sub":"Console API Passport"} . <HS256(SECRET_KEY)>
```

- `SECRET_KEY` = `api/.env` 里的值；`iss` = `platformRunMode`（默认 `SELF_HOSTED`）

### 7.5 端到端调用与期望 SSE 事件

```bash
curl -sN -X POST "localhost:2670/console/api/agents/<AGENT>/chat" \
  -H "Authorization: Bearer $JWT" -H 'content-type: application/json' \
  -d '{"query":"用 Bash 运行 `echo 事件流验证OK` 然后报告"}'
```

实测完整事件流（5 个事件）：

```
event: message_start       # 会话开始（conversation_id / message_id）
event: skill_call_start    # 真 Claude 工具调用 tool_name=Bash, arguments={command:"echo 事件流验证OK"}
event: skill_call_end      # 工具真实 stdout 回填 result.output="事件流验证OK"
event: message             # 助手最终文本回答
event: message_end         # status=completed, stream_event_count=5
```

对应映射：`message_start`(Go 开场) · `skill_call_start`←runner `tool_use` ·
`skill_call_end`←runner `tool_result` · `message`←runner `text` · `message_end`←runner `done`。

> 实测回答示例："我是 Claude…，可以帮你完成代码开发、调试、代码审查、架构设计以及调用各类 MCP 工具（文件生成、图表、Agent 配置、数据库等）" —— 证明 MCP 工具（`/console/api/agent-mcp` 暴露的 52 个 ZGI 内置工具）已注入真 Agent。

### 7.6 两个真 Agent 均实测通过（2026-08-12）

- **Claude Code**（`runtime_type=claude-code`）：✅ SSE 事件流 message_start → skill_call_start(Bash) → skill_call_end → message → message_end(completed)。
- **Codex**（`runtime_type=codex`）：✅ 同上，事件含 `command_logged`（真 Codex 执行 shell 命令）；回答基于 GPT-5。
- 两者都走同一链路：Go 驱动 → agent-runner → 真 CLI → SSE 映射回传；MCP 工具对 Claude 生效。

关键修复（本机曾遇）：
1. **codex 二进制损坏**：`npm i -g @openai/codex` 重装后恢复（ENOENT）。
2. **codex 默认模型**：`loadCodexConfig` 的 `ZGI_CODEX_MODEL_NAME` 默认由 `codex-default` 改为空，否则真 Codex 用该占位名启动失败（"Reading prompt from stdin…"）。不传模型时 Codex 用本地 `~/.codex/config.toml` 默认。
3. **codex MCP**：Codex 的 MCP server 通过 SDK `config` 的 `--config` 点路径逐键注入（`mcp_servers.<name>.url=…`、`http_headers.<KEY>=…`），map 字段必须逐键、不能发内联表（`env` 仅 stdio 支持，streamable_http 会拒绝）；见 `codex.ts::buildMcpServersConfig`。Claude 的 MCP 不受影响。

### 7.7 已知限制

- 邮件注册需要真实 `RESEND_API_KEY`；本地用 7.3 手插数据替代。
- 审批仍为治理自动决策（`permission_request` 事件已透出，交互式审批 UI 待做）。
- Codex 的 MCP 已通过 `--config` 点路径逐键注入接通（见 7.6-3）；`mcpbridge` 的 Streamable HTTP GET/SSE 会话握手已补齐（`server.go` 对 `Accept: text/event-stream` 返回 `text/event-stream` 流），Codex 客户端可完成握手。
- **`allowed_tools` 治理仅对 Claude 生效，Codex 无工具级约束。** Claude 通过 SDK 的 `canUseTool` 回调应用 `allowed_tools`/`disallowed_tools`；Codex 0.147 的权限模型是 **命名权限档案**（`[permissions.<name>]`，仅控制 filesystem 路径与 network 域名），没有"禁用某个工具"的配置。把治理工具名映射到档案会**过度限制**（`web_fetch` 禁用 → 连 git/包下载一起禁）或**覆盖不全**（`shell_run` 在 `danger-full-access` 下无法限制）。真正可靠的工具级约束需把 Codex 跑进 **zgi-sandbox 容器**（OS 层限制文件/网络/进程），或等待 Codex 提供工具级权限；在此之前 `allowed_tools` 对 Codex 是**文档化限制**。
