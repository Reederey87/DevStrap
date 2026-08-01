// Package manifest implements the plain-text workspace manifest (AD-7): the
// escape hatch that answers "what happens to my namespace if DevStrap goes
// away?" with an artifact a tool DevStrap does not control can consume.
//
// The document IS a vcstool `.repos` file — root key `repositories`, entries
// keyed by relative path, each `{type, url, version}` — rather than a DevStrap
// format that resembles one. That distinction is the entire point: `db backup
// --full` is already a complete backup, but only DevStrap can restore it, so it
// is not an escape hatch. `vcs import < workspace.yaml` reconstructs the git
// projects with an unrelated binary, which is what makes the claim checkable.
//
// DevStrap's own metadata lives under ONE top-level `devstrap` key. vcstool
// reads only `root["repositories"]` and only the `type`/`url`/`version`
// attributes inside each entry, so a sibling top-level key is ignored by
// construction. Interleaving DevStrap fields into the entries vcstool parses
// would put our evolution and its parser in the same namespace forever.
//
// Scope of the interop claim, stated here because the manifest header states it
// to users: it holds for `git_repo` projects only. `local_git`, `plain_folder`
// and `draft_project` rows have no `{url, version}` to clone, so they are
// recorded under `devstrap.projects` for `devstrap import` and are structurally
// invisible to `vcs import`. Encrypted content (captured env profiles, draft
// bundles) is out of scope entirely — a plaintext manifest cannot carry
// age-encrypted blobs and must not appear to.
package manifest

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// SchemaVersion is the manifest's contract floor (AD5-01 evolution rule): every
// key documented at version N is still present, with the same name and meaning,
// at every version >= N. New keys may be added WITHOUT a bump, so a consumer
// must ignore keys it does not recognize; the version is bumped only when a
// consumer written against the previous version would have to change.
const SchemaVersion = 1

// VCSTypeGit is the only `type` value DevStrap emits or imports. vcstool also
// understands hg/svn/bzr; DevStrap's materialization plane is git-only, so a
// foreign type is reported and skipped rather than registered as a git repo
// that could never clone.
const VCSTypeGit = "git"

// RepoEntry is one vcstool `repositories` entry. These three keys are the whole
// of what `vcs import` reads. Nothing may be added here — DevStrap fields go in
// Project, under the `devstrap` key.
type RepoEntry struct {
	Type    string `yaml:"type"`
	URL     string `yaml:"url"`
	Version string `yaml:"version,omitempty"`
}

// Project is DevStrap's per-path metadata. For a git_repo it deliberately does
// NOT repeat the remote URL: `repositories` is the single source of truth for
// that, so the two can never disagree after a hand edit.
type Project struct {
	Type                  string `yaml:"type"`
	DefaultBranch         string `yaml:"default_branch,omitempty"`
	LFSPolicy             string `yaml:"lfs_policy,omitempty"`
	ForgeKind             string `yaml:"forge_kind,omitempty"`
	MaterializationPolicy string `yaml:"materialization_policy,omitempty"`
	// EnvProfile names that this project HAS a captured/bound env profile. It
	// never carries one: the values are age-encrypted to device recipients and
	// are recoverable only through DevStrap (`db backup --full` + `sync`).
	EnvProfile bool `yaml:"env_profile,omitempty"`
}

// Section is the `devstrap` key: everything vcstool ignores.
type Section struct {
	SchemaVersion int    `yaml:"schema_version"`
	WorkspaceID   string `yaml:"workspace_id"`
	WorkspaceName string `yaml:"workspace_name,omitempty"`
	ExportedAt    string `yaml:"exported_at"`
	ExportedBy    string `yaml:"exported_by_device,omitempty"`
	// Pinned records whether `version` holds resolved SHAs (`export --pinned`,
	// mirroring `vcs export --exact`) or branch names.
	Pinned   bool               `yaml:"pinned"`
	Projects map[string]Project `yaml:"projects"`
}

// Manifest is the whole document.
type Manifest struct {
	Repositories map[string]RepoEntry `yaml:"repositories"`
	DevStrap     Section              `yaml:"devstrap"`
}

// ErrNotAManifest reports a YAML document that is neither a vcstool `.repos`
// file nor a DevStrap manifest.
var ErrNotAManifest = errors.New("not a workspace manifest: no `repositories` key")

