package agentruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

type stubDriver struct {
	runtimeType RuntimeType
}

func (d *stubDriver) RuntimeType() RuntimeType { return d.runtimeType }
func (d *stubDriver) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	return &ChatResponse{Status: "completed"}, nil
}
func (d *stubDriver) ChatStream(context.Context, ChatRequest, func(string) error, func(StreamEvent) error) (*ChatResponse, error) {
	return &ChatResponse{Status: "completed"}, nil
}
func (d *stubDriver) Stop(context.Context, StopRequest) error { return nil }
func (d *stubDriver) LoadSession(context.Context, uuid.UUID, uuid.UUID) (*SessionState, error) {
	return nil, ErrSessionNotFound
}
func (d *stubDriver) SaveSession(context.Context, *SessionState) error { return nil }

func TestNormalizeRuntimeType(t *testing.T) {
	cases := []struct {
		name string
		in   RuntimeType
		want RuntimeType
	}{
		{name: "empty defaults to business", in: "", want: RuntimeTypeBusiness},
		{name: "business kept", in: RuntimeTypeBusiness, want: RuntimeTypeBusiness},
		{name: "codex kept", in: RuntimeTypeCodex, want: RuntimeTypeCodex},
		{name: "unknown falls back to business", in: RuntimeType("custom"), want: RuntimeTypeBusiness},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeRuntimeType(tc.in); got != tc.want {
				t.Fatalf("NormalizeRuntimeType(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsValidRuntimeType(t *testing.T) {
	if !IsValidRuntimeType(RuntimeTypeBusiness) {
		t.Fatal("business should be valid")
	}
	if !IsValidRuntimeType(RuntimeTypeCodex) {
		t.Fatal("codex should be valid")
	}
	if IsValidRuntimeType(RuntimeType("bogus")) {
		t.Fatal("bogus should be invalid")
	}
}

func TestRouterRoutesCodexDescriptorToCodexDriver(t *testing.T) {
	business := &stubDriver{runtimeType: RuntimeTypeBusiness}
	codexDriver := &stubDriver{runtimeType: RuntimeTypeCodex}
	r := NewRouter(WithBusinessDriver(business), WithCodexDriver(codexDriver))

	got := r.Route(context.Background(), AgentDescriptor{RuntimeType: RuntimeTypeCodex})
	if got == nil {
		t.Fatal("route for codex returned nil driver")
	}
	if got.RuntimeType() != RuntimeTypeCodex {
		t.Fatalf("route for codex = %s, want codex", got.RuntimeType())
	}
}

func TestRouterRoutesBusinessDescriptorToBusinessDriver(t *testing.T) {
	business := &stubDriver{runtimeType: RuntimeTypeBusiness}
	codexDriver := &stubDriver{runtimeType: RuntimeTypeCodex}
	r := NewRouter(WithBusinessDriver(business), WithCodexDriver(codexDriver))

	got := r.Route(context.Background(), AgentDescriptor{RuntimeType: RuntimeTypeBusiness})
	if got == nil {
		t.Fatal("route for business returned nil driver")
	}
	if got.RuntimeType() != RuntimeTypeBusiness {
		t.Fatalf("route for business = %s, want business", got.RuntimeType())
	}
}

func TestRouterRoutesUnknownDescriptorToBusinessDriver(t *testing.T) {
	business := &stubDriver{runtimeType: RuntimeTypeBusiness}
	codexDriver := &stubDriver{runtimeType: RuntimeTypeCodex}
	r := NewRouter(WithBusinessDriver(business), WithCodexDriver(codexDriver))

	got := r.Route(context.Background(), AgentDescriptor{RuntimeType: RuntimeType("custom")})
	if got == nil || got.RuntimeType() != RuntimeTypeBusiness {
		t.Fatalf("route for unknown runtime = %#v, want business driver", got)
	}
}

func TestRouterFallsBackToBusinessWhenCodexDriverMissing(t *testing.T) {
	business := &stubDriver{runtimeType: RuntimeTypeBusiness}
	r := NewRouter(WithBusinessDriver(business))

	got := r.Route(context.Background(), AgentDescriptor{RuntimeType: RuntimeTypeCodex})
	if got == nil || got.RuntimeType() != RuntimeTypeBusiness {
		t.Fatalf("route for codex without codex driver = %#v, want business fallback", got)
	}
}

func TestRouterReturnsNilWithoutMatchingDriver(t *testing.T) {
	r := NewRouter()
	if got := r.Route(context.Background(), AgentDescriptor{RuntimeType: RuntimeTypeBusiness}); got != nil {
		t.Fatalf("route without drivers = %#v, want nil", got)
	}
}

func TestChatRequestConversationIDOrDefault(t *testing.T) {
	req := ChatRequest{}
	if id := req.ConversationIDOrDefault(); id == uuid.Nil {
		t.Fatal("default conversation id should not be nil")
	}
	existing := uuid.New()
	req = ChatRequest{ConversationID: &existing}
	if got := req.ConversationIDOrDefault(); got != existing {
		t.Fatalf("ConversationIDOrDefault = %s, want %s", got, existing)
	}
}

func TestChatRequestMarshalKeepsStreamEventShape(t *testing.T) {
	evt := StreamEvent{
		ID:        uuid.New(),
		EventType: "message",
		Payload:   json.RawMessage(`{"answer":"hi"}`),
	}
	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal stream event: %v", err)
	}
	var back StreamEvent
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal stream event: %v", err)
	}
	if back.EventType != "message" {
		t.Fatalf("event_type roundtrip = %q, want message", back.EventType)
	}
}
