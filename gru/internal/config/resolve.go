package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

const repoFile = ".gru.json"

// RepoConfig is a per-project .gru.json: it picks a template via `use` and
// may override any Template field (embedded fields sit at the JSON top level).
// Its own `templates` and `hosts` are merged over the global config (local
// wins), so a project can `use` a template defined right in its .gru.json.
type RepoConfig struct {
	Use       string              `json:"use"`
	Hosts     map[string]Host     `json:"hosts"`
	Templates map[string]Template `json:"templates"`
	Template
}

// Resolved is the effective config for a directory after detection and
// per-project overrides.
type Resolved struct {
	Root         string // project root: nearest ancestor with a marker or .gru.json
	TemplateName string
	Template     Template
	RepoFile     string
	Ambiguous    []string // other templates that also matched at Root
	Config       *Config  // effective config: global plus the .gru.json hosts/templates
}

// Resolve walks up from dir; the first level with a .gru.json or a matching
// template's detection markers is the project root. This handles monorepos of
// projects (e.g. paperwork/<app>/main.tex). The walk stops at the git root
// (inclusive) or the filesystem root.
func (c *Config) Resolve(dir string) (*Resolved, error) {
	dir = filepath.Clean(dir)
	gr := gitRoot(dir)
	for cur := dir; ; cur = filepath.Dir(cur) {
		path := filepath.Join(cur, repoFile)
		b, err := os.ReadFile(path)
		if err == nil {
			return c.resolveRepo(cur, path, b)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if m := c.detect(cur); len(m) > 0 {
			return &Resolved{Root: cur, TemplateName: m[0], Template: c.Templates[m[0]], Ambiguous: m[1:], Config: c}, nil
		}
		if cur == gr || cur == filepath.Dir(cur) {
			break
		}
	}
	if gr != "" {
		dir = gr
	}
	return &Resolved{Root: dir, Config: c}, nil
}

func (c *Config) resolveRepo(root, path string, b []byte) (*Resolved, error) {
	rc := &RepoConfig{}
	if err := decode(b, rc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	merged := &Config{Hosts: map[string]Host{}, Templates: map[string]Template{}, Notify: c.Notify}
	maps.Copy(merged.Hosts, c.Hosts)
	maps.Copy(merged.Hosts, rc.Hosts)
	maps.Copy(merged.Templates, c.Templates)
	maps.Copy(merged.Templates, rc.Templates)

	r := &Resolved{Root: root, TemplateName: rc.Use, RepoFile: path, Config: merged}
	if rc.Use == "" {
		if m := merged.detect(root); len(m) > 0 {
			r.TemplateName, r.Ambiguous = m[0], m[1:]
		}
	} else if _, ok := merged.Templates[rc.Use]; !ok {
		return nil, fmt.Errorf("%s: use %q is not a defined template", path, rc.Use)
	}

	// The .gru.json fields overlay a copy of the template: lists and values
	// replace, tasks merge by name, env adds, a notify block replaces.
	base := merged.Templates[r.TemplateName]
	cp, _ := json.Marshal(base)
	json.Unmarshal(cp, &r.Template)
	if rc.Notify != nil {
		r.Template.Notify = nil
	}
	json.Unmarshal(b, &r.Template)
	if len(rc.Env) > 0 {
		r.Template.Env = append(slices.Clone(base.Env), rc.Env...)
	}
	return r, r.Template.Notify.Validate()
}

// detect returns, sorted, the templates with a detection marker present in root.
func (c *Config) detect(root string) []string {
	var out []string
	for name, t := range c.Templates {
		if slices.ContainsFunc(t.Detect, func(m string) bool {
			_, err := os.Stat(filepath.Join(root, m))
			return err == nil
		}) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func gitRoot(dir string) string {
	for ; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if dir == filepath.Dir(dir) {
			return ""
		}
	}
}
