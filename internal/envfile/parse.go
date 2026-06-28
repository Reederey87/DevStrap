package envfile

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/Reederey87/DevStrap/internal/childenv"
)

const MaxBytes = 1 << 20

type Options struct {
	Literal bool
}

type Binding struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Line  int    `json:"line"`
}

type SecretMap map[string]string

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ParseBytes(raw []byte, opts Options) ([]Binding, error) {
	if len(raw) > MaxBytes {
		return nil, fmt.Errorf("env file is %d bytes, max %d", len(raw), MaxBytes)
	}
	reader := bufio.NewReader(bytes.NewReader(raw))
	var out []Binding
	seen := map[string]int{}
	lineNo := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if err == io.EOF && line == "" {
			break
		}
		lineNo++
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		binding, ok, parseErr := parseLine(line, lineNo, opts)
		if parseErr != nil {
			return nil, parseErr
		}
		if ok {
			if firstLine, exists := seen[binding.Name]; exists {
				return nil, fmt.Errorf("line %d: duplicate variable %s first defined on line %d", lineNo, binding.Name, firstLine)
			}
			seen[binding.Name] = lineNo
			out = append(out, binding)
		}
		if err == io.EOF {
			break
		}
	}
	return out, nil
}

func ToSecretMap(bindings []Binding) SecretMap {
	out := make(SecretMap, len(bindings))
	for _, binding := range bindings {
		out[binding.Name] = binding.Value
	}
	return out
}

func parseLine(line string, lineNo int, opts Options) (Binding, bool, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return Binding{}, false, nil
	}
	if rest, ok := strings.CutPrefix(trimmed, "export "); ok {
		trimmed = strings.TrimSpace(rest)
	}
	namePart, valuePart, ok := strings.Cut(trimmed, "=")
	if !ok {
		return Binding{}, false, fmt.Errorf("line %d: expected KEY=VALUE", lineNo)
	}
	name := strings.TrimSpace(namePart)
	if !namePattern.MatchString(name) {
		return Binding{}, false, fmt.Errorf("line %d: invalid variable name %q", lineNo, name)
	}
	if childenv.Dangerous(name) {
		return Binding{}, false, fmt.Errorf("line %d: refusing dangerous variable name %q", lineNo, name)
	}
	value, err := parseValue(strings.TrimSpace(valuePart), lineNo)
	if err != nil {
		return Binding{}, false, err
	}
	if !opts.Literal && looksInterpolated(value) {
		return Binding{}, false, fmt.Errorf("line %d: %s looks like shell interpolation; pass --literal to capture it as text", lineNo, name)
	}
	return Binding{Name: name, Value: value, Line: lineNo}, true, nil
}

func parseValue(raw string, lineNo int) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '\'':
		end := strings.IndexByte(raw[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("line %d: unterminated single-quoted value", lineNo)
		}
		value := raw[1 : end+1]
		if err := trailingOnlyComment(raw[end+2:], lineNo); err != nil {
			return "", err
		}
		return value, nil
	case '"':
		return parseDoubleQuoted(raw, lineNo)
	default:
		return parseUnquoted(raw), nil
	}
}

func parseDoubleQuoted(raw string, lineNo int) (string, error) {
	var b strings.Builder
	escaped := false
	for i := 1; i < len(raw); i++ {
		ch := raw[i]
		if escaped {
			switch ch {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"', '\\':
				b.WriteByte(ch)
			default:
				return "", fmt.Errorf("line %d: unsupported escape \\%c", lineNo, ch)
			}
			escaped = false
			continue
		}
		switch ch {
		case '\\':
			escaped = true
		case '"':
			if err := trailingOnlyComment(raw[i+1:], lineNo); err != nil {
				return "", err
			}
			return b.String(), nil
		default:
			b.WriteByte(ch)
		}
	}
	return "", fmt.Errorf("line %d: unterminated double-quoted value", lineNo)
}

func parseUnquoted(raw string) string {
	for i, r := range raw {
		if r == '#' && i > 0 {
			prev := raw[i-1]
			if prev == ' ' || prev == '\t' {
				return strings.TrimSpace(raw[:i])
			}
		}
	}
	return strings.TrimSpace(raw)
}

func trailingOnlyComment(raw string, lineNo int) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return nil
	}
	return fmt.Errorf("line %d: unexpected content after quoted value", lineNo)
}

func looksInterpolated(value string) bool {
	if strings.Contains(value, "$(") ||
		strings.Contains(value, "${") ||
		strings.Contains(value, "`") {
		return true
	}
	// SECR-01: also flag a bare unescaped $ followed by a letter/{/( so
	// $VAR values require explicit --literal, preventing silent truncation
	// in dotenv loaders that interpolate double-quoted values.
	for i := 0; i < len(value); i++ {
		if value[i] == '$' && i+1 < len(value) {
			next := value[i+1]
			if (next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') || next == '{' || next == '(' {
				return true
			}
		}
	}
	return false
}
