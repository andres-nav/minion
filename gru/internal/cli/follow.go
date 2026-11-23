package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// jobStatus reads a job's recorded exit code; ok is false while it runs.
func jobStatus(dir string) (code int, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return 0, false
	}
	code, err = strconv.Atoi(strings.TrimSpace(string(b)))
	return code, err == nil
}

// onThisHost reports whether the job in dir was launched on this machine, so
// its pid means something here.
func onThisHost(dir string) bool {
	b, _ := os.ReadFile(filepath.Join(dir, "hostname"))
	h, err := os.Hostname()
	return err == nil && strings.TrimSpace(string(b)) == h
}

// result returns the job's recorded exit code, or why there is none.
func result(dir string) (int, error) {
	if code, ok := jobStatus(dir); ok {
		return code, nil
	}
	if !onThisHost(dir) {
		return 1, errors.New("the job runs on another host; attach from there")
	}
	return 1, errors.New("the job ended without recording a status")
}
