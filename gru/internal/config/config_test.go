package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const globalJSON = `{
  "hosts": {
    "gpu01": { "ssh": "gpu01", "remote_dir": "~/experiments/{repo}" },
    "gpu02": { "ssh": "gpu02" }
  },
  "templates": {
    "latex": {
      "detect": ["main.tex"],
      "tasks": { "build": "latexmk -pdflua -outdir=build main.tex", "clean": "rm -rf build" }
    },
    "python-gpu": {
      "detect": ["requirements.txt"],
      "host": "gpu02",
      "env": ["A=1"],
      "notify": { "run": ["echo template"] },
      "tasks": { "test": "pytest", "sweep": "python sweep.py" }
    }
  }
}`

// loadGlobal writes globalJSON to a temp file and loads it via GRU_CONFIG.
func loadGlobal(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(globalJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRU_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// mkRepo creates a repo dir with a .git marker plus the given marker files.
func mkRepo(t *testing.T, markers ...string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range markers {
		if err := os.WriteFile(filepath.Join(root, m), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDetectTemplate(t *testing.T) {
	cfg := loadGlobal(t)
	root := mkRepo(t, "main.tex")

	r, err := cfg.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.TemplateName != "latex" {
		t.Fatalf("template = %q, want latex", r.TemplateName)
	}
	if cmd := r.Template.Tasks["build"]; cmd == "" {
		t.Fatal("build task missing")
	}
	if r.Template.Host != "" { // no defaults, and latex sets no host
		t.Fatalf("host = %q, want empty", r.Template.Host)
	}
}

func TestResolveFromSubdir(t *testing.T) {
	cfg := loadGlobal(t)
	root := mkRepo(t, "requirements.txt")
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	r, err := cfg.Resolve(sub)
	if err != nil {
		t.Fatal(err)
	}
	if r.Root != root {
		t.Fatalf("project root = %q, want %q", r.Root, root)
	}
	if r.TemplateName != "python-gpu" {
		t.Fatalf("template = %q, want python-gpu", r.TemplateName)
	}
	if r.Template.Host != "gpu02" {
		t.Fatalf("host = %q, want gpu02", r.Template.Host)
	}
}

func TestMonorepoSubproject(t *testing.T) {
	cfg := loadGlobal(t)
	// git root has no marker; a subdir is the actual project (paperwork layout).
	root := mkRepo(t) // just .git, no markers
	proj := filepath.Join(root, "applications", "acme")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "main.tex"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := cfg.Resolve(proj)
	if err != nil {
		t.Fatal(err)
	}
	if r.Root != proj {
		t.Fatalf("project root = %q, want %q", r.Root, proj)
	}
	if r.TemplateName != "latex" {
		t.Fatalf("template = %q, want latex", r.TemplateName)
	}
}

func TestPerRepoOverride(t *testing.T) {
	cfg := loadGlobal(t)
	root := mkRepo(t, "main.tex") // would auto-detect latex...
	writeRepo(t, root, `{
  "use": "python-gpu",
  "host": "gpu99",
  "env": ["B=2"],
  "notify": { "on": "failure", "email": { "smtp": "h:587", "from": "a@x", "to": "a@x" } },
  "tasks": { "build": "python setup.py build" }
}`)
	r, err := cfg.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.TemplateName != "python-gpu" { // `use` overrides detection
		t.Fatalf("template = %q, want python-gpu", r.TemplateName)
	}
	want := map[string]string{"build": "python setup.py build", "test": "pytest", "sweep": "python sweep.py"}
	if r.Template.Host != "gpu99" || !reflect.DeepEqual(r.Template.Tasks, want) {
		t.Fatalf("host %q tasks %v: want gpu99 and merged tasks", r.Template.Host, r.Template.Tasks)
	}
	if !reflect.DeepEqual(r.Template.Env, []string{"A=1", "B=2"}) { // env adds
		t.Fatalf("env = %v", r.Template.Env)
	}
	if n := r.Template.Notify; n.On != "failure" || n.Run != nil { // notify replaces
		t.Fatalf("notify = %+v, want the repo's block only", n)
	}
	if got := cfg.Templates["python-gpu"]; len(got.Tasks) != 2 || len(got.Env) != 1 {
		t.Fatalf("the global template changed: %+v", got)
	}
}

func writeRepo(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".gru.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A .gru.json may define the template it uses, plus a local host, without
// any global config entry.
func TestLocalTemplateResolves(t *testing.T) {
	cfg := loadGlobal(t)
	root := mkRepo(t)
	writeRepo(t, root, `{
  "use": "ai",
  "hosts": { "ds-ai": { "ssh": "ds-ai-box", "remote_dir": "~/workspace" } },
  "templates": {
    "ai": { "host": "ds-ai", "tasks": { "ai": "claude -p go" } }
  }
}`)

	r, err := cfg.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.TemplateName != "ai" {
		t.Fatalf("template = %q, want ai", r.TemplateName)
	}
	if r.Template.Host != "ds-ai" || r.Config.Hosts["ds-ai"].SSH != "ds-ai-box" {
		t.Fatalf("host = %q, want ds-ai with ssh ds-ai-box", r.Template.Host)
	}
	if cmd := r.Template.Tasks["ai"]; cmd != "claude -p go" {
		t.Fatalf("local task = %q", cmd)
	}
	// The global config must be untouched by the local merge.
	if _, ok := cfg.Templates["ai"]; ok {
		t.Fatal("local template leaked into the global config")
	}
}

// A local template with the same name as a global one wins.
func TestLocalTemplateOverridesGlobal(t *testing.T) {
	cfg := loadGlobal(t)
	root := mkRepo(t)
	writeRepo(t, root, `{
  "use": "python-gpu",
  "templates": { "python-gpu": { "host": "gpu01", "tasks": { "test": "local-test" } } }
}`)
	r, err := cfg.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if cmd := r.Template.Tasks["test"]; cmd != "local-test" {
		t.Fatalf("test = %q, want local-test (local override)", cmd)
	}
	if r.Template.Host != "gpu01" {
		t.Fatalf("host = %q, want gpu01", r.Template.Host)
	}
}

func TestTaskNames(t *testing.T) {
	cfg := loadGlobal(t)
	if got := cfg.Templates["python-gpu"].TaskNames(); !reflect.DeepEqual(got, []string{"sweep", "test"}) {
		t.Fatalf("task names = %v", got)
	}
}

// Unknown keys and bad notify blocks are refused, so a typo fails loudly.
func TestConfigErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("GRU_CONFIG", path)
	for body, want := range map[string]string{
		`{"templates": {"x": {"build": "make"}}}`:                  `unknown field "build"`,
		`{"hosts": {"h": {"remote": "x"}}}`:                        `unknown field "remote"`,
		`{"notify": {"on": "faliure", "run": ["x"]}}`:              `notify.on "faliure"`,
		`{"templates": {"x": {"notify": {"email": {"to": "a"}}}}}`: "template x: notify.email needs",
		`{"hosts": `: "unexpected EOF",
		`{"templates": {"x": {"env": ["MY-VAR=1"]}}}`: `template x: env "MY-VAR=1": not a variable name`,
	} {
		os.WriteFile(path, []byte(body), 0o644)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: err %v, want %q", body, err, want)
		}
	}
	cfg := loadGlobal(t)
	root := mkRepo(t)
	for body, want := range map[string]string{
		`{"use": "latex", "test": "x"}`:         `unknown field "test"`,
		`{"notify": {"on": "x", "run": ["y"]}}`: `notify.on "x"`,
		`{"env": ["A=1", "2B"]}`:                `env "2B": not a variable name`,
	} {
		writeRepo(t, root, body)
		if _, err := cfg.Resolve(root); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", body, err, want)
		}
	}
}

func TestUnknownUseErrors(t *testing.T) {
	cfg := loadGlobal(t)
	root := mkRepo(t)
	writeRepo(t, root, `{ "use": "nope" }`)
	if _, err := cfg.Resolve(root); err == nil {
		t.Fatal("use of an undefined template should error")
	}
	// an inline .gru.json with no `use` is still valid (name stays empty).
	writeRepo(t, root, `{ "tasks": { "build": "make" } }`)
	r, err := cfg.Resolve(root)
	if err != nil {
		t.Fatalf("inline config without use should not error: %v", err)
	}
	if r.Template.Tasks["build"] != "make" || r.TemplateName != "" {
		t.Fatalf("inline: template %q tasks %v", r.TemplateName, r.Template.Tasks)
	}
}

func TestMissingGlobalConfigIsEmpty(t *testing.T) {
	t.Setenv("GRU_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.json"))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load of missing config should not error: %v", err)
	}
	root := mkRepo(t, "main.tex")
	r, err := cfg.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.TemplateName != "" {
		t.Fatalf("template = %q, want empty (no config)", r.TemplateName)
	}
}

func TestNotifyValidate(t *testing.T) {
	if err := (*Notify)(nil).Validate(); err != nil {
		t.Errorf("nil notify should validate: %v", err)
	}
	if err := (&Notify{On: "maybe"}).Validate(); err == nil {
		t.Error("invalid on value should fail")
	}
	if err := (&Notify{Email: &EmailSink{SMTP: "h:587", From: "a@x"}}).Validate(); err == nil {
		t.Error("email missing smtp/to should fail")
	}
	if err := (&Notify{On: "failure", Email: &EmailSink{SMTP: "h:587", From: "a@x", To: "b@x"}}).Validate(); err != nil {
		t.Errorf("valid notify failed: %v", err)
	}
}

func TestNotifyForFallback(t *testing.T) {
	global := &Notify{Email: &EmailSink{SMTP: "h:587", From: "g@x", To: "g@x"}}
	own := &Notify{Email: &EmailSink{SMTP: "h:587", From: "t@x", To: "t@x"}}
	cfg := &Config{Notify: global}

	// template without notify falls back to the global default.
	if got := cfg.NotifyFor(Template{}); got != global {
		t.Errorf("empty template = %v, want global default", got)
	}
	// template with its own notify wins.
	if got := cfg.NotifyFor(Template{Notify: own}); got != own {
		t.Errorf("template override = %v, want own", got)
	}
	// no notify anywhere → nil.
	if got := (&Config{}).NotifyFor(Template{}); got != nil {
		t.Errorf("no notify = %v, want nil", got)
	}
}

func TestNotifyFires(t *testing.T) {
	cases := []struct {
		on   string
		code int
		want bool
	}{
		{"", 1, true}, {"always", 0, true}, {"failure", 0, false}, {"failure", 2, true},
		{"failure", -1, true}, {"success", 0, true}, {"success", 1, false},
	}
	for _, c := range cases {
		if got := (&Notify{On: c.on}).Fires(c.code); got != c.want {
			t.Errorf("on %q exit %d: fires %v, want %v", c.on, c.code, got, c.want)
		}
	}
}
