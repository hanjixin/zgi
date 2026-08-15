package gateway

import (
	"encoding/json"
	"testing"

	adapter "github.com/zgiai/zgi/api/internal/modules/llm/protocol/adapters"
)

func TestResponsesToChatRequest(t *testing.T) {
	req := &adapter.CreateResponseRequest{
		Model:        "deepseek-chat",
		Instructions: "You are helpful",
		Input: []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{map[string]interface{}{"type": "input_text", "text": "hi"}}},
			map[string]interface{}{"type": "function_call_output", "call_id": "c1", "output": "42"},
		},
		MaxOutputTokens: intPtr(100),
		Stream:          true,
	}
	chat := responsesToChatRequest(req)
	if len(chat.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(chat.Messages))
	}
	if chat.Messages[0].Role != "system" || chat.Messages[0].Content != "You are helpful" {
		t.Fatalf("system message = %#v", chat.Messages[0])
	}
	if chat.Messages[1].Role != "user" {
		t.Fatalf("user message role = %q", chat.Messages[1].Role)
	}
	if chat.Messages[2].Role != "tool" || chat.Messages[2].ToolCallID != "c1" {
		t.Fatalf("tool message = %#v", chat.Messages[2])
	}
	if chat.MaxTokens == nil || *chat.MaxTokens != 100 {
		t.Fatalf("max_tokens = %v", chat.MaxTokens)
	}
	if !chat.Stream {
		t.Fatal("stream should be preserved")
	}
}

func TestChatToResponsesResponse(t *testing.T) {
	resp := &adapter.ChatResponse{
		ID: "chat_1", Model: "deepseek-chat",
		Choices: []adapter.Choice{{Message: adapter.Message{Role: "assistant", Content: "hello"}}},
		Usage:   &adapter.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}
	out := chatToResponsesResponse(resp)
	if out.Object != "response" || out.Status != "completed" {
		t.Fatalf("object/status = %q/%q", out.Object, out.Status)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "message" || len(out.Output[0].Content) != 1 {
		t.Fatalf("output = %#v", out.Output)
	}
	if out.Output[0].Content[0].Text != "hello" {
		t.Fatalf("text = %q", out.Output[0].Content[0].Text)
	}
}

func TestChatStreamToResponsesStream(t *testing.T) {
	chatCh := make(chan adapter.StreamResponse)
	go func() {
		defer close(chatCh)
		chatCh <- adapter.StreamResponse{Choices: []adapter.StreamChoice{{Delta: adapter.Message{Role: "assistant", Content: "hi "}}}}
		chatCh <- adapter.StreamResponse{Choices: []adapter.StreamChoice{{Delta: adapter.Message{Role: "assistant", Content: "there"}}}}
		chatCh <- adapter.StreamResponse{Done: true}
	}()

	out := chatStreamToResponsesStream(chatCh, "deepseek-chat")
	var events []string
	for evt := range out {
		if evt.Done {
			break
		}
		events = append(events, evt.Event)
	}
	want := []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done", "response.completed"}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q (all: %v)", i, events[i], want[i], events)
		}
	}
	// The first delta must carry the first text chunk.
	for _, evt := range out {
		_ = evt
	}
	// Re-collect to inspect the delta payload.
	chatCh2 := make(chan adapter.StreamResponse)
	go func() {
		defer close(chatCh2)
		chatCh2 <- adapter.StreamResponse{Choices: []adapter.StreamChoice{{Delta: adapter.Message{Content: "abc"}}}}
		chatCh2 <- adapter.StreamResponse{Done: true}
	}()
	for evt := range chatStreamToResponsesStream(chatCh2, "deepseek-chat") {
		if evt.Event == "response.output_text.delta" {
			var payload struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal(evt.Data, &payload); err != nil || payload.Delta != "abc" {
				t.Fatalf("delta payload = %s (err %v)", evt.Data, err)
			}
			return
		}
	}
	t.Fatal("no delta event found")
}

func intPtr(v int) *int { return &v }
