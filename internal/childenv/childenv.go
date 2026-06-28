package childenv

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

type Options struct {
	Base  []string
	Allow []string
	Set   map[string]string
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func FromOS(allow []string, set map[string]string) ([]string, error) {
	return Build(Options{Base: os.Environ(), Allow: allow, Set: set})
}

func Build(opts Options) ([]string, error) {
	base := map[string]string{}
	for _, pair := range opts.Base {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || !validName(name) || Dangerous(name) {
			continue
		}
		base[name] = value
	}

	out := make([]string, 0, len(opts.Allow)+len(opts.Set))
	emitted := map[string]bool{}
	for _, pattern := range opts.Allow {
		if pattern == "" {
			continue
		}
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			var names []string
			for name := range base {
				if strings.HasPrefix(name, prefix) {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			for _, name := range names {
				if !emitted[name] {
					out = append(out, name+"="+base[name])
					emitted[name] = true
				}
			}
			continue
		}
		if value, ok := base[pattern]; ok && !emitted[pattern] {
			out = append(out, pattern+"="+value)
			emitted[pattern] = true
		}
	}

	var setNames []string
	for name := range opts.Set {
		if !validName(name) {
			return nil, fmt.Errorf("invalid child environment variable %q", name)
		}
		if Dangerous(name) {
			return nil, fmt.Errorf("refusing to set dangerous child environment variable %q", name)
		}
		setNames = append(setNames, name)
	}
	sort.Strings(setNames)
	for _, name := range setNames {
		out = append(out, name+"="+opts.Set[name])
	}
	return out, nil
}

func BasicAllowlist() []string {
	return []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "SSH_AUTH_SOCK", "TMPDIR", "TERM"}
}

// AgentAllowlist returns the environment allowlist for agent subprocesses.
// It excludes SSH_AUTH_SOCK (and SSH_ASKPASS) so a semi-trusted agent command
// cannot use the user's ssh-agent to authenticate as the user (AGEN-02/SECU-02).
// HOME is also excluded from inheritance; the caller must set HOME to the
// worktree path so agent tooling that needs $HOME does not reach the user's
// real dotfiles (~/.ssh, ~/.aws, ~/.config/gh, etc.) (SECU-02).
// Git/editor/gh launches continue to use BasicAllowlist for legitimate SSH auth.
func AgentAllowlist() []string {
	return []string{"PATH", "USER", "LOGNAME", "SHELL", "TMPDIR", "TERM"}
}

func Dangerous(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	if name == "" {
		return true
	}
	if strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
		return true
	}
	switch name {
	case "BASH_ENV",
		"ENV",
		"IFS",
		"NODE_OPTIONS",
		"NODE_PATH",
		"PYTHONHOME",
		"PYTHONPATH",
		"PYTHONSTARTUP",
		"RUBYLIB",
		"RUBYOPT",
		"PERL5LIB",
		"PERL5OPT",
		"CLASSPATH",
		"JAVA_TOOL_OPTIONS",
		"GIT_EXEC_PATH",
		"GIT_SSH",
		"GIT_SSH_COMMAND",
		"PROMPT_COMMAND",
		"ZDOTDIR":
		return true
	default:
		return false
	}
}

func validName(name string) bool {
	return envName.MatchString(name)
}
