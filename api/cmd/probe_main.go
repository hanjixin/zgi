package main
import (
  "fmt"
  "github.com/zgiai/zgi/api/config"
)
func main() {
  cfg := config.Current()
  fmt.Printf("Codex.Enabled=%v AgentRunner.URL=%s ClaudeCode=%v CodexModel=%s\n", cfg.Codex.Enabled, cfg.AgentRunner.URL, cfg.AgentRunner.ClaudeCodeEnabled, cfg.Codex.ModelName)
}
