package cli

import (
	"encoding/json"
	"io"
)

// render writes a command's terminal output through a single seam (P5-CLI-01):
// when --json is set it encodes the typed value v as indented JSON; otherwise it
// invokes the human-render callback. Routing every command's output through this
// (rather than ad-hoc `if json` blocks inside business logic) is how `--json`
// becomes a uniform contract instead of a flag a minority of commands honor.
func (o *options) render(w io.Writer, human func(io.Writer) error, v any) error {
	if o.v.GetBool("json") {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	return human(w)
}
