// Package skillstools exposes ZGI skills as builtin tools so real agent CLIs
// (Codex / Claude Code) can list and run skills through the MCP bridge.
//
// It lives outside api/internal/modules/tools because the skills module imports
// the tools module; a tools/* subpackage would create an import cycle.
package skillstools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zgiai/zgi/api/internal/modules/skills"
	"github.com/zgiai/zgi/api/internal/modules/tools"
	"github.com/zgiai/zgi/api/internal/modules/tools/builtin"
)

const ProviderID = "skills"

// Provider serves ZGI skills as callable tools.
type Provider struct {
	*builtin.BuiltinProvider
	runtime *skills.Runtime
}

func NewProvider(runtime *skills.Runtime) *Provider {
	identity := tools.ToolProviderIdentity{
		Name:   ProviderID,
		Author: "System",
		Label:  tools.I18nText{"en_US": "ZGI Skills", "zh_Hans": "ZGI 技能"},
		Description: tools.I18nText{
			"en_US":   "List and run ZGI skills.",
			"zh_Hans": "列出并运行 ZGI 技能。",
		},
		Icon: "sparkles",
		Tags: []string{"system"},
	}
	provider := &Provider{
		BuiltinProvider: builtin.NewBuiltinProvider(identity),
		runtime:         runtime,
	}
	provider.RegisterTool(newListSkillsTool(runtime))
	provider.RegisterTool(newRunSkillTool(runtime))
	return provider
}

func newListSkillsTool(runtime *skills.Runtime) tools.Tool {
	entity := skillsToolEntity("list_skills", "List Skills", "列出技能",
		"List the ZGI skills available to this agent, with their descriptions and when-to-use guidance. Call this before deciding to run a skill.",
		nil)
	return &listSkillsTool{BuiltinTool: builtin.NewBuiltinTool(entity, ""), runtime: runtime}
}

type listSkillsTool struct {
	*builtin.BuiltinTool
	runtime *skills.Runtime
}

func (t *listSkillsTool) Invoke(ctx context.Context, _ string, _ map[string]interface{}, _ *string, _ *string, _ *string) ([]tools.ToolInvokeMessage, error) {
	if t.runtime == nil {
		return nil, fmt.Errorf("skills runtime not configured")
	}
	skillsList, err := t.runtime.ListSkills(ctx)
	if err != nil {
		return nil, err
	}
	skillsList = filterEnabledSkills(skillsList, enabledSkillIDs(t.Runtime()))
	return jsonMessages(map[string]interface{}{"skills": skillsList})
}

func (t *listSkillsTool) ForkToolRuntime(runtime *tools.ToolRuntime) tools.Tool {
	return &listSkillsTool{BuiltinTool: t.BuiltinTool.ForkToolRuntime(runtime), runtime: t.runtime}
}

func newRunSkillTool(runtime *skills.Runtime) tools.Tool {
	entity := skillsToolEntity("run_skill", "Run Skill", "运行技能",
		"Run a ZGI skill by id. Provide skill_id from list_skills and arguments as a JSON object string the skill accepts. Returns the skill's execution result.",
		[]tools.ToolParameter{
			{Name: "skill_id", Label: tools.I18nText{"en_US": "Skill ID", "zh_Hans": "技能 ID"}, LLMDescription: "The skill id returned by list_skills.", Type: tools.ToolParameterTypeString, Form: tools.ToolParameterFormLLM, Required: true},
			{Name: "arguments", Label: tools.I18nText{"en_US": "Arguments", "zh_Hans": "参数"}, LLMDescription: "Optional JSON object string of skill arguments, e.g. {\"path\":\"src\"}.", Type: tools.ToolParameterTypeString, Form: tools.ToolParameterFormLLM, Required: false},
		})
	return &runSkillTool{BuiltinTool: builtin.NewBuiltinTool(entity, ""), runtime: runtime}
}

type runSkillTool struct {
	*builtin.BuiltinTool
	runtime  *skills.Runtime
	tenantID string
}

