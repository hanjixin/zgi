package agents

import "testing"

func TestAgentBeforeCreateNormalizesEmptyRuntimeConfig(t *testing.T) {
	a := &Agent{}
	if err := a.BeforeCreate(nil); err != nil {
		t.Fatalf("BeforeCreate: %v", err)
	}
	if a.RuntimeConfig != "{}" {
		t.Fatalf("RuntimeConfig = %q, want {}", a.RuntimeConfig)
	}
}

func TestAgentBeforeCreatePreservesNonEmptyRuntimeConfig(t *testing.T) {
	a := &Agent{RuntimeConfig: `{"skill_ids":[]}`}
	if err := a.BeforeCreate(nil); err != nil {
		t.Fatalf("BeforeCreate: %v", err)
	}
	if a.RuntimeConfig != `{"skill_ids":[]}` {
		t.Fatalf("RuntimeConfig = %q, want preserved", a.RuntimeConfig)
	}
}
