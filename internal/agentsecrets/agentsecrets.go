// Package agentsecrets loads a project's opt-in agent secret allowlist
// (spec/10_AGENT_WORKSPACES_AND_POLICIES.md, "Secret policy") and filters a
// captured/hydrated env profile down to the variables an agent subprocess may
// receive. It mirrors internal/ignore's project-root-dotfile convention
// (.devstrapignore) rather than a DB column: this is a local wrapper-security
// policy with no cross-device sync requirement, so it belongs in the
// repository the agent actually checks out, not in synced state.
package agentsecrets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// FileName is the project-root config file that opts an agent run into
// exposing captured env-profile secrets to the child process.
const FileName = ".devstrapagent.yml"

// Policy is a project's agent_secrets allow/deny list.
type Policy struct {
	Allow []string
	Deny  []string
}

// LoadFromDir reads FileName from dir. A missing file returns (nil, nil): the
// caller must treat a nil Policy as "no opt-in" and inject no captured
// secrets, preserving the pre-existing agent run behavior. A present file
// always yields a non-nil Policy, even with an empty or absent agent_secrets
// key -- the file's mere presence is an explicit opt-in, and an empty Allow
// denies everything rather than falling back to "no policy" semantics.
func LoadFromDir(dir string) (*Policy, error) {
	path := filepath.Join(dir, FileName)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", FileName, err)
	}
	v := viper.New()
	v.SetConfigType("yaml")
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read %s: %w", FileName, err)
	}
	return &Policy{
		Allow: cleanNames(v.GetStringSlice("agent_secrets.allow")),
		Deny:  cleanNames(v.GetStringSlice("agent_secrets.deny")),
	}, nil
}

// Filter returns the subset of vars whose names are on Allow and not on Deny.
// Deny wins on conflict: a name listed in both allow and deny is denied. A nil
// Policy (no config file) allows nothing, matching LoadFromDir's contract.
func (p *Policy) Filter(vars map[string]string) map[string]string {
	out := make(map[string]string)
	if p == nil {
		return out
	}
	deny := make(map[string]bool, len(p.Deny))
	for _, name := range p.Deny {
		deny[name] = true
	}
	for _, name := range p.Allow {
		if deny[name] {
			continue
		}
		if value, ok := vars[name]; ok {
			out[name] = value
		}
	}
	return out
}

func cleanNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}
