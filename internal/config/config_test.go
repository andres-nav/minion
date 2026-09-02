package config

import (
	"os"
	"path/filepath"
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

func TestMissingGlobalConfigIsEmpty(t *testing.T) {
	t.Setenv("MINION_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.toml"))
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
