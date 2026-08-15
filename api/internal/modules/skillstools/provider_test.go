package skillstools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zgiai/zgi/api/internal/modules/skills"
	"github.com/zgiai/zgi/api/internal/modules/tools"
)

func TestProviderListsSkills(t *testing.T) {
	manager := tools.NewToolManager(nil)
	runtime := skills.NewRuntime(tools.NewToolEngine(manager), manager)
	provider := NewProvider(runtime)

	tool, err := provider.GetTool("list_skills")
	if err != nil {
		t.Fatalf("GetTool(list_skills): %v", err)
	}
	msgs, err := tool.Invoke(context.Background(), "u1", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(msgs) == 0 || msgs[0].Text == "" {
		t.Fatalf("no output messages")
	}
	var out struct {
		Skills []json.RawMessage `json:"skills"`
	}
	if err := json.Unmarshal([]byte(msgs[0].Text), &out); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, msgs[0].Text)
	}
	if len(out.Skills) == 0 {
		t.Fatalf("expected embedded skills to be listed, got %s", msgs[0].Text)
	}
}

func TestEnabledSkillBindingFiltersList(t *testing.T) {
	manager := tools.NewToolManager(nil)
	runtime := skills.NewRuntime(tools.NewToolEngine(manager), manager)
	provider := NewProvider(runtime)

	// Pick a real skill id from the full catalog.
	all, err := runtime.ListSkills(context.Background())
	if err != nil || len(all) == 0 {
		t.Fatalf("no skills available to test binding: %v", err)
	}
	enabledID := all[0].ID

	tool, err := provider.GetTool("list_skills")
	if err != nil {
		t.Fatalf("GetTool(list_skills): %v", err)
	}
	fork := tool.ForkToolRuntime(&tools.ToolRuntime{
		RuntimeParameters: map[string]interface{}{"enabled_skill_ids": []string{enabledID}},
		InvokeFrom:        tools.ToolInvokeFromAgent,
	})
	msgs, err := fork.Invoke(context.Background(), "u1", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Skills []struct {
			ID string `json:"skill_id"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(msgs[0].Text), &out); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, msgs[0].Text)
	}
	if len(out.Skills) != 1 || out.Skills[0].ID != enabledID {
		t.Fatalf("expected only the bound skill %q, got %d skills: %s", enabledID, len(out.Skills), msgs[0].Text)
	}
}

func TestRunSkillRejectsNonEnabledSkill(t *testing.T) {
	manager := tools.NewToolManager(nil)
	runtime := skills.NewRuntime(tools.NewToolEngine(manager), manager)
	provider := NewProvider(runtime)

	tool, err := provider.GetTool("run_skill")
	if err != nil {
		t.Fatalf("GetTool(run_skill): %v", err)
	}
	fork := tool.ForkToolRuntime(&tools.ToolRuntime{
		RuntimeParameters: map[string]interface{}{"enabled_skill_ids": []string{"some-other-skill"}},
		InvokeFrom:        tools.ToolInvokeFromAgent,
	})
	_, err = fork.Invoke(context.Background(), "u1", map[string]interface{}{"skill_id": "not-bound"}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for non-enabled skill")
	}
}

func TestRunSkillRequiresSkillID(t *testing.T) {
	manager := tools.NewToolManager(nil)
	runtime := skills.NewRuntime(tools.NewToolEngine(manager), manager)
	provider := NewProvider(runtime)

	tool, err := provider.GetTool("run_skill")
	if err != nil {
		t.Fatalf("GetTool(run_skill): %v", err)
	}
	_, err = tool.Invoke(context.Background(), "u1", map[string]interface{}{}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for missing skill_id")
	}
}
