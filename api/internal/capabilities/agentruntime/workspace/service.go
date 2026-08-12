package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type Params struct {
	AgentID  uuid.UUID
	TenantID uuid.UUID
	AccountID uuid.UUID
}

type SessionSnapshot struct {
	SessionID      uuid.UUID
	AgentID        uuid.UUID
	WorkspaceID    uuid.UUID
	ConversationID uuid.UUID
	RuntimeType    string
	Status         string
	RuntimeState   []byte
	Checkpoint     []byte
}

type Service interface {
	EnsureWorkspace(ctx context.Context, p Params) (*Workspace, error)
	GetWorkspace(ctx context.Context, id uuid.UUID) (*Workspace, error)
	GetWorkspaceByAgent(ctx context.Context, agentID uuid.UUID) (*Workspace, error)
	DeleteWorkspace(ctx context.Context, id uuid.UUID) error
	SaveSessionSnapshot(ctx context.Context, s SessionSnapshot) error
	LoadSessionSnapshot(ctx context.Context, conversationID uuid.UUID) (*SessionSnapshot, error)
}

type service struct {
	repo Repo
}

func NewService(repo Repo) Service {
	return &service{repo: repo}
}

func (s *service) EnsureWorkspace(ctx context.Context, p Params) (*Workspace, error) {
	if s.repo == nil {
		return nil, errors.New("workspace service not configured")
	}
	existing, err := s.repo.GetWorkspaceByAgent(ctx, p.AgentID)
	if err == nil && existing != nil {
		return existing, nil
	}
	w := &Workspace{
		ID:        uuid.New(),
		AgentID:   p.AgentID,
		TenantID:  p.TenantID,
		AccountID: p.AccountID,
		Status:    "active",
		Metadata:  `{}`,
	}
	now := time.Now()
	w.CreatedAt = &now
	w.UpdatedAt = &now
	if err := s.repo.CreateWorkspace(ctx, w); err != nil {
		return nil, err
	}
	return w, nil
}

func (s *service) GetWorkspace(ctx context.Context, id uuid.UUID) (*Workspace, error) {
	if s.repo == nil {
		return nil, errors.New("workspace service not configured")
	}
	return s.repo.GetWorkspace(ctx, id)
}

func (s *service) GetWorkspaceByAgent(ctx context.Context, agentID uuid.UUID) (*Workspace, error) {
	if s.repo == nil {
		return nil, errors.New("workspace service not configured")
	}
	return s.repo.GetWorkspaceByAgent(ctx, agentID)
}

func (s *service) DeleteWorkspace(ctx context.Context, id uuid.UUID) error {
	if s.repo == nil {
		return errors.New("workspace service not configured")
	}
	w, err := s.repo.GetWorkspace(ctx, id)
	if err != nil {
		return err
	}
	w.Status = "deleted"
	now := time.Now()
	w.UpdatedAt = &now
	return s.repo.UpdateWorkspace(ctx, w)
}

func (s *service) SaveSessionSnapshot(ctx context.Context, snap SessionSnapshot) error {
	if s.repo == nil {
		return errors.New("workspace service not configured")
	}
	var stateJSON string
	if len(snap.RuntimeState) == 0 {
		stateJSON = `{}`
	} else {
		stateJSON = string(snap.RuntimeState)
	}
	var checkpointJSON string
	if len(snap.Checkpoint) == 0 {
		checkpointJSON = `{}`
	} else {
		checkpointJSON = string(snap.Checkpoint)
	}
	activeAt := time.Now()
	return s.repo.SaveSessionSnapshot(ctx, &SessionSnapshotRecord{
		SessionID:      snap.SessionID,
		AgentID:        snap.AgentID,
		WorkspaceID:    snap.WorkspaceID,
		ConversationID: snap.ConversationID,
		RuntimeType:    snap.RuntimeType,
		RuntimeState:   stateJSON,
		LastCheckpoint: checkpointJSON,
		Status:         snap.Status,
		LastActiveAt:   &activeAt,
	})
}

func (s *service) LoadSessionSnapshot(ctx context.Context, conversationID uuid.UUID) (*SessionSnapshot, error) {
	if s.repo == nil {
		return nil, errors.New("workspace service not configured")
	}
	rec, err := s.repo.LoadSessionSnapshot(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	var stateBytes, checkpointBytes []byte
	if rec.RuntimeState != "" {
		stateBytes = []byte(rec.RuntimeState)
	}
	if rec.LastCheckpoint != "" {
		checkpointBytes = []byte(rec.LastCheckpoint)
	}
	return &SessionSnapshot{
		SessionID:      rec.SessionID,
		AgentID:        rec.AgentID,
		WorkspaceID:    rec.WorkspaceID,
		ConversationID: rec.ConversationID,
		RuntimeType:    rec.RuntimeType,
		Status:         rec.Status,
		RuntimeState:   stateBytes,
		Checkpoint:     checkpointBytes,
	}, nil
}
