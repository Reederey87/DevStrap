package cli

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestDoctorDetailsNeverCarryRawErrorStrings is a STRUCTURAL guard, not a
// behavioral one, and that is the point: converting doctor's 28 raw
// err.Error() interpolations to scrubbed() once is a cleanup, but nothing stops
// site #29 landing next quarter. This parses doctor.go, walks every
// checkResult composite literal, and fails on any Detail/Remedy value whose
// expression tree contains an .Error() call that is not wrapped in scrubbed()
// or redact.Scrub().
//
// Known limit, stated so a future reader is not misled: it only sees
// checkResult COMPOSITE LITERALS in this file. A checkResult assembled through
// a local variable, or built in another file, is not covered. That is
// acceptable because the whole doctor check surface is written in this one
// file in that one style — but if that changes, this guard silently narrows
// rather than failing, so widen it deliberately at that point.
func TestDoctorDetailsNeverCarryRawErrorStrings(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "doctor.go", nil, 0)
	if err != nil {
		t.Fatalf("parse doctor.go: %v", err)
	}

	// wrapped reports whether expr is a scrubbed(...) / redact.Scrub(...) call.
	wrapped := func(expr ast.Expr) bool {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return false
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			return fn.Name == "scrubbed"
		case *ast.SelectorExpr:
			pkg, ok := fn.X.(*ast.Ident)
			return ok && pkg.Name == "redact" && fn.Sel.Name == "Scrub"
		}
		return false
	}

	// rawErrorCall finds an .Error() call anywhere under expr that is not
	// itself inside a scrubbing wrapper.
	rawErrorCall := func(expr ast.Expr) bool {
		if expr == nil || wrapped(expr) {
			return false
		}
		found := false
		ast.Inspect(expr, func(n ast.Node) bool {
			if found {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if wrapped(call) {
				return false // do not descend into a scrubbed(...) argument
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Error" && len(call.Args) == 0 {
				found = true
				return false
			}
			return true
		})
		return found
	}

	// Match on the FIELD NAME anywhere in the file, not on the literal's type.
	// The first version of this guard keyed on `lit.Type` being the ident
	// `checkResult` — which silently missed every site written as
	// `[]checkResult{{Name: ..., Detail: ...}}`, because an element of a slice
	// literal has an ELIDED type (lit.Type == nil). That is most of this file,
	// and it made the guard pass against a deliberately reverted site. doctor.go
	// has exactly one struct with Detail/Remedy fields, so keying on the field
	// name is both simpler and strictly broader.
	checked := 0
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || (key.Name != "Detail" && key.Name != "Remedy") {
			return true
		}
		checked++
		if rawErrorCall(kv.Value) {
			t.Errorf("%s: checkResult.%s interpolates a raw .Error(); wrap it in scrubbed(err) — doctor output reaches both the human table and --json",
				fset.Position(kv.Pos()), key.Name)
		}
		return true
	})

	// Guard the guard: if the literal style changes and this stops matching,
	// the test would pass vacuously forever. 100+ Detail/Remedy fields exist
	// today; a floor well above the explicit-literal-only subset that fooled
	// the first version.
	if checked < 90 {
		t.Fatalf("only inspected %d Detail/Remedy fields; the guard has stopped matching doctor.go and is no longer protecting anything", checked)
	}
}

// TestScrubbedRedactsCredentialBearingErrors is the BEHAVIORAL half. The AST
// guard above proves every Detail/Remedy routes through scrubbed(); this proves
// scrubbed() actually redacts. Neither alone is sufficient — a guard over an
// identity function would pass, and a redaction test with unguarded call sites
// would not stop the next raw one landing.
//
// The inputs are the shapes doctor genuinely produces: a hub remote URL with
// userinfo (checkHubHealth's open/reachability paths), an ssh git remote (the
// git-state and WIP checks surface raw git errors), and a JSON token field.
func TestScrubbedRedactsCredentialBearingErrors(t *testing.T) {
	cases := []struct {
		name, in, mustNotContain string
	}{
		{"https userinfo", "open hub: https://user:s3cr3t@example.invalid/x failed", "s3cr3t"},
		{"ssh userinfo", "git error: ssh://git:hunter2@github.com/a/b.git", "hunter2"},
		{"json token field", `{"token": "abcdef1234567890abcdef"}`, "abcdef1234567890abcdef"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scrubbed(errors.New(c.in))
			if strings.Contains(got, c.mustNotContain) {
				t.Fatalf("scrubbed(%q) = %q, still contains the secret %q", c.in, got, c.mustNotContain)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("scrubbed(%q) = %q, want a [REDACTED] marker", c.in, got)
			}
		})
	}
	if scrubbed(nil) != "" {
		t.Fatalf("scrubbed(nil) = %q, want empty", scrubbed(nil))
	}
}