func (t *runSkillTool) Invoke(ctx context.Context, userID string, params map[string]interface{}, conversationID *string, appID *string, messageID *string) ([]tools.ToolInvokeMessage, error) {
	if t.runtime == nil {
		return nil, fmt.Errorf("skills runtime not configured")
	}
	skillID := stringValue(params, "skill_id")
	if skillID == "" {
		return nil, fmt.Errorf("skill_id is required")
	}
	if enabled := enabledSkillIDs(t.Runtime()); enabled != nil && !contains(enabled, skillID) {
		return nil, fmt.Errorf("skill %s is not enabled for this agent", skillID)
	}
	args := map[string]interface{}{}
	if raw := stringValue(params, "arguments"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return nil, fmt.Errorf("arguments must be a JSON object string: %w", err)
		}
	}
	resolved, err := t.runtime.ResolveEnabledSkills(ctx, []string{skillID})
	if err != nil {
		return nil, err
	}
	execCtx := skills.ExecutionContext{
		OrganizationID: t.tenantID,
		UserID:         userID,
		InvokeFrom:     tools.ToolInvokeFromAgent,
	}
	if conversationID != nil {
		execCtx.ConversationID = *conversationID
	}
	if appID != nil {
		execCtx.AppID = *appID
	}
	if messageID != nil {
		execCtx.MessageID = *messageID
	}
	result, err := t.runtime.CallSkillTool(ctx, resolved, skillID, skills.SkillScriptToolRun, args, execCtx, "")
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("skill %s returned no result", skillID)
	}
	if len(result.Messages) > 0 {
		return result.Messages, nil
	}
	return jsonMessages(map[string]interface{}{
		"skill_id": skillID,
		"status":   result.Trace.Status,
	})
}

func (t *runSkillTool) ForkToolRuntime(runtime *tools.ToolRuntime) tools.Tool {
	return &runSkillTool{
		BuiltinTool: t.BuiltinTool.ForkToolRuntime(runtime),
		runtime:     t.runtime,
		tenantID:    runtime.TenantID,
	}
}

func skillsToolEntity(name, labelEN, labelZH, description string, params []tools.ToolParameter) tools.ToolEntity {
	return tools.ToolEntity{
		Identity: tools.ToolIdentity{
			Name:     name,
			Author:   "System",
			Provider: ProviderID,
			Label:    tools.I18nText{"en_US": labelEN, "zh_Hans": labelZH},
			Icon:     "sparkles",
		},
		Description: tools.ToolDescription{
			Human: tools.I18nText{"en_US": description, "zh_Hans": description},
			LLM:   description,
		},
		Parameters: params,
		OutputType: "json",
		Tags:       []string{"skills", "system"},
	}
}

// enabledSkillIDs reads the agent's bound skill ids from the tool runtime.
// Returns nil when no constraint is set (all system skills available), or a
// non-empty list to filter against.
func enabledSkillIDs(runtime *tools.ToolRuntime) []string {
	if runtime == nil || runtime.RuntimeParameters == nil {
		return nil
	}
	raw, ok := runtime.RuntimeParameters["enabled_skill_ids"]
	if !ok {
		return nil
	}
	var out []string
	switch v := raw.(type) {
	case []string:
		out = append(out, v...)
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		return []string{}
	}
	return out
}

func filterEnabledSkills(list []skills.SkillDiscoveryMetadata, enabled []string) []skills.SkillDiscoveryMetadata {
	if enabled == nil {
		return list
	}
	allowed := map[string]struct{}{}
	for _, id := range enabled {
		allowed[id] = struct{}{}
	}
	out := make([]skills.SkillDiscoveryMetadata, 0, len(list))
	for _, s := range list {
		if _, ok := allowed[s.ID]; ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func stringValue(params map[string]interface{}, key string) string {
	if params == nil {
		return ""
	}
	v, _ := params[key].(string)
	return v
}

func jsonMessages(value interface{}) ([]tools.ToolInvokeMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return []tools.ToolInvokeMessage{builtin.CreateTextMessage(string(data))}, nil
}

var _ tools.ToolProvider = (*Provider)(nil)
var _ tools.Tool = (*listSkillsTool)(nil)
var _ tools.Tool = (*runSkillTool)(nil)
