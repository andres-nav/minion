package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstall(t *testing.T) {
	dir := t.TempDir()
	src, prefix, gru := filepath.Join(dir, "src"), filepath.Join(dir, "prefix"), filepath.Join(dir, "prefix", "bin", "gru")
	for _, f := range []string{"gru", "gru-linux-arm64", "gru-darwin-arm64", "minion-linux-amd64", "SKILL.md"} {
		write(t, filepath.Join(src, f), f)
	}
	write(t, gru, "old")
	old, _ := os.Stat(gru)
	os.Mkdir(filepath.Join(dir, "claude"), 0o755)
	if err := install(src, prefix, filepath.Join(dir, "claude", "skills")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"prefix/bin/gru":                        "gru",
		"prefix/libexec/gru/gru-linux-arm64":    "gru-linux-arm64",
		"prefix/libexec/gru/minion-linux-amd64": "minion-linux-amd64",
		"claude/skills/gru/SKILL.md":            "SKILL.md",
	} {
		if b, err := os.ReadFile(filepath.Join(dir, path)); string(b) != want {
			t.Errorf("%s: %q err %v, want %q", path, b, err, want)
		}
	}
	if fi, _ := os.Stat(gru); os.SameFile(old, fi) || fi.Mode().Perm() != 0o755 {
		t.Errorf("gru not replaced by a new file with mode 0755: %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(dir, "claude/skills/gru/SKILL.md")); fi.Mode().Perm() != 0o644 {
		t.Errorf("SKILL.md mode %v, want 0644", fi.Mode())
	}
	for d, n := range map[string]int{"bin": 1, "libexec/gru": 2} { // no temp files, no macOS gru
		if e, _ := os.ReadDir(filepath.Join(prefix, d)); len(e) != n {
			t.Errorf("%s: %d files, want %d", d, len(e), n)
		}
	}

	if err := install(src, prefix, filepath.Join(dir, "none", "skills")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "none")); !os.IsNotExist(err) {
		t.Errorf("no Claude dir: the skill was installed anyway (%v)", err)
	}
	os.Remove(filepath.Join(src, "SKILL.md"))
	if err := install(src, prefix, filepath.Join(dir, "claude", "skills")); err == nil || !strings.Contains(err.Error(), "no SKILL.md") {
		t.Errorf("not a build or release dir: err %v", err)
	}
}

func TestUpgrade(t *testing.T) {
	defer func(v string) { version = v }(version)
	dir := t.TempDir()
	prefix, ran := filepath.Join(dir, "prefix"), filepath.Join(dir, "ran")
	files := map[string][]byte{}
	base := fakeReleases(t, files)
	for _, tag := range []string{"v1", "v2"} {
		// Its gru records how upgrade runs it. Old releases also have minion-*.tar.gz.
		addRelease(files, base, tag, tarGz(map[string]string{"gru": "#!/bin/sh\necho " + tag + " \"$@\" > " + ran + "\n"}), "minion-linux-amd64.tar.gz")
	}
	files["/latest"] = files["/tags/v2"]
	for _, c := range []struct{ version, want, ran string }{
		{"v1", "", "v2 install --prefix " + prefix},
		{"v2", "v1", "v1 install --prefix " + prefix},
		{"v2", "", ""}, // up to date
	} {
		version = c.version
		os.Remove(ran)
		if err := upgrade(base, c.want, filepath.Join(prefix, "bin", "gru")); err != nil {
			t.Fatalf("gru %s, upgrade to %q: %v", c.version, c.want, err)
		}
		if b, _ := os.ReadFile(ran); strings.TrimSpace(string(b)) != c.ran {
			t.Errorf("gru %s, upgrade to %q ran %q, want %q", c.version, c.want, b, c.ran)
		}
	}
}

func TestUpgradeRefuses(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "v1"
	dir := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(dir, "tmp"))
	os.Mkdir(filepath.Join(dir, "tmp"), 0o755)
	files := map[string][]byte{}
	base := fakeReleases(t, files)
	gru := tarGz(map[string]string{"gru": "#!/bin/sh\ntouch " + filepath.Join(dir, "ran") + "\n"})
	addRelease(files, base, "v2", gru)
	files["/dl/v2/checksums.txt"] = []byte(checksum([]byte("other"), archiveName()))
	addRelease(files, base, "v3", tarGz(map[string]string{"../evil": "x"}))
	files["/tags/v4"] = releaseJSON(base, "v4", "minion-linux-amd64.tar.gz")
	for want, msg := range map[string]string{
		"v2": "doesn't match the release's checksums.txt",
		"v3": `refused "../evil"`,
		"v4": "release v4 has no " + archiveName(),
	} {
		if err := upgrade(base, want, filepath.Join(dir, "bin", "gru")); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: err %v, want %q", want, err, msg)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); !os.IsNotExist(err) {
		t.Errorf("a refused release was installed")
	}
	if e, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(e) != 0 {
		t.Errorf("left in TMPDIR: %v", e)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func archiveName() string { return "gru-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz" }

// fakeReleases serves files by path, like GitHub's release API and downloads,
// and fails the test on a GET of anything else.
func fakeReleases(t *testing.T, files map[string][]byte) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			t.Errorf("GET %s: not served", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// addRelease adds release tag with archive as this platform's, its
// checksums.txt and the other assets, which mustn't be fetched.
func addRelease(files map[string][]byte, base, tag string, archive []byte, other ...string) {
	files["/tags/"+tag] = releaseJSON(base, tag, append(other, archiveName(), "checksums.txt")...)
	files["/dl/"+tag+"/"+archiveName()] = archive
	files["/dl/"+tag+"/checksums.txt"] = []byte(checksum(archive, archiveName()))
}

func releaseJSON(base, tag string, assets ...string) []byte {
	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}
	rel := struct {
		Tag    string  `json:"tag_name"`
		Assets []asset `json:"assets"`
	}{Tag: tag}
	for _, a := range assets {
		rel.Assets = append(rel.Assets, asset{a, base + "/dl/" + tag + "/" + a})
	}
	b, _ := json.Marshal(rel)
	return b
}

func checksum(b []byte, name string) string { return fmt.Sprintf("%x  %s\n", sha256.Sum256(b), name) }

func tarGz(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}
