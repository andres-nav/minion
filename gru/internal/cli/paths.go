package cli

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tsLayout is a job's start time in its id and in {ts}: UTC, so names don't
// depend on the machine's time zone.
const tsLayout = "20060102T150405Z"

func stamp(t time.Time) string { return t.UTC().Format(tsLayout) }

// idTime is the start time in a job id (name-20261123T142501Z).
func idTime(id string) (time.Time, bool) {
	t, err := time.Parse(tsLayout, id[max(len(id)-len(tsLayout), 0):])
	return t, err == nil
}

// fetchDest resolves the local destination for fetched results, expanding
// {id}/{ts}/{repo} in tmpl. Empty tmpl → root; a relative path is under root.
func fetchDest(root, tmpl, id, repo string) string {
	if tmpl == "" {
		return root
	}
	ts := ""
	if t, ok := idTime(id); ok {
		ts = stamp(t)
	}
	p := expandLocalHome(strings.NewReplacer("{id}", id, "{ts}", ts, "{repo}", repo).Replace(tmpl))
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}

// expandLocalHome expands a leading ~ using the local home directory.
func expandLocalHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// remoteDirFor computes the remote project directory, expanding {repo}. A
// template's remote_dir (e.g. a workspace slot) takes precedence over the host's.
func remoteDirFor(tmplDir, hostDir, root string) string {
	rd := tmplDir
	if rd == "" {
		rd = hostDir
	}
	if rd == "" {
		rd = "gru/{repo}"
	}
	return strings.ReplaceAll(rd, "{repo}", filepath.Base(root))
}
