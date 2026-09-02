package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const repoFile = ".minion.json"

// Resolved is the effective config for a directory after detection and
// per-project overrides.
type Resolved struct {
	Root         string // project root: nearest ancestor with a marker or .minion.json
	GitRoot      string // nearest .git ancestor (sync base); may equal Root or be ""
	TemplateName string
	HostName     string
	Host         Host
	Template     Template
	RepoFile     string
	Ambiguous    []string // other templates that also matched at Root
}

func gitRoot(dir string) string {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Resolve walks up from dir; the first level with a .minion.json or a matching
// template's detection markers is the project root. This handles monorepos of
// projects (e.g. paperwork/<app>/main.tex). The walk stops at the git root
// (inclusive) or the filesystem root.
func (c *Config) Resolve(dir string) (*Resolved, error) {
	dir = filepath.Clean(dir)
	gr := gitRoot(dir)

	for cur := dir; ; {
		rcPath := filepath.Join(cur, repoFile)
		b, err := os.ReadFile(rcPath)
		switch {
		case err == nil:
			rc := &RepoConfig{}
			if uErr := json.Unmarshal(b, rc); uErr != nil {
				return nil, fmt.Errorf("parse %s: %w", rcPath, uErr)
			}
			return c.finish(cur, gr, rc, rcPath), nil
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}

		if matches := c.detect(cur); len(matches) > 0 {
			sort.Strings(matches)
			return c.build(cur, gr, matches[0], matches[1:], nil, ""), nil
		}

		if cur == gr {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}

	root := gr
	if root == "" {
		root = dir
	}
	return c.build(root, gr, "", nil, nil, ""), nil
}

func (c *Config) finish(root, gr string, rc *RepoConfig, rcPath string) *Resolved {
	name := rc.Use
	var ambiguous []string
	if name == "" {
		matches := c.detect(root)
		sort.Strings(matches)
		if len(matches) > 0 {
			name = matches[0]
			ambiguous = matches[1:]
		}
	}
	return c.build(root, gr, name, ambiguous, rc, rcPath)
}

func (c *Config) build(root, gr, name string, ambiguous []string, rc *RepoConfig, rcPath string) *Resolved {
	tmpl := c.Templates[name]
	if rc != nil {
		tmpl = mergeTemplate(tmpl, rc.Template)
	}
	return &Resolved{
		Root:         root,
		GitRoot:      gr,
		TemplateName: name,
		HostName:     tmpl.Host,
		Host:         c.Hosts[tmpl.Host],
		Template:     tmpl,
		RepoFile:     rcPath,
		Ambiguous:    ambiguous,
	}
}

// detect returns templates with at least one detection marker present in root.
func (c *Config) detect(root string) []string {
	var out []string
	for name, t := range c.Templates {
		for _, marker := range t.Detect {
			if _, err := os.Stat(filepath.Join(root, marker)); err == nil {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// mergeTemplate overlays non-empty fields of over onto base; Tasks merge
// key-by-key.
func mergeTemplate(base, over Template) Template {
	out := base
	if len(over.Detect) > 0 {
		out.Detect = over.Detect
	}
	if over.Host != "" {
		out.Host = over.Host
	}
	if over.RemoteDir != "" {
		out.RemoteDir = over.RemoteDir
	}
	if len(over.Needs) > 0 {
		out.Needs = over.Needs
	}
	if len(over.Push) > 0 {
		out.Push = over.Push
	}
	if len(over.Exclude) > 0 {
		out.Exclude = over.Exclude
	}
	if len(over.Env) > 0 {
		// env is additive: template vars plus per-repo vars (later wins).
		out.Env = append(append([]string{}, base.Env...), over.Env...)
	}
	if len(over.Before) > 0 {
		out.Before = over.Before
	}
	if len(over.OnSuccess) > 0 {
		out.OnSuccess = over.OnSuccess
	}
	if len(over.OnFailure) > 0 {
		out.OnFailure = over.OnFailure
	}
	if len(over.Always) > 0 {
		out.Always = over.Always
	}
	if len(over.Fetch) > 0 {
		out.Fetch = over.Fetch
	}
	if over.Build != "" {
		out.Build = over.Build
	}
	if over.Test != "" {
		out.Test = over.Test
	}
	if over.Run != "" {
		out.Run = over.Run
	}
	if over.Lint != "" {
		out.Lint = over.Lint
	}
	if over.Clean != "" {
		out.Clean = over.Clean
	}
	if len(over.Tasks) > 0 {
		merged := make(map[string]string, len(base.Tasks)+len(over.Tasks))
		for k, v := range base.Tasks {
			merged[k] = v
		}
		for k, v := range over.Tasks {
			merged[k] = v
		}
		out.Tasks = merged
	}
	return out
}
