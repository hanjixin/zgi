package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Repo interface {
	CreateWorkspace(ctx context.Context, w *Workspace) error
	GetWorkspace(ctx context.Context, id uuid.UUID) (*Workspace, error)
	GetWorkspaceByAgent(ctx context.Context, agentID uuid.UUID) (*Workspace, error)
	UpdateWorkspace(ctx context.Context, w *Workspace) error
	SaveSessionSnapshot(ctx context.Context, snap *SessionSnapshotRecord) error
	LoadSessionSnapshot(ctx context.Context, conversationID uuid.UUID) (*SessionSnapshotRecord, error)
}

type GormRepo struct {
	db *gorm.DB
}

func NewGormRepo(db *gorm.DB) *GormRepo {
	return &GormRepo{db: db}
}

type Workspace struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey;default:uuid_generate_v4()" json:"id"`
	AgentID        uuid.UUID  `gorm:"type:uuid;not null;index:idx_codex_workspaces_agent" json:"agent_id"`
	TenantID       uuid.UUID  `gorm:"type:uuid;not null" json:"tenant_id"`
	AccountID      uuid.UUID  `gorm:"type:uuid;not null" json:"account_id"`
	GitRepo        string     `gorm:"type:text" json:"git_repo"`
	GitBranch      string     `gorm:"type:text" json:"git_branch"`
	WorkspacePath  string     `gorm:"type:text" json:"workspace_path"`
	SandboxID      string     `gorm:"type:text" json:"sandbox_id"`
	Status         string     `gorm:"type:varchar(32);not null;default:'active';index:idx_codex_workspaces_tenant_status" json:"status"`
	Metadata       string     `gorm:"type:jsonb;not null;default:'{}'::jsonb" json:"metadata"`
	CreatedAt      *time.Time `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt      *time.Time `gorm:"not null;default:now()" json:"updated_at"`
}

func (Workspace) TableName() string { return "codex_workspaces" }

type SessionSnapshotRecord struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey;default:uuid_generate_v4()" json:"id"`
	SessionID      uuid.UUID  `gorm:"type:uuid;not null;index:idx_codex_sessions_conversation" json:"session_id"`
	AgentID        uuid.UUID  `gorm:"type:uuid;not null;index:idx_codex_sessions_agent" json:"agent_id"`
	WorkspaceID    uuid.UUID  `gorm:"type:uuid;not null" json:"workspace_id"`
	ConversationID uuid.UUID  `gorm:"type:uuid;not null;uniqueIndex" json:"conversation_id"`
	RuntimeType    string     `gorm:"type:varchar(32);not null" json:"runtime_type"`
	RuntimeState   string     `gorm:"type:jsonb;not null;default:'{}'::jsonb" json:"runtime_state"`
	LastCheckpoint string     `gorm:"type:jsonb;not null;default:'{}'::jsonb" json:"last_checkpoint"`
	Status         string     `gorm:"type:varchar(32);not null;default:'active'" json:"status"`
	LastActiveAt   *time.Time `gorm:"timestamptz" json:"last_active_at"`
	CreatedAt      *time.Time `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt      *time.Time `gorm:"not null;default:now()" json:"updated_at"`
}

func (SessionSnapshotRecord) TableName() string { return "codex_sessions" }

func (r *GormRepo) CreateWorkspace(ctx context.Context, w *Workspace) error {
	if r == nil || r.db == nil {
		return errors.New("workspace repo not configured")
	}
	return r.db.WithContext(ctx).Create(w).Error
}

func (r *GormRepo) GetWorkspace(ctx context.Context, id uuid.UUID) (*Workspace, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("workspace repo not configured")
	}
	var w Workspace
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&w).Error; err != nil {
		return nil, err
	}
	return &w, nil
}

func (r *GormRepo) GetWorkspaceByAgent(ctx context.Context, agentID uuid.UUID) (*Workspace, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("workspace repo not configured")
	}
	var w Workspace
	if err := r.db.WithContext(ctx).
		Where("agent_id = ? AND status = 'active'", agentID).
		Order("created_at DESC").
		First(&w).Error; err != nil {
		return nil, err
	}
	return &w, nil
}

func (r *GormRepo) UpdateWorkspace(ctx context.Context, w *Workspace) error {
	if r == nil || r.db == nil {
		return errors.New("workspace repo not configured")
	}
	return r.db.WithContext(ctx).Save(w).Error
}

func (r *GormRepo) SaveSessionSnapshot(ctx context.Context, snap *SessionSnapshotRecord) error {
	if r == nil || r.db == nil {
		return errors.New("workspace repo not configured")
	}
	var existing SessionSnapshotRecord
	err := r.db.WithContext(ctx).
		Where("conversation_id = ?", snap.ConversationID).
		First(&existing).Error
	if err == nil {
		existing.RuntimeState = snap.RuntimeState
		existing.LastCheckpoint = snap.LastCheckpoint
		existing.Status = snap.Status
		existing.LastActiveAt = snap.LastActiveAt
		now := time.Now()
		existing.UpdatedAt = &now
		return r.db.WithContext(ctx).Save(&existing).Error
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		now := time.Now()
		snap.CreatedAt = &now
		snap.UpdatedAt = &now
		// Do not rely on a database-side uuid default; mint the id here so the
		// snapshot is portable across postgres and sqlite test databases.
		if snap.ID == uuid.Nil {
			snap.ID = uuid.New()
		}
		return r.db.WithContext(ctx).Create(snap).Error
	}
	return err
}

func (r *GormRepo) LoadSessionSnapshot(ctx context.Context, conversationID uuid.UUID) (*SessionSnapshotRecord, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("workspace repo not configured")
	}
	var snap SessionSnapshotRecord
	if err := r.db.WithContext(ctx).
		Where("conversation_id = ?", conversationID).
		First(&snap).Error; err != nil {
		return nil, err
	}
	return &snap, nil
}
