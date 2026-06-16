// Package agents embeds the built-in Agent.md profiles so the binary ships them,
// while allowing an external directory (e.g. a mounted ConfigMap) to override.
package agents

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yurifrl/agents/internal/agentmd"
)

//go:embed agents/*.md
var profilesFS embed.FS

// LoadProfile loads a profile by name. If dir is non-empty and contains
// "<name>.md", that file wins; otherwise the embedded profile is used.
func LoadProfile(dir, name string) (*agentmd.Definition, error) {
	if dir != "" {
		path := filepath.Join(dir, name+".md")
		if data, err := os.ReadFile(path); err == nil {
			return agentmd.Parse(string(data))
		}
	}
	data, err := profilesFS.ReadFile("agents/" + name + ".md")
	if err != nil {
		return nil, fmt.Errorf("unknown profile %q (no embedded or mounted definition)", name)
	}
	return agentmd.Parse(string(data))
}
