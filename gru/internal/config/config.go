package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type Config struct {
	Hosts     map[string]Host     `json:"hosts"`
	Templates map[string]Template `json:"templates"`
	Notify    *Notify             `json:"notify"` // default notification, used when a template sets none
}

// Notify describes how to announce a finished job. `on` gates when it fires
// ("always", "failure", or "success"; default "always"). Secret fields accept
// ${ENV} references, resolved locally at launch so credentials never live in
// the config or the repo.
type Notify struct {
	On    string            `json:"on"`
	Email *EmailSink        `json:"email,omitempty"`
	Run   []string          `json:"run,omitempty"` // shell run at job end with $GRU_* in the environment (e.g. a curl webhook)
	Env   map[string]string `json:"env,omitempty"` // extra vars exported for the run hooks; values accept ${ENV}
}

// EmailSink is an SMTP recipient. SMTP is host:port (e.g. smtp.gmail.com:587).
// User defaults to From; Pass is an SMTP/app password.
type EmailSink struct {
	SMTP string `json:"smtp"`
	From string `json:"from"`
	To   string `json:"to"`
	User string `json:"user"`
	Pass string `json:"pass"`
}

func (n *Notify) empty() bool {
	return n == nil || (n.Email == nil && len(n.Run) == 0)
}

// Validate reports a configuration error in the notify block, if any.
func (n *Notify) Validate() error {
	if n == nil {
		return nil
	}
	if !slices.Contains([]string{"", "always", "failure", "success"}, n.On) {
		return fmt.Errorf("notify.on %q must be always, failure or success", n.On)
	}
	if e := n.Email; e != nil && (e.SMTP == "" || e.From == "" || e.To == "") {
		return errors.New("notify.email needs smtp, from and to")
	}
	return nil
}

// Fires reports whether a job that exited with code is announced, per `on`.
func (n *Notify) Fires(code int) bool {
	switch n.On {
	case "failure":
		return code != 0
	case "success":
		return code == 0
	}
	return true
}

// NotifyFor returns the effective notify config for a template: its own block if
// set, else the top-level default. Returns nil when neither notifies.
func (c *Config) NotifyFor(t Template) *Notify {
	if !t.Notify.empty() {
		return t.Notify
	}
	if !c.Notify.empty() {
		return c.Notify
	}
	return nil
}

// Host is a remote target; SSH is the alias passed to the system ssh binary.
type Host struct {
	SSH       string `json:"ssh"`
	RemoteDir string `json:"remote_dir"`
	StateDir  string `json:"state_dir"` // jobs and the agent (default ~/.gru); keep it on local disk
}

// Template is a repo-type: its tasks and their environment (detection markers,
// host, lifecycle hooks, fetch paths).
type Template struct {
	Detect    []string          `json:"detect"`
	Host      string            `json:"host"`
	RemoteDir string            `json:"remote_dir"` // overrides the host's remote_dir (e.g. a workspace slot)
	Needs     []string          `json:"needs"`      // sibling packages to sync first (e.g. Brazil deps)
	Push      []string          `json:"push"`       // local dirs copied to the same remote path first (e.g. ~/.midway)
	Exclude   []string          `json:"exclude"`
	Env       []string          `json:"env"` // KEY=VALUE, exported before the command/hooks
	Before    []string          `json:"before"`
	OnSuccess []string          `json:"on_success"`
	OnFailure []string          `json:"on_failure"`
	Always    []string          `json:"always"`
	Fetch     []string          `json:"fetch"`
	FetchInto string            `json:"fetch_into"` // local dest for fetched results; supports {id} {ts} (UTC, 20261123T142501Z) {repo}
	Notify    *Notify           `json:"notify"`     // overrides the top-level default for this template
	Tasks     map[string]string `json:"tasks"`
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validate refuses a bad notify block, and an env name the job's export would fail on.
func (t Template) validate() error {
	for _, e := range t.Env {
		if k, _, _ := strings.Cut(e, "="); !envName.MatchString(k) {
			return fmt.Errorf("env %q: not a variable name", e)
		}
	}
	return t.Notify.Validate()
}

// TaskNames lists the template's tasks by name.
func (t Template) TaskNames() []string {
	return slices.Sorted(maps.Keys(t.Tasks))
}

func configPath() string {
	if p := os.Getenv("GRU_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "gru", "config.json")
}

// Load reads and checks the global config; a missing file yields an empty Config.
func Load() (*Config, error) {
	path := configPath()
	c := &Config{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err == nil {
		err = decode(b, c)
	}
	if err == nil {
		err = c.Notify.Validate()
	}
	for name, t := range c.Templates {
		if err == nil {
			if err = t.validate(); err != nil {
				err = fmt.Errorf("template %s: %w", name, err)
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// decode parses a config file, refusing unknown keys so a typo fails loudly.
func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
