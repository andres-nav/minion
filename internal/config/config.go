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

type Config struct {
	Hosts     map[string]Host     `json:"hosts"`
	Templates map[string]Template `json:"templates"`
}

// Host is a remote target; SSH is the alias passed to the system ssh binary.
type Host struct {
	SSH       string `json:"ssh"`
	RemoteDir string `json:"remote_dir"`
}

// Template is a repo-type: standard verbs, freeform tasks, and the environment
// (detection markers, host, lifecycle hooks, fetch paths).
type Template struct {
	Detect    []string `json:"detect"`
	Host      string   `json:"host"`
	RemoteDir string   `json:"remote_dir"` // overrides the host's remote_dir (e.g. a workspace slot)
	Needs     []string `json:"needs"`      // sibling packages to sync first (e.g. Brazil deps)
	Push      []string `json:"push"`       // local dirs copied to the same remote path first (e.g. ~/.midway)
	Exclude   []string `json:"exclude"`
	Env       []string `json:"env"` // KEY=VALUE, exported before the command/hooks
	Before    []string `json:"before"`
	OnSuccess []string `json:"on_success"`
	OnFailure []string `json:"on_failure"`
	Always    []string `json:"always"`
	Fetch     []string `json:"fetch"`

	Build string `json:"build"`
	Test  string `json:"test"`
	Run   string `json:"run"`
	Lint  string `json:"lint"`
	Clean string `json:"clean"`

	Tasks map[string]string `json:"tasks"`
}

// RepoConfig is a per-project .minion.json: it picks a template via `use` and
// may override any Template field (embedded fields sit at the JSON top level).
type RepoConfig struct {
	Use string `json:"use"`
	Template
}

var StdVerbs = []string{"build", "test", "run", "lint", "clean"}

type Task struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

func (t Template) verb(name string) string {
	switch name {
	case "build":
		return t.Build
	case "test":
		return t.Test
	case "run":
		return t.Run
	case "lint":
		return t.Lint
	case "clean":
		return t.Clean
	}
	return ""
}

// Lookup returns the command for a task name (standard verb or freeform task).
func (t Template) Lookup(name string) (string, bool) {
	if c := t.verb(name); c != "" {
		return c, true
	}
	c, ok := t.Tasks[name]
	return c, ok
}

// OrderedTasks lists standard verbs first (canonical order), then freeform
// tasks sorted by name.
func (t Template) OrderedTasks() []Task {
	var out []Task
	seen := map[string]bool{}
	for _, n := range StdVerbs {
		if c := t.verb(n); c != "" {
			out = append(out, Task{n, c})
			seen[n] = true
		}
	}
	var extra []string
	for k := range t.Tasks {
		if !seen[k] && t.verb(k) == "" {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		out = append(out, Task{k, t.Tasks[k]})
	}
	return out
}

func globalConfigPath() string {
	if p := os.Getenv("MINION_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "minion", "config.json")
}

// Load reads the global config; a missing file yields an empty Config.
func Load() (*Config, error) {
	return loadFrom(globalConfigPath())
}

func loadFrom(path string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}
