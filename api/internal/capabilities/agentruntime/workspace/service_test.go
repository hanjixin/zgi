package workspace

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	// A per-test temp file avoids shared-memory sqlite connection edge cases.
	dsn := filepath.Join(t.TempDir(), "codex-test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// uuid_generate_v4() is a postgres-only default, so the workspace tables are
	// created with sqlite-compatible DDL here rather than through AutoMigrate.
	if err := db.Exec(`
		CREATE TABLE codex_workspaces (
			id             text PRIMARY KEY,
			agent_id       text NOT NULL,
			tenant_id      text NOT NULL,
			account_id     text NOT NULL,
			git_repo       text,
			git_branch     text,
			workspace_path text,
			sandbox_id     text,
			status         varchar(32) NOT NULL DEFAULT 'active',
			metadata       text NOT NULL DEFAULT '{}',
			created_at     datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at     datetime NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE codex_sessions (
			id              text PRIMARY KEY,
			session_id      text NOT NULL,
			agent_id        text NOT NULL,
			workspace_id    text NOT NULL,
			conversation_id text NOT NULL UNIQUE,
			runtime_type    varchar(32) NOT NULL,
			runtime_state   text NOT NULL DEFAULT '{}',
			last_checkpoint text NOT NULL DEFAULT '{}',
			status          varchar(32) NOT NULL DEFAULT 'active',
			last_active_at  datetime,
			created_at      datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at      datetime NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`).Error; err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func TestEnsureWorkspaceCreatesThenReuses(t *testing.T) {
	svc := NewService(NewGormRepo(newTestDB(t)))
	ctx := context.Background()

	agentID := uuid.New()
	p := Params{AgentID: agentID, TenantID: uuid.New(), AccountID: uuid.New()}

	first, err := svc.EnsureWorkspace(ctx, p)
	if err != nil {
		t.Fatalf("EnsureWorkspace (create): %v", err)
	}
	if first.ID == uuid.Nil || first.Status != "active" {
		t.Fatalf("workspace not initialized: %#v", first)
	}

	second, err := svc.EnsureWorkspace(ctx, p)
	if err != nil {
		t.Fatalf("EnsureWorkspace (reuse): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("EnsureWorkspace created a second workspace: %s != %s", second.ID, first.ID)
	}
}

func TestSaveAndLoadSessionSnapshotRoundTrip(t *testing.T) {
	svc := NewService(NewGormRepo(newTestDB(t)))
	ctx := context.Background()

	agentID := uuid.New()
	ws, err := svc.EnsureWorkspace(ctx, Params{AgentID: agentID, TenantID: uuid.New(), AccountID: uuid.New()})
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}

	conversationID := uuid.New()
	err = svc.SaveSessionSnapshot(ctx, SessionSnapshot{
		SessionID:      uuid.New(),
		AgentID:        agentID,
		WorkspaceID:    ws.ID,
		ConversationID: conversationID,
		RuntimeType:    "codex",
		Status:         "completed",
		RuntimeState:   []byte(`{"step":3}`),
		Checkpoint:     []byte(`{"step":3,"plan":"p"}`),
	})
	if err != nil {
		t.Fatalf("SaveSessionSnapshot: %v", err)
	}

	loaded, err := svc.LoadSessionSnapshot(ctx, conversationID)
	if err != nil {
		t.Fatalf("LoadSessionSnapshot: %v", err)
	}
	if loaded.RuntimeType != "codex" || loaded.Status != "completed" {
		t.Fatalf("snapshot metadata mismatch: %#v", loaded)
	}
	if string(loaded.RuntimeState) != `{"step":3}` {
		t.Fatalf("runtime state mismatch: %q", loaded.RuntimeState)
	}
	if string(loaded.Checkpoint) != `{"step":3,"plan":"p"}` {
		t.Fatalf("checkpoint mismatch: %q", loaded.Checkpoint)
	}
}

func TestSaveSessionSnapshotUpsertsByConversation(t *testing.T) {
	svc := NewService(NewGormRepo(newTestDB(t)))
	ctx := context.Background()

	agentID := uuid.New()
	ws, err := svc.EnsureWorkspace(ctx, Params{AgentID: agentID, TenantID: uuid.New(), AccountID: uuid.New()})
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}

	conversationID := uuid.New()
	base := SessionSnapshot{
		SessionID:      uuid.New(),
		AgentID:        agentID,
		WorkspaceID:    ws.ID,
		ConversationID: conversationID,
		RuntimeType:    "codex",
	}
	if err := svc.SaveSessionSnapshot(ctx, base); err != nil {
		t.Fatalf("first save: %v", err)
	}
	updated := base
	updated.RuntimeState = []byte(`{"step":5}`)
	if err := svc.SaveSessionSnapshot(ctx, updated); err != nil {
		t.Fatalf("second save: %v", err)
	}

	loaded, err := svc.LoadSessionSnapshot(ctx, conversationID)
	if err != nil {
		t.Fatalf("LoadSessionSnapshot: %v", err)
	}
	if string(loaded.RuntimeState) != `{"step":5}` {
		t.Fatalf("runtime state not upserted: %q", loaded.RuntimeState)
	}
}

func TestDeleteWorkspaceSoftDeletes(t *testing.T) {
	svc := NewService(NewGormRepo(newTestDB(t)))
	ctx := context.Background()

	agentID := uuid.New()
	ws, err := svc.EnsureWorkspace(ctx, Params{AgentID: agentID, TenantID: uuid.New(), AccountID: uuid.New()})
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if err := svc.DeleteWorkspace(ctx, ws.ID); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	got, err := svc.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace after delete: %v", err)
	}
	if got.Status != "deleted" {
		t.Fatalf("status = %q, want deleted", got.Status)
	}
}

func TestWorkspaceServiceNilRepoErrors(t *testing.T) {
	var svc Service = NewService(nil)
	ctx := context.Background()
	if _, err := svc.EnsureWorkspace(ctx, Params{AgentID: uuid.New()}); err == nil {
		t.Fatal("EnsureWorkspace with nil repo should error")
	}
}
