//go:build !linux

package cli

import (
	"io"
	"os"
	"path/filepath"
	"time"
)

// follow copies the job's log to w as it grows and returns the job's exit code
// once it is recorded. Without inotify and pidfd it checks once a second.
func follow(dir string, w io.Writer) (int, error) {
	log, err := os.Open(filepath.Join(dir, "log"))
	if err != nil {
		return 1, err
	}
	defer log.Close()
	for {
		_, done := jobStatus(dir)
		if _, err := io.Copy(w, log); err != nil {
			return 1, err
		}
		if done {
			return result(dir)
		}
		time.Sleep(time.Second)
	}
}
