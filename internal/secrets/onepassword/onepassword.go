// Package onepassword wraps `op` CLI subprocess calls for browsing 1Password
// items (field identities only, never values) and writing a value into
// 1Password (via a private JSON template file, never a bare CLI argument).
// It backs `devstrap env op list`/`env op set` (W12-03).
//
// Security posture, matching 1Password's own CLI best-practices guidance
// (https://developer.1password.com/docs/cli/best-practices/): an inline
// `op item edit field=value` assignment is visible in shell history and to
// other local processes inspecting argv, so a value this package writes
// always travels through a private (mode-0700 dir, mode-0600 file) JSON
// template passed via `op item edit --template=<file>`, and that file is
// removed before the call returns, on every path. Browsing never passes
// `--reveal`, and the Field type this package decodes `op item get`'s JSON
// into has no value member, so a field's secret value has nowhere to land
// even if the CLI's JSON output includes it.
//
// Every subprocess runs under the shared sanitized child environment
// (internal/childenv, BasicAllowlist + OP_*) with a bounded timeout,
// mirroring the existing `op read`/`op inject`/`op run` call sites in
// internal/cli (resolveOpRef, injectProviderRefs, runtimeEnvCommand).
package onepassword

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/childenv"
	"github.com/Reederey87/DevStrap/internal/redact"
)

// CommandTimeout bounds a single `op` subprocess call — long enough for an
// interactive biometric/desktop-app unlock approval but finite so a wedged
// prompt cannot hang `env op list`/`env op set` forever. A var so tests can
// shrink it (mirrors internal/cli's opReadTimeout for `op read`).
var CommandTimeout = 60 * time.Second

// LookPath reports whether the `op` binary is present on PATH, returning an
// actionable error naming what requires it and how to fix it if not.
func LookPath() error {
	if _, err := exec.LookPath("op"); err != nil {
		return fmt.Errorf("devstrap env op requires the 1Password CLI (`op`) on PATH; install it from https://developer.1password.com/docs/cli/get-started/ and run `op signin`: %w", err)
	}
	return nil
}

// Item is a 1Password item summary: id/title/vault only. `op item list
// --format=json` and `op item get --format=json` both carry more fields
// (category, tags, timestamps, and — for get — field values); this struct
// deliberately decodes only what browsing needs.
type Item struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Vault struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"vault"`
}

// Field is one field's identity within an item — never its value. Decoding
// `op item get --format=json` into a struct with no Value member means a
// field's secret value has no reachable Go field to land in, regardless of
// whether the CLI's JSON output includes it.
type Field struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Type  string `json:"type"`
}

type itemDetail struct {
	Fields []Field `json:"fields"`
}

// ListItems runs `op item list --format=json`, optionally scoped to a single
// vault, and returns item summaries. Never passes --reveal.
func ListItems(ctx context.Context, vault string) ([]Item, error) {
	args := []string{"item", "list", "--format=json"}
	if vault != "" {
		args = append(args, "--vault", vault)
	}
	out, err := run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var items []Item
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("parse op item list output: %w", err)
	}
	return items, nil
}

// ListFields runs `op item get <id> --format=json` and returns the item's
// field identities. Never passes --reveal.
func ListFields(ctx context.Context, itemID string) ([]Field, error) {
	out, err := run(ctx, "item", "get", itemID, "--format=json")
	if err != nil {
		return nil, err
	}
	var detail itemDetail
	if err := json.Unmarshal(out, &detail); err != nil {
		return nil, fmt.Errorf("parse op item get output: %w", err)
	}
	return detail.Fields, nil
}

// templateField/templatePayload are the minimal `op item edit --template`
// JSON shape (https://developer.1password.com/docs/cli/edit-items/). Only
// the field being changed is included — deliberately never a full
// fetch-modify-write of the item, which would pull every sibling field's
// plaintext value into this process just to write one field back out.
type templateField struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

type templatePayload struct {
	Fields []templateField `json:"fields"`
}

// SetField writes value into vault/item's field via a private JSON template
// file passed to `op item edit --template=<file>` — never as a bare
// `field=value` CLI argument, which 1Password's own CLI guidance warns is
// visible in shell history and to other local processes. The template lives
// in its own freshly created, mode-0700 temp directory (mode-0600 file
// inside it) and both are removed before this function returns, on every
// path — success or error. The named return + deferred cleanup mirrors
// internal/cli's writeEnvBlob (CODE-04): a cleanup failure is folded into the
// returned error on the success path rather than silently discarded, so a
// template that could not be removed is never reported as if it had been
// (W12-03 review). Returns the resulting op://vault/item/field reference.
func SetField(ctx context.Context, vault, item, field, value string) (ref string, err error) {
	if vault == "" || item == "" || field == "" {
		return "", fmt.Errorf("vault, item, and field are all required to write a 1Password value")
	}
	dir, err := os.MkdirTemp("", "devstrap-op-template-*")
	if err != nil {
		return "", fmt.Errorf("create private template dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil && err == nil {
			err = fmt.Errorf("remove private template dir %s (may still contain the plaintext value -- remove it manually): %w", dir, rmErr)
			ref = ""
		}
	}()
	tmplPath := filepath.Join(dir, "template.json")
	payload := templatePayload{Fields: []templateField{{ID: field, Label: field, Type: "CONCEALED", Value: value}}}
	raw, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return "", fmt.Errorf("build op edit template: %w", marshalErr)
	}
	if writeErr := os.WriteFile(tmplPath, raw, 0o600); writeErr != nil {
		return "", fmt.Errorf("write op edit template: %w", writeErr)
	}
	if _, runErr := run(ctx, "item", "edit", item, "--vault", vault, "--template="+tmplPath); runErr != nil {
		return "", runErr
	}
	return fmt.Sprintf("op://%s/%s/%s", vault, item, field), nil
}

// run executes `op <args...>` under the sanitized child environment
// (BasicAllowlist + OP_*), bounded by CommandTimeout, and returns stdout.
// Stderr is captured, scrubbed, and folded into the returned error so callers
// get an actionable message without printing raw subprocess output
// themselves. No argument in args is ever a secret value: browsing reads
// field identities only, and SetField's value travels through a template
// file, never argv.
func run(ctx context.Context, args ...string) ([]byte, error) {
	env, err := childenv.FromOS(append(childenv.BasicAllowlist(), "OP_*"), nil)
	if err != nil {
		return nil, err
	}
	octx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(octx, "op", args...) //nolint:gosec // fixed 1Password CLI subcommands with explicit argv and a sanitized environment; no secret value ever reaches args.
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		msg := redact.Scrub(strings.TrimSpace(stderr.String()))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("op %s failed: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}
