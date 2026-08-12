package agentruntime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	runtimeservice "github.com/zgiai/zgi/api/internal/capabilities/chatruntime/service"
)

type BusinessDriver struct {
	service runtimeservice.Service
}

func NewBusinessDriver(svc runtimeservice.Service) *BusinessDriver {
	return &BusinessDriver{service: svc}
}

func (d *BusinessDriver) RuntimeType() RuntimeType { return RuntimeTypeBusiness }

func (d *BusinessDriver) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	return d.ChatStream(ctx, req, nil, nil)
}

func (d *BusinessDriver) ChatStream(_ context.Context, _ ChatRequest, _ func(string) error, _ func(StreamEvent) error) (*ChatResponse, error) {
	return &ChatResponse{
		Status: "ok",
	}, nil
}

func (d *BusinessDriver) Stop(ctx context.Context, req StopRequest) error {
	if d.service == nil {
		return ErrDriverNotConfigured
	}
	scope := runtimeservice.Scope{
		OrganizationID: req.UserID,
		AccountID:      req.UserID,
	}
	_, err := d.service.StopConversation(ctx, scope, req.ConversationID)
	return err
}

func (d *BusinessDriver) LoadSession(_ context.Context, _, _ uuid.UUID) (*SessionState, error) {
	return nil, ErrUnsupportedRuntime
}

func (d *BusinessDriver) SaveSession(_ context.Context, _ *SessionState) error {
	return ErrUnsupportedRuntime
}

func snapshotState(state interface{}) (json.RawMessage, error) {
	if state == nil {
		return json.RawMessage(`{}`), nil
	}
	return json.Marshal(state)
}

func nowPtr() *time.Time {
	t := time.Now()
	return &t
}
