package migrations

import mschema "github.com/zgiai/zgi/api/internal/migrations/schema"

const migrationAddCodexSessionsSessionID = "20260815230000_add_codex_sessions_session_id"

func init() {
	registerSchemaMigration(
		migrationAddCodexSessionsSessionID,
		upAddCodexSessionsSessionID,
		downAddCodexSessionsSessionID,
	)
}

// upAddCodexSessionsSessionID adds the session_id column the workspace session
// repository writes on every snapshot. The original codex_runtime migration
// created codex_sessions without it, so every SaveSessionSnapshot insert failed
// with "column session_id does not exist".
func upAddCodexSessionsSessionID(schema *mschema.Builder) error {
	if err := schema.Raw(`
		ALTER TABLE public.codex_sessions
			ADD COLUMN IF NOT EXISTS session_id uuid
	`); err != nil {
		return err
	}
	return schema.Raw(`
		CREATE INDEX IF NOT EXISTS idx_codex_sessions_session_id
			ON public.codex_sessions (session_id)
	`)
}

func downAddCodexSessionsSessionID(schema *mschema.Builder) error {
	if err := schema.Raw(`
		DROP INDEX IF EXISTS idx_codex_sessions_session_id
	`); err != nil {
		return err
	}
	return schema.Raw(`
		ALTER TABLE public.codex_sessions
			DROP COLUMN IF EXISTS session_id
	`)
}
