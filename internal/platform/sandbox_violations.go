package platform

import "strings"

func parseSeatbeltDenials(logText string) []SandboxViolation {
	if strings.TrimSpace(logText) == "" {
		return nil
	}
	var out []SandboxViolation
	for _, line := range strings.Split(logText, "\n") {
		deny := strings.Index(line, "deny(")
		if deny < 0 {
			continue
		}
		afterDeny := line[deny:]
		closeParen := strings.IndexByte(afterDeny, ')')
		if closeParen < 0 {
			continue
		}
		rest := strings.TrimSpace(afterDeny[closeParen+1:])
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		operation := fields[0]
		path := strings.TrimSpace(strings.TrimPrefix(rest, operation))
		if tagAt := strings.Index(path, " devstrap-sb-"); tagAt >= 0 {
			path = strings.TrimSpace(path[:tagAt])
		}
		out = append(out, SandboxViolation{
			Operation: operation,
			Path:      path,
			Detail:    line,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
