package platform

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

// defaultRestartDelaySeconds is the throttle both adapters fall back to when a
// spec leaves RestartDelaySeconds at 0. It is coupled to run-loop's own
// consecutive-failure ceiling — see the note by runLoopMaxConsecutiveFailures.
const defaultRestartDelaySeconds = 30

// launchdKV is one sorted environment pair; the plist template renders the
// EnvironmentVariables dict from a stable slice, never a map, so the output is
// byte-deterministic (golden-testable).
type launchdKV struct {
	Key   string
	Value string
}

type launchdPlistView struct {
	Label             string
	ProgramArguments  []string
	RestartOnFailure  bool
	ThrottleInterval  int
	StandardOutPath   string
	StandardErrorPath string
	WorkingDirectory  string
	Env               []launchdKV
}

// launchdPlistTemplate renders a launchd LaunchAgent plist. Every interpolated
// value passes through the "xml" FuncMap helper (encoding/xml.EscapeText) so a
// label, path, or env value containing & < > " can never break the XML.
var launchdPlistTemplate = template.Must(template.New("plist").Funcs(template.FuncMap{
	"xml": xmlEscape,
}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{xml .Label}}</string>
	<key>ProgramArguments</key>
	<array>
{{- range .ProgramArguments}}
		<string>{{xml .}}</string>
{{- end}}
	</array>
	<key>RunAtLoad</key>
	<true/>
{{- if .RestartOnFailure}}
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
{{- end}}
	<key>ThrottleInterval</key>
	<integer>{{.ThrottleInterval}}</integer>
{{- if .StandardOutPath}}
	<key>StandardOutPath</key>
	<string>{{xml .StandardOutPath}}</string>
{{- end}}
{{- if .StandardErrorPath}}
	<key>StandardErrorPath</key>
	<string>{{xml .StandardErrorPath}}</string>
{{- end}}
{{- if .WorkingDirectory}}
	<key>WorkingDirectory</key>
	<string>{{xml .WorkingDirectory}}</string>
{{- end}}
{{- if .Env}}
	<key>EnvironmentVariables</key>
	<dict>
{{- range .Env}}
		<key>{{xml .Key}}</key>
		<string>{{xml .Value}}</string>
{{- end}}
	</dict>
{{- end}}
</dict>
</plist>
`))

// xmlEscape returns s with XML metacharacters escaped, for interpolation into
// the plist template.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	// EscapeText only errors if the underlying writer errors; a bytes.Buffer
	// never does, so the error is structurally impossible here.
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

func renderLaunchdPlist(spec ServiceSpec) ([]byte, error) {
	if spec.Label == "" {
		return nil, fmt.Errorf("launchd: empty label")
	}
	if spec.ExecPath == "" {
		return nil, fmt.Errorf("launchd: empty exec path")
	}
	// XML 1.0 forbids most control characters even escaped; launchd would
	// reject the plist at load, silently (exit 78 class). Fail closed here,
	// matching the systemd renderer's injection gate.
	if err := rejectServiceControlChars(spec); err != nil {
		return nil, fmt.Errorf("launchd: %w", err)
	}
	throttle := spec.RestartDelaySeconds
	if throttle <= 0 {
		throttle = defaultRestartDelaySeconds
	}
	view := launchdPlistView{
		Label:             spec.Label,
		ProgramArguments:  append([]string{spec.ExecPath}, spec.Args...),
		RestartOnFailure:  spec.RestartOnFailure,
		ThrottleInterval:  throttle,
		StandardOutPath:   spec.StdoutPath,
		StandardErrorPath: spec.StderrPath,
		WorkingDirectory:  spec.WorkingDir,
	}
	for _, k := range sortedKeys(spec.Env) {
		view.Env = append(view.Env, launchdKV{Key: k, Value: spec.Env[k]})
	}
	var buf bytes.Buffer
	if err := launchdPlistTemplate.Execute(&buf, view); err != nil {
		return nil, fmt.Errorf("render launchd plist: %w", err)
	}
	return buf.Bytes(), nil
}

// serviceLabelPattern constrains service labels to filename- and argv-safe
// characters. A label reaches filepath.Join (the plist/unit path), a launchctl
// domain target, and a systemd unit name — a "/", "..", or whitespace in any
// of those would escape the unit directory or corrupt the argv (review
// finding: `--label ../../evil` wrote and deleted files outside
// ~/Library/LaunchAgents).
var serviceLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateServiceLabel gates every adapter entry point that derives a path or
// argv from the label.
func validateServiceLabel(label string) error {
	if !serviceLabelPattern.MatchString(label) {
		return fmt.Errorf("invalid service label %q: use only letters, digits, '.', '-', '_' (must start with a letter or digit)", label)
	}
	return nil
}

// launchdPlistPath is the on-disk plist path; launchd requires the filename to
// equal <Label>.plist.
func launchdPlistPath(agentsDir, label string) string {
	return filepath.Join(agentsDir, label+".plist")
}

// The launchctl argv builders emit only the modern per-domain verbs
// (bootstrap/bootout/print), never the deprecated load/unload.

func launchdBootstrapArgs(uid int, plistPath string) []string {
	return []string{"launchctl", "bootstrap", "gui/" + strconv.Itoa(uid), plistPath}
}

func launchdBootoutArgs(uid int, label string) []string {
	return []string{"launchctl", "bootout", "gui/" + strconv.Itoa(uid) + "/" + label}
}

func launchdPrintArgs(uid int, label string) []string {
	return []string{"launchctl", "print", "gui/" + strconv.Itoa(uid) + "/" + label}
}

// parseLaunchctlPrint extracts a running flag and a short detail from
// `launchctl print` output. The format is NOT API-stable — Apple changes it
// between releases — so parsing is line-oriented and tolerant: an unrecognized
// blob degrades to (false, "loaded (state unknown)") rather than an error.
//
// Each key is read from its FIRST occurrence only. `launchctl print` emits the
// service's own top-level `state = running` line before the nested per-endpoint
// `state = active` lines in the service's sub-dictionaries; taking the last
// match would misreport a live service as not-running (caught in live dogfood).
func parseLaunchctlPrint(out string) (running bool, detail string) {
	var state, pid, lastExit string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case state == "" && strings.HasPrefix(line, "state = "):
			state = strings.TrimSpace(strings.TrimPrefix(line, "state = "))
		case pid == "" && strings.HasPrefix(line, "pid = "):
			pid = strings.TrimSpace(strings.TrimPrefix(line, "pid = "))
		case lastExit == "" && strings.HasPrefix(line, "last exit code = "):
			lastExit = strings.TrimSpace(strings.TrimPrefix(line, "last exit code = "))
		}
	}
	running = state == "running"
	var parts []string
	if state != "" {
		parts = append(parts, "state "+state)
	}
	if pid != "" {
		parts = append(parts, "pid "+pid)
	}
	if lastExit != "" {
		parts = append(parts, "last exit code "+lastExit)
	}
	if len(parts) == 0 {
		return false, "loaded (state unknown)"
	}
	return running, strings.Join(parts, ", ")
}

// defaultDarwinPath is the PATH launchd seeds the service with when the caller
// supplies none: the binary's own dir first (so a Homebrew or /usr/local
// install can find sibling tools), then the standard Homebrew and system dirs.
func defaultDarwinPath(execDir string) string {
	return execDir + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// atomicWrite writes data to path via a temp file in the same directory and a
// rename, so a reader never sees a partially written plist/unit. Shared by both
// the launchd and systemd adapters (untagged so it builds on every OS).
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}
