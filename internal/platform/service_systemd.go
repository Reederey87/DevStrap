package platform

import (
	"fmt"
	"path/filepath"
	"strings"
)

// renderSystemdUnit renders a systemd user unit wrapping the run-loop. The
// [Unit] StartLimit* pair bounds restart storms; [Service] Type=simple matches
// the foreground ticker; [Install] WantedBy=default.target enables it for the
// user session.
func renderSystemdUnit(spec ServiceSpec) ([]byte, error) {
	if spec.Label == "" {
		return nil, fmt.Errorf("systemd: empty label")
	}
	if spec.ExecPath == "" {
		return nil, fmt.Errorf("systemd: empty exec path")
	}
	// Unit files are line-oriented: a control character in ANY interpolated
	// value (an exec path, arg, env value, description, working dir) would
	// terminate its directive and inject arbitrary unit lines — systemdQuote
	// cannot quote a newline (Codex review, HIGH). Fail closed instead.
	if err := rejectServiceControlChars(spec); err != nil {
		return nil, fmt.Errorf("systemd: %w", err)
	}
	restartSec := spec.RestartDelaySeconds
	if restartSec <= 0 {
		restartSec = defaultRestartDelaySeconds
	}
	words := append([]string{spec.ExecPath}, spec.Args...)
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = systemdQuote(w)
	}

	var b strings.Builder
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", spec.Description)
	b.WriteString("After=network-online.target\n")
	b.WriteString("StartLimitIntervalSec=300\n")
	b.WriteString("StartLimitBurst=5\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(quoted, " "))
	if spec.RestartOnFailure {
		b.WriteString("Restart=on-failure\n")
	}
	fmt.Fprintf(&b, "RestartSec=%d\n", restartSec)
	for _, k := range sortedKeys(spec.Env) {
		fmt.Fprintf(&b, "Environment=\"%s=%s\"\n", k, spec.Env[k])
	}
	if spec.WorkingDir != "" {
		fmt.Fprintf(&b, "WorkingDirectory=%s\n", spec.WorkingDir)
	}
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return []byte(b.String()), nil
}

// systemdQuote renders one ExecStart word. `%` introduces a systemd specifier
// and is expanded regardless of quoting, so it is doubled in every word. Words
// containing whitespace or quotes are wrapped in double quotes with backslash
// and double-quote escaped.
func systemdQuote(word string) string {
	escaped := strings.ReplaceAll(word, "%", "%%")
	if word != "" && !strings.ContainsAny(word, " \t\n\"'") {
		return escaped
	}
	escaped = strings.ReplaceAll(escaped, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// rejectServiceControlChars refuses any spec whose interpolated strings carry
// ASCII control characters (including \n, \r, NUL) or DEL. Shared by both
// renderers: the systemd unit is line-oriented (injection), and launchd would
// silently reject a plist carrying raw control bytes XML 1.0 forbids.
func rejectServiceControlChars(spec ServiceSpec) error {
	check := func(field, v string) error {
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("%s contains a control character (%q)", field, v)
			}
		}
		return nil
	}
	if err := check("label", spec.Label); err != nil {
		return err
	}
	if err := check("description", spec.Description); err != nil {
		return err
	}
	if err := check("exec path", spec.ExecPath); err != nil {
		return err
	}
	for _, a := range spec.Args {
		if err := check("arg", a); err != nil {
			return err
		}
	}
	if err := check("working dir", spec.WorkingDir); err != nil {
		return err
	}
	if err := check("stdout path", spec.StdoutPath); err != nil {
		return err
	}
	if err := check("stderr path", spec.StderrPath); err != nil {
		return err
	}
	for k, v := range spec.Env {
		if err := check("env key", k); err != nil {
			return err
		}
		if err := check("env value", v); err != nil {
			return err
		}
	}
	return nil
}

// systemdUnitPath is the on-disk unit path; the filename is <label>.service.
func systemdUnitPath(unitDir, label string) string {
	return filepath.Join(unitDir, label+".service")
}

// The systemctl argv builders always target the --user manager.

func systemdProbeArgs() []string {
	return []string{"systemctl", "--user", "show-environment"}
}

func systemdReloadArgs() []string {
	return []string{"systemctl", "--user", "daemon-reload"}
}

func systemdEnableArgs(unit string) []string {
	return []string{"systemctl", "--user", "enable", unit}
}

func systemdRestartArgs(unit string) []string {
	return []string{"systemctl", "--user", "restart", unit}
}

func systemdDisableNowArgs(unit string) []string {
	return []string{"systemctl", "--user", "disable", "--now", unit}
}

func systemdIsActiveArgs(unit string) []string {
	return []string{"systemctl", "--user", "is-active", unit}
}

func lingerProbeArgs(user string) []string {
	return []string{"loginctl", "show-user", user, "--property=Linger", "--value"}
}

// defaultLinuxPath is the PATH the unit runs with when the caller supplies
// none: the binary's own dir first, then the user-local and system dirs.
func defaultLinuxPath(execDir, home string) string {
	return execDir + ":" + home + "/.local/bin:/usr/local/bin:/usr/bin:/bin"
}
