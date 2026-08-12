package migrations

import mschema "github.com/zgiai/zgi/api/internal/migrations/schema"

const (
	migrationEnableCodexRuntimeID = "20260811000000_enable_codex_runtime"
	enableCodexRuntimeSQL         = `
		ALTER TABLE public.agents
			ADD COLUMN IF NOT EXISTS runtime_type  varchar(32) NOT NULL DEFAULT 'business',
			ADD COLUMN IF NOT EXISTS runtime_config jsonb     NOT NULL DEFAULT '{}'::jsonb;

		CREATE TABLE IF NOT EXISTS public.codex_workspaces (
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

		CREATE INDEX IF NOT EXISTS idx_codex_workspaces_agent
			ON public.codex_workspaces (agent_id, created_at DESC);

		CREATE INDEX IF NOT EXISTS idx_codex_workspaces_tenant_status
			ON public.codex_workspaces (tenant_id, status);

		CREATE TABLE IF NOT EXISTS public.codex_sessions (
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

		CREATE INDEX IF NOT EXISTS idx_codex_sessions_agent
			ON public.codex_sessions (agent_id, created_at DESC);

		CREATE INDEX IF NOT EXISTS idx_codex_sessions_conversation
			ON public.codex_sessions (conversation_id);

		CREATE TABLE IF NOT EXISTS public.codex_command_logs (
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

		CREATE INDEX IF NOT EXISTS idx_codex_command_logs_session
			ON public.codex_command_logs (session_id, created_at DESC);

		CREATE TABLE IF NOT EXISTS public.codex_tool_calls (
			id          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
			session_id  uuid NOT NULL,
			tool_name   varchar(64) NOT NULL,
			arguments   jsonb NOT NULL DEFAULT '{}'::jsonb,
			result      jsonb NOT NULL DEFAULT '{}'::jsonb,
			status      varchar(32) NOT NULL DEFAULT 'pending',
			started_at  timestamptz,
			finished_at timestamptz
		);

		CREATE INDEX IF NOT EXISTS idx_codex_tool_calls_session
			ON public.codex_tool_calls (session_id, started_at DESC);
	`

	enableCodexRuntimeDownSQL = `
		DROP TABLE IF EXISTS public.codex_tool_calls;
		DROP TABLE IF EXISTS public.codex_command_logs;
		DROP TABLE IF EXISTS public.codex_sessions;
		DROP TABLE IF EXISTS public.codex_workspaces;
	`
)

func init() {
	registerSchemaMigration(
		migrationEnableCodexRuntimeID,
		func(schema *mschema.Builder) error { return schema.Raw(enableCodexRuntimeSQL) },
		func(schema *mschema.Builder) error { return schema.Raw(enableCodexRuntimeDownSQL) },
	)
}
