package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// tsLayout formats a run's start time for {ts} in fetch_into.
const tsLayout = "20060102-150405"

// fetchDest resolves the local destination for fetched results, expanding
// {id}/{ts}/{repo} in tmpl. Empty tmpl → root; a relative path is under root.
// Using {id}/{ts} gives each run its own dir, so fetches never conflict.
func fetchDest(root, tmpl, id, ts, repo string) string {
	if tmpl == "" {
		return root
	}
	p := strings.NewReplacer("{id}", id, "{ts}", ts, "{repo}", repo).Replace(tmpl)
	p = expandLocalHome(p)
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

// remoteDirFor computes the remote destination directory, expanding {repo}. A
// template's remote_dir (e.g. a workspace slot) takes precedence over the host's.
func remoteDirFor(tmplDir, hostDir, root string) string {
	rd := tmplDir
	if rd == "" {
		rd = hostDir
	}
	if rd == "" {
		rd = "minion/{repo}"
	}
	return strings.ReplaceAll(rd, "{repo}", filepath.Base(root))
}
