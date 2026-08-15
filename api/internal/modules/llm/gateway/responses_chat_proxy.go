package gateway

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	adapter "github.com/zgiai/zgi/api/internal/modules/llm/protocol/adapters"
)

// responses_chat_proxy translates between the OpenAI Responses API (used by
// Codex) and Chat Completions, so chat-only providers (e.g. DeepSeek) can serve
// /v1/responses requests. Streaming maps the chat SSE chunks onto the Responses
// SSE event sequence (response.created -> response.output_text.delta ->
// response.completed).

// responsesToChatRequest converts a Responses request into a Chat request.
func responsesToChatRequest(req *adapter.CreateResponseRequest) *adapter.ChatRequest {
	chat := &adapter.ChatRequest{
		Model:       req.Model,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
	}
	if req.MaxOutputTokens != nil {
		chat.MaxTokens = req.MaxOutputTokens
	} else if req.MaxTokens != nil {
		chat.MaxTokens = req.MaxTokens
	}
	if req.Instructions != "" {
		chat.Messages = append(chat.Messages, adapter.Message{Role: "system", Content: req.Instructions})
	}
	chat.Messages = append(chat.Messages, responsesInputToMessages(req.Input)...)
	chat.Messages = append(chat.Messages, req.Messages...)
	return chat
}

// responsesInputToMessages converts the Responses `input` items into chat
// messages. `input` may be a plain string or an array of items.
func responsesInputToMessages(input interface{}) []adapter.Message {
	switch v := input.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []adapter.Message{{Role: "user", Content: v}}
	case []interface{}:
		out := make([]adapter.Message, 0, len(v))
		for _, raw := range v {
			item, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			out = append(out, responsesItemToMessage(item))
		}
		return out
	}
	return nil
}

func responsesItemToMessage(item map[string]interface{}) adapter.Message {
	typ, _ := item["type"].(string)
	switch typ {
	case "function_call_output":
		callID, _ := item["call_id"].(string)
		content, _ := item["output"].(string)
		return adapter.Message{Role: "tool", ToolCallID: callID, Content: content}
	case "function_call":
		callID, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		return adapter.Message{
			Role: "assistant",
			ToolCalls: []adapter.ToolCall{{
				ID: callID,
				Function: adapter.FunctionCall{Name: name, Arguments: args},
			}},
		}
	case "message":
		role, _ := item["role"].(string)
		if role == "" {
			role = "user"
		}
		content, _ := item["content"].(interface{})
		return adapter.Message{Role: role, Content: responsesContentToChatContent(content)}
	}
	// Fallback: treat unknown items as a user message with their JSON.
	if data, err := json.Marshal(item); err == nil {
		return adapter.Message{Role: "user", Content: string(data)}
	}
	return adapter.Message{}
}

// responsesContentToChatContent converts a Responses content array into chat
// message content (string or content parts).
func responsesContentToChatContent(content interface{}) interface{} {
	arr, ok := content.([]interface{})
	if !ok {
		return content
	}
	parts := make([]adapter.MessageContentPart, 0, len(arr))
	for _, raw := range arr {
		p, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := p["type"].(string)
		if typ == "input_text" {
			typ = "text"
		}
		text, _ := p["text"].(string)
		parts = append(parts, adapter.MessageContentPart{Type: typ, Text: text})
	}
	return parts
}

// chatToResponsesResponse converts a Chat response into a Responses response.
func chatToResponsesResponse(resp *adapter.ChatResponse) *adapter.CreateResponseResponse {
	out := &adapter.CreateResponseResponse{
		ID:        resp.ID,
		Object:    "response",
		Created:   resp.Created,
		CreatedAt: resp.Created,
		Model:     resp.Model,
		Usage:     resp.Usage,
		Status:    "completed",
	}
	for _, ch := range resp.Choices {
		item := adapter.Output{Type: "message", Role: "assistant", Status: "completed"}
		text := messageContentToText(ch.Message.Content)
		if text != "" {
			item.Content = append(item.Content, adapter.OutputContent{Type: "output_text", Text: text})
		}
		for _, tc := range ch.Message.ToolCalls {
			item.Content = append(item.Content, adapter.OutputContent{Type: "output_text", Text: fmt.Sprintf("[tool_call %s: %s(%s)]", tc.ID, tc.Function.Name, tc.Function.Arguments)})
		}
		out.Output = append(out.Output, item)
	}
	return out
}

