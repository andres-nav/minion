// Package sync implements minion's pure-Go, file-level sync: it walks a tree,
// compares file size+mtime to decide what changed, and streams only the changed
// files over an io stream (the ssh pipe). No external rsync is used.
package sync

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileMeta is the per-file state used to detect changes.
type FileMeta struct {
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"` // Unix seconds
	Mode  uint32 `json:"mode"`  // permission bits
}

// Manifest maps a slash-separated relative path to its metadata.
type Manifest map[string]FileMeta

// Walk builds a manifest of regular files under root, skipping .git and any
// path matching an exclude pattern. A missing root yields an empty manifest.
func Walk(root string, exclude []string) (Manifest, error) {
	m := Manifest{}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return m, nil
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if excluded(rel, exclude) {
				return filepath.SkipDir
			}
			return nil
		}
		if excluded(rel, exclude) {
			return nil
		}
		// os.Stat follows symlinks, so symlinked files are synced as files.
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		m[rel] = FileMeta{Size: info.Size(), Mtime: info.ModTime().Unix(), Mode: uint32(info.Mode().Perm())}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// WalkGit builds a manifest using `git ls-files` so .gitignore, global excludes,
// and .git/info/exclude are all respected. Untracked-but-not-ignored files are
// included. Falls back to Walk when root is not in a git repo.
func WalkGit(root string, exclude []string) (Manifest, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return Walk(root, exclude)
	}
	m := Manifest{}
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" || excluded(rel, exclude) {
			continue
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		m[rel] = FileMeta{Size: info.Size(), Mtime: info.ModTime().Unix(), Mode: uint32(info.Mode().Perm())}
	}
	return m, nil
}

// Deleted returns remote paths absent locally (for --delete mirroring).
func Deleted(local, remote Manifest) []string {
	var out []string
	for p := range remote {
		if _, ok := local[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// excluded reports whether rel should be skipped. .git is always skipped.
func excluded(rel string, patterns []string) bool {
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return true
	}
	base := path.Base(rel)
	for _, p := range patterns {
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			continue
		}
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		if ok, _ := path.Match(p, base); ok {
			return true
		}
	}
	return false
}

// Match returns the slash-relative paths of regular files under root selected by
// the patterns. A pattern is a glob ("results/*.json"); a matched directory is
// included recursively, so "build" or "results" fetches everything under it.
func Match(root string, patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(full string) {
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return
		}
		rel = filepath.ToSlash(rel)
		if !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pat)))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				continue
			}
			if fi.IsDir() {
				filepath.WalkDir(m, func(p string, d fs.DirEntry, werr error) error {
					if werr == nil && d.Type().IsRegular() {
						add(p)
					}
					return nil
				})
			} else if fi.Mode().IsRegular() {
				add(m)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Changed returns the local paths absent from remote or differing in size/mtime.
func Changed(local, remote Manifest) []string {
	var out []string
	for p, l := range local {
		if r, ok := remote[p]; !ok || r.Size != l.Size || r.Mtime != l.Mtime {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// WriteManifest walks root and writes its manifest as JSON to w.
func WriteManifest(w io.Writer, root string, exclude []string) error {
	m, err := Walk(root, exclude)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(m)
}

// DecodeManifest parses a JSON manifest.
func DecodeManifest(data []byte) (Manifest, error) {
	m := Manifest{}
	if len(bytes.TrimSpace(data)) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// frame is a newline-delimited JSON header preceding a file's raw bytes.
type frame struct {
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Mode  uint32 `json:"mode,omitempty"`
	Mtime int64  `json:"mtime,omitempty"`
}

// Send streams changed files (relative to root) followed by delete frames for
// deletes, ending with an "end" frame. Each file is a JSON header then its raw
// bytes.
func Send(w io.Writer, root string, paths, deletes []string) error {
	enc := json.NewEncoder(w)
	for _, rel := range paths {
		full := filepath.Join(root, filepath.FromSlash(rel))
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		info, err := f.Stat() // stat the open fd so size matches what we copy
		if err != nil {
			f.Close()
			return err
		}
		hdr := frame{Op: "file", Path: rel, Size: info.Size(), Mode: uint32(info.Mode().Perm()), Mtime: info.ModTime().Unix()}
		if err := enc.Encode(hdr); err != nil {
			f.Close()
			return err
		}
		if _, err := io.CopyN(w, f, info.Size()); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	for _, rel := range deletes {
		if err := enc.Encode(frame{Op: "delete", Path: rel}); err != nil {
			return err
		}
	}
	return enc.Encode(frame{Op: "end"})
}

// Receive reads a Send stream and writes files under destRoot, returning the
// number of files written.
func Receive(r io.Reader, destRoot string) (int, error) {
	br := bufio.NewReader(r)
	count := 0
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var hdr frame
			if e := json.Unmarshal(bytes.TrimSpace(line), &hdr); e != nil {
				return count, e
			}
			switch hdr.Op {
			case "end":
				return count, nil
			case "file":
				full, ok := safeJoin(destRoot, hdr.Path)
				if !ok {
					return count, fmt.Errorf("unsafe path %q", hdr.Path)
				}
				if e := writeFile(full, hdr, br); e != nil {
					return count, e
				}
				count++
			case "delete":
				if full, ok := safeJoin(destRoot, hdr.Path); ok {
					os.Remove(full)
				}
			default:
				return count, fmt.Errorf("unknown op %q", hdr.Op)
			}
		}
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, err
		}
	}
}

// safeJoin resolves rel under destRoot, rejecting paths that escape it.
func safeJoin(destRoot, rel string) (string, bool) {
	root := filepath.Clean(destRoot)
	full := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	if full == root || !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

func writeFile(full string, hdr frame, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp := full + ".minion.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, r, hdr.Size); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	mode := os.FileMode(hdr.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	os.Chmod(tmp, mode)
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return err
	}
	if hdr.Mtime != 0 {
		t := time.Unix(hdr.Mtime, 0)
		os.Chtimes(full, t, t)
	}
	return nil
}