// header is emitted as leading comments on every exported manifest. A user
// reading this artifact mid-disaster should not have to find the spec to learn
// what it can and cannot rebuild, so the honest scoping lives in the file
// itself, not only in spec/16.
const header = `DevStrap workspace manifest — vcstool ".repos" schema.

Recover WITHOUT DevStrap:  vcs import <target-dir> < workspace.yaml
                           (https://github.com/dirk-thomas/vcstool)
Recover WITH DevStrap:     devstrap import --manifest workspace.yaml && devstrap sync

SCOPE — what a third-party tool can rebuild from this file:
  "vcs import" reconstructs the git projects listed under "repositories"
  below, and ONLY those. Projects of type local_git, plain_folder and
  draft_project have no clonable remote, so they appear under
  "devstrap.projects" for "devstrap import" and are structurally invisible
  to vcstool. This is not whole-tree third-party recovery, and does not
  claim to be.

NO SECRETS — this file is plain text and carries NO encrypted content.
  Captured env profiles and draft-folder bundles are age-encrypted to
  device recipients; they are recoverable only through DevStrap, from
  "db backup --full" plus "devstrap sync". The "env_profile: true" marker
  below names which projects HAVE a profile. It never carries one.`

// devstrapKeyComment sits on the `devstrap` key so a reader of the raw file
// knows the boundary between the interop schema and DevStrap's own fields.
const devstrapKeyComment = `DevStrap-specific metadata. vcstool reads only the "repositories" key
above and ignores this one entirely.`

// Encode renders m as the manifest document: leading header comments, then
// `repositories`, then `devstrap`. Map keys are emitted in sorted order rather
// than Go's map order so two exports of the same namespace are byte-identical
// and a golden-file assertion can pin the schema.
func Encode(m Manifest) ([]byte, error) {
	repos := &yaml.Node{Kind: yaml.MappingNode}
	for _, path := range sortedRepoPaths(m.Repositories) {
		entry := m.Repositories[path]
		fields := []*yaml.Node{
			str("type"), str(entry.Type),
			str("url"), str(entry.URL),
		}
		if entry.Version != "" {
			fields = append(fields, str("version"), str(entry.Version))
		}
		repos.Content = append(repos.Content, str(path), &yaml.Node{Kind: yaml.MappingNode, Content: fields})
	}

	projects := &yaml.Node{Kind: yaml.MappingNode}
	for _, path := range sortedProjectPaths(m.DevStrap.Projects) {
		p := m.DevStrap.Projects[path]
		fields := []*yaml.Node{str("type"), str(p.Type)}
		fields = appendIfSet(fields, "default_branch", p.DefaultBranch)
		fields = appendIfSet(fields, "lfs_policy", p.LFSPolicy)
		fields = appendIfSet(fields, "forge_kind", p.ForgeKind)
		fields = appendIfSet(fields, "materialization_policy", p.MaterializationPolicy)
		if p.EnvProfile {
			fields = append(fields, str("env_profile"), boolean(true))
		}
		projects.Content = append(projects.Content, str(path), &yaml.Node{Kind: yaml.MappingNode, Content: fields})
	}

	section := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		str("schema_version"), integer(m.DevStrap.SchemaVersion),
		str("workspace_id"), str(m.DevStrap.WorkspaceID),
	}}
	if m.DevStrap.WorkspaceName != "" {
		section.Content = append(section.Content, str("workspace_name"), str(m.DevStrap.WorkspaceName))
	}
	section.Content = append(section.Content, str("exported_at"), str(m.DevStrap.ExportedAt))
	if m.DevStrap.ExportedBy != "" {
		section.Content = append(section.Content, str("exported_by_device"), str(m.DevStrap.ExportedBy))
	}
	section.Content = append(section.Content,
		str("pinned"), boolean(m.DevStrap.Pinned),
		str("projects"), projects,
	)

	reposKey := str("repositories")
	reposKey.HeadComment = header
	devstrapKey := str("devstrap")
	devstrapKey.HeadComment = devstrapKeyComment

	root := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		reposKey, repos,
		devstrapKey, section,
	}}

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("encode workspace manifest: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode workspace manifest: %w", err)
	}
	return []byte(buf.String()), nil
}

// Decode parses a manifest. Unknown keys are IGNORED rather than rejected, per
// the SchemaVersion contract — a manifest written by a newer DevStrap must stay
// readable. A plain vcstool `.repos` file with no `devstrap` key decodes
// successfully with an empty Section, so a hand-written or third-party `.repos`
// file can be imported directly.
func Decode(raw []byte) (Manifest, error) {
	// Probe for the root key first: yaml.Unmarshal of an unrelated mapping
	// succeeds with a zero Manifest, which would silently import nothing.
	var probe map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return Manifest{}, fmt.Errorf("parse workspace manifest: %w", err)
	}
	if _, ok := probe["repositories"]; !ok {
		return Manifest{}, ErrNotAManifest
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse workspace manifest: %w", err)
	}
	return m, nil
}

func appendIfSet(fields []*yaml.Node, key, value string) []*yaml.Node {
	if value == "" {
		return fields
	}
	return append(fields, str(key), str(value))
}

func sortedRepoPaths(m map[string]RepoEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedProjectPaths(m map[string]Project) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func str(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func boolean(v bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprintf("%t", v)}
}

func integer(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", v)}
}
