package cli

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// releases is gru's releases in GitHub's API; GRU_RELEASES overrides it.
const releases = "https://api.github.com/repos/andres-nav/minion/releases"

// cmdInstall installs gru from the directory of this executable: a build's
// gru/bin or an extracted release.
func cmdInstall(args []string) int {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	prefix := fs.String("prefix", filepath.Join(home, ".local"), "install into DIR/bin and DIR/libexec/gru")
	skills := fs.String("skill-dir", filepath.Join(home, ".claude", "skills"), "install the AI skill into DIR/gru, if DIR's parent exists")
	if _, err := parseArgs(fs, args, false); err != nil {
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		return errf("%v", err)
	}
	return exit(0, install(filepath.Dir(exe), *prefix, *skills))
}

// install puts src's gru in prefix/bin, its Linux agents and minion builds in
// prefix/libexec/gru, and SKILL.md in skills/gru if skills' parent (~/.claude)
// exists.
func install(src, prefix, skills string) error {
	if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil {
		return fmt.Errorf("no SKILL.md next to gru in %s: install from a release or from gru/bin after make build", src)
	}
	bin, lib := filepath.Join(prefix, "bin"), filepath.Join(prefix, "libexec", "gru")
	if err := put(filepath.Join(src, "gru"), filepath.Join(bin, "gru"), 0o755); err != nil {
		return err
	}
	agents, _ := filepath.Glob(filepath.Join(src, "gru-linux-*"))
	minions, _ := filepath.Glob(filepath.Join(src, "minion-linux-*"))
	for _, f := range append(agents, minions...) {
		if err := put(f, filepath.Join(lib, filepath.Base(f)), 0o755); err != nil {
			return err
		}
	}
	fmt.Printf("installed → %s (agents and minion in %s)\n", filepath.Join(bin, "gru"), lib)
	if _, err := os.Stat(filepath.Dir(skills)); err != nil {
		fmt.Printf("skipped skill (no %s)\n", filepath.Dir(skills))
	} else if err := put(filepath.Join(src, "SKILL.md"), filepath.Join(skills, "gru", "SKILL.md"), 0o644); err != nil {
		return err
	} else {
		fmt.Printf("skill → %s\n", filepath.Join(skills, "gru"))
	}
	if !slices.Contains(filepath.SplitList(os.Getenv("PATH")), bin) {
		fmt.Printf("add %s to your PATH\n", bin)
	}
	return nil
}

// put copies src to dst through a temp file and a rename: overwriting a binary
// in place invalidates its code signature on macOS, and the kernel kills it.
func put(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".gru-install-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, in)
	if err := errors.Join(err, f.Chmod(mode), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), dst)
}

func cmdUpgrade(args []string) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	want := fs.String("version", "", "the release to install, e.g. v0.2.0 (default: the latest)")
	if _, err := parseArgs(fs, args, false); err != nil {
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		return errf("%v", err)
	}
	return exit(0, upgrade(cmp.Or(os.Getenv("GRU_RELEASES"), releases), *want, exe))
}

// upgrade installs release want, or the latest, over exe (<prefix>/bin/gru):
// it checks this platform's archive against the release's checksums.txt and
// runs the new gru's install.
func upgrade(base, want, exe string) error {
	path := "/latest"
	if want != "" {
		path = "/tags/" + want
	}
	b, err := get(base + path)
	if err != nil {
		return err
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &rel); err != nil {
		return fmt.Errorf("%s%s: %w", base, path, err)
	}
	if rel.Tag == version {
		fmt.Printf("gru %s is up to date\n", version)
		return nil
	}
	name := "gru-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	got := map[string][]byte{}
	for _, a := range rel.Assets {
		if a.Name == name || a.Name == "checksums.txt" {
			if got[a.Name], err = get(a.URL); err != nil {
				return err
			}
		}
	}
	for _, n := range []string{name, "checksums.txt"} {
		if got[n] == nil {
			return fmt.Errorf("release %s has no %s", rel.Tag, n)
		}
	}
	sum := sha256.Sum256(got[name])
	if !slices.Contains(strings.Split(string(got["checksums.txt"]), "\n"), hex.EncodeToString(sum[:])+"  "+name) {
		return fmt.Errorf("%s doesn't match the release's checksums.txt", name)
	}
	dir, err := os.MkdirTemp("", "gru-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := untar(got[name], dir); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	fmt.Printf("gru %s -> %s\n", version, rel.Tag)
	cmd := exec.Command(filepath.Join(dir, "gru"), "install", "--prefix", filepath.Dir(filepath.Dir(exe)))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func get(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// untar writes the files of a flat .tar.gz into dir, and refuses any entry that
// isn't a regular file with a plain name.
func untar(archive []byte, dir string) error {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || !filepath.IsLocal(h.Name) || strings.Contains(h.Name, "/") {
			return fmt.Errorf("refused %q: not a plain file name", h.Name)
		}
		f, err := os.OpenFile(filepath.Join(dir, h.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, h.FileInfo().Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	}
}