func messageContentToText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []adapter.MessageContentPart:
		var b []byte
		for _, p := range v {
			if p.Type == "text" || p.Type == "input_text" {
				b = append(b, p.Text...)
			}
		}
		return string(b)
	}
	return ""
}

// chatStreamToResponsesStream converts a Chat stream into the Responses SSE
// event sequence codex expects.
func chatStreamToResponsesStream(chatCh <-chan adapter.StreamResponse, model string) <-chan adapter.RawStreamEvent {
	out := make(chan adapter.RawStreamEvent)
	go func() {
		defer close(out)
		responseID := "resp_" + uuid.NewString()
		itemID := "msg_" + uuid.NewString()
		created := time.Now().Unix()
		var fullText string

		// response.created
		out <- responsesEvent("response.created", map[string]interface{}{
			"type": "response.created", "event_id": "evt_" + uuid.NewString(),
			"response": map[string]interface{}{
				"id": responseID, "object": "response", "created_at": created,
				"status": "in_progress", "model": model, "output": []interface{}{},
			},
		})
		// response.in_progress
		out <- responsesEvent("response.in_progress", map[string]interface{}{
			"type": "response.in_progress", "event_id": "evt_" + uuid.NewString(),
			"response": map[string]interface{}{"id": responseID, "status": "in_progress"},
		})

		for chunk := range chatCh {
			if chunk.Done {
				break
			}
			for _, choice := range chunk.Choices {
				delta := choice.Delta
				if text := messageContentToText(delta.Content); text != "" {
					if fullText == "" {
						// first content: announce the message item
						out <- responsesEvent("response.output_item.added", map[string]interface{}{
							"type": "response.output_item.added", "event_id": "evt_" + uuid.NewString(),
							"output_index": 0,
							"item": map[string]interface{}{
								"id": itemID, "type": "message", "role": "assistant", "content": []interface{}{},
							},
						})
						out <- responsesEvent("response.content_part.added", map[string]interface{}{
							"type": "response.content_part.added", "event_id": "evt_" + uuid.NewString(),
							"item_id": itemID, "output_index": 0, "content_index": 0,
							"part": map[string]interface{}{"type": "output_text", "text": ""},
						})
					}
					fullText += text
					out <- responsesEvent("response.output_text.delta", map[string]interface{}{
						"type": "response.output_text.delta", "event_id": "evt_" + uuid.NewString(),
						"item_id": itemID, "output_index": 0, "content_index": 0, "delta": text,
					})
				}
			}
		}

		if fullText != "" {
			out <- responsesEvent("response.output_text.done", map[string]interface{}{
				"type": "response.output_text.done", "event_id": "evt_" + uuid.NewString(),
				"item_id": itemID, "output_index": 0, "content_index": 0, "text": fullText,
			})
			out <- responsesEvent("response.content_part.done", map[string]interface{}{
				"type": "response.content_part.done", "event_id": "evt_" + uuid.NewString(),
				"item_id": itemID, "output_index": 0, "content_index": 0,
				"part": map[string]interface{}{"type": "output_text", "text": fullText},
			})
			out <- responsesEvent("response.output_item.done", map[string]interface{}{
				"type": "response.output_item.done", "event_id": "evt_" + uuid.NewString(), "output_index": 0,
				"item": map[string]interface{}{
					"id": itemID, "type": "message", "role": "assistant", "status": "completed",
					"content": []interface{}{map[string]interface{}{"type": "output_text", "text": fullText, "annotations": []interface{}{}}},
				},
			})
		}

		// response.completed
		out <- responsesEvent("response.completed", map[string]interface{}{
			"type": "response.completed", "event_id": "evt_" + uuid.NewString(),
			"response": map[string]interface{}{
				"id": responseID, "object": "response", "created_at": created, "status": "completed",
				"model": model, "output": []interface{}{},
			},
		})
	}()
	return out
}

func responsesEvent(event string, payload map[string]interface{}) adapter.RawStreamEvent {
	data, _ := json.Marshal(payload)
	return adapter.RawStreamEvent{Event: event, Data: data}
}
