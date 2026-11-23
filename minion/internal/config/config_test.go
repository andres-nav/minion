package config

import (
	"os"
	"path/filepath"
	"reflect"
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
      "build": "latexmk -pdflua -outdir=build main.tex",
      "clean": "rm -rf build"
    },
    "python-gpu": {
      "detect": ["requirements.txt"],
      "test": "pytest",
      "run": "python spike.py",
      "host": "gpu02",
      "tasks": { "sweep": "python sweep.py" }
    }
  }
}`

// loadGlobal writes globalJSON to a temp file and loads it via MINION_CONFIG.
func loadGlobal(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(globalJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINION_CONFIG", path)
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
	if cmd, ok := r.Template.Lookup("build"); !ok || cmd == "" {
		t.Fatalf("build task missing: %q ok=%v", cmd, ok)
	}
	if r.HostName != "" { // no defaults, and latex sets no host
		t.Fatalf("host = %q, want empty", r.HostName)
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
	if r.HostName != "gpu02" { // template-level host wins over defaults
		t.Fatalf("host = %q, want gpu02", r.HostName)
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
	if r.GitRoot != root {
		t.Fatalf("git root = %q, want %q", r.GitRoot, root)
	}
	if r.TemplateName != "latex" {
		t.Fatalf("template = %q, want latex", r.TemplateName)
	}
}

func TestPerRepoOverride(t *testing.T) {
	cfg := loadGlobal(t)
	root := mkRepo(t, "main.tex") // would auto-detect latex...
	repoJSON := `{
  "use": "python-gpu",
  "host": "gpu99",
  "build": "python setup.py build"
}`
	if err := os.WriteFile(filepath.Join(root, ".minion.json"), []byte(repoJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := cfg.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.TemplateName != "python-gpu" { // `use` overrides detection
		t.Fatalf("template = %q, want python-gpu", r.TemplateName)
	}
	if r.HostName != "gpu99" { // per-repo host override
		t.Fatalf("host = %q, want gpu99", r.HostName)
	}
	if cmd, _ := r.Template.Lookup("build"); cmd != "python setup.py build" {
		t.Fatalf("build override = %q", cmd)
	}
	if cmd, _ := r.Template.Lookup("test"); cmd != "pytest" { // inherited from template
		t.Fatalf("inherited test = %q, want pytest", cmd)
	}
	if cmd, ok := r.Template.Lookup("sweep"); !ok || cmd != "python sweep.py" { // freeform inherited
		t.Fatalf("inherited sweep = %q ok=%v", cmd, ok)
	}
}

// A .minion.json may define the template it uses, plus a local host, without
// any global config entry.
func TestLocalTemplateResolves(t *testing.T) {
	cfg := loadGlobal(t)
	root := mkRepo(t)
	repoJSON := `{
  "use": "ai",
  "hosts": { "ds-ai": { "ssh": "ds-ai-box", "remote_dir": "~/workspace" } },
  "templates": {
    "ai": { "host": "ds-ai", "tasks": { "ai": "claude -p go" } }
  }
}`
	if err := os.WriteFile(filepath.Join(root, ".minion.json"), []byte(repoJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := cfg.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.TemplateName != "ai" {
		t.Fatalf("template = %q, want ai", r.TemplateName)
	}
	if r.HostName != "ds-ai" || r.Host.SSH != "ds-ai-box" {
		t.Fatalf("host = %q ssh=%q, want ds-ai/ds-ai-box", r.HostName, r.Host.SSH)
	}
	if cmd, ok := r.Template.Lookup("ai"); !ok || cmd != "claude -p go" {
		t.Fatalf("local task = %q ok=%v", cmd, ok)
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
	repoJSON := `{
  "use": "python-gpu",
  "templates": { "python-gpu": { "host": "gpu01", "test": "local-test" } }
}`
	if err := os.WriteFile(filepath.Join(root, ".minion.json"), []byte(repoJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := cfg.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if cmd, _ := r.Template.Lookup("test"); cmd != "local-test" {
		t.Fatalf("test = %q, want local-test (local override)", cmd)
	}
	if r.HostName != "gpu01" {
		t.Fatalf("host = %q, want gpu01", r.HostName)
	}
}

func TestOrderedTasks(t *testing.T) {
	cfg := loadGlobal(t)
	tmpl := cfg.Templates["python-gpu"]
	got := tmpl.OrderedTasks()

	// std verbs (test, run) first in canonical order, then freeform (sweep).
	want := []string{"test", "run", "sweep"}
	if len(got) != len(want) {
		t.Fatalf("got %d tasks, want %d: %+v", len(got), len(want), got)
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("task[%d] = %q, want %q (order: %+v)", i, got[i].Name, name, got)
		}
	}
}

// fullTemplate sets every field to a distinct value derived from prefix.
func fullTemplate(p string) Template {
	return Template{
		Detect: []string{p + "-detect"}, Host: p + "-host", RemoteDir: p + "-rd",
		Needs: []string{p + "-needs"}, Push: []string{p + "-push"}, Exclude: []string{p + "-excl"},
		Env: []string{p + "=1"}, Before: []string{p + "-before"},
		OnSuccess: []string{p + "-succ"}, OnFailure: []string{p + "-fail"}, Always: []string{p + "-always"},
		Fetch: []string{p + "-fetch"}, FetchInto: p + "-fetchinto", Build: p + "-build", Test: p + "-test",
		Run: p + "-run", Lint: p + "-lint", Clean: p + "-clean",
		Tasks:  map[string]string{"shared": p + "-shared", p + "-only": p + "-only"},
		Notify: &Notify{Email: &EmailSink{SMTP: "h:587", From: p + "@x", To: p + "@x"}},
	}
}

// Guards against drift: a new Template field not wired into mergeTemplate would
// leave out.Field == base.Field and fail here. Env/Tasks merge specially.
func TestMergeTemplateOverridesEveryField(t *testing.T) {
	base, over := fullTemplate("base"), fullTemplate("over")
	out := mergeTemplate(base, over)

	bv, tv := reflect.ValueOf(base), reflect.ValueOf(out)
	for i := 0; i < tv.NumField(); i++ {
		if reflect.DeepEqual(tv.Field(i).Interface(), bv.Field(i).Interface()) {
			t.Errorf("field %s not overridden", tv.Type().Field(i).Name)
		}
	}
	if want := []string{"base=1", "over=1"}; !reflect.DeepEqual(out.Env, want) {
		t.Errorf("Env = %v, want %v (additive)", out.Env, want)
	}
	wantTasks := map[string]string{"shared": "over-shared", "base-only": "base-only", "over-only": "over-only"}
	if !reflect.DeepEqual(out.Tasks, wantTasks) {
		t.Errorf("Tasks = %v, want %v", out.Tasks, wantTasks)
	}
}

// Zero-valued override fields are ignored.
func TestMergeTemplateEmptyOverrideKeepsBase(t *testing.T) {
	base := fullTemplate("base")
	if out := mergeTemplate(base, Template{}); !reflect.DeepEqual(out, base) {
		t.Errorf("empty override changed base:\n got %+v\nwant %+v", out, base)
	}
}

func TestUnknownUseErrors(t *testing.T) {
	cfg := loadGlobal(t)
	root := mkRepo(t)
	repoJSON := `{ "use": "nope" }`
	if err := os.WriteFile(filepath.Join(root, ".minion.json"), []byte(repoJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Resolve(root); err == nil {
		t.Fatal("use of an undefined template should error")
	}
	// an inline .minion.json with no `use` is still valid (name stays empty).
	if err := os.WriteFile(filepath.Join(root, ".minion.json"), []byte(`{ "build": "make" }`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := cfg.Resolve(root)
	if err != nil {
		t.Fatalf("inline config without use should not error: %v", err)
	}
	if cmd, _ := r.Template.Lookup("build"); cmd != "make" {
		t.Fatalf("inline build = %q, want make", cmd)
	}
}

func TestMissingGlobalConfigIsEmpty(t *testing.T) {
	t.Setenv("MINION_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.json"))
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
	if err := (&Notify{Email: &EmailSink{From: "a@x"}}).Validate(); err == nil {
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
