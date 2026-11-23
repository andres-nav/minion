package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeJob makes a job dir of this host whose wrapper is script, run in the dir.
func fakeJob(t *testing.T, host, script string) (string, *exec.Cmd) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hostname"), []byte(host+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "log"), []byte("early\n"), 0o600)
	w := exec.Command("sh", "-c", script)
	w.Dir = dir
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(w.Process.Pid)), 0o600)
	t.Cleanup(func() { w.Process.Kill(); w.Wait() })
	return dir, w
}

func TestFollowStreamsUntilTheWrapperExits(t *testing.T) {
	h, _ := os.Hostname()
	// Output after the status (the notify step) still counts.
	dir, _ := fakeJob(t, h, "sleep 0.2; echo late >> log; echo 5 > status; sleep 0.1; echo after >> log")
	var out strings.Builder
	code, err := follow(dir, &out)
	if code != 5 || err != nil || out.String() != "early\nlate\nafter\n" {
		t.Fatalf("code %d err %v out %q", code, err, out.String())
	}
	// Finished: the log as is, and the status.
	out.Reset()
	if code, err := follow(dir, &out); code != 5 || err != nil || out.String() != "early\nlate\nafter\n" {
		t.Fatalf("finished: code %d err %v out %q", code, err, out.String())
	}
}

func TestFollowWithoutStatus(t *testing.T) {
	h, _ := os.Hostname()
	dir, w := fakeJob(t, h, "echo dying >> log; kill -9 $$")
	w.Wait()
	var out strings.Builder
	if _, err := follow(dir, &out); err == nil || !strings.Contains(err.Error(), "without") || out.String() != "early\ndying\n" {
		t.Fatalf("killed wrapper: err %v out %q", err, out.String())
	}
	// Another host's running job: its pid means nothing here.
	dir, _ = fakeJob(t, "elsewhere", "sleep 30")
	out.Reset()
	if _, err := follow(dir, &out); err == nil || !strings.Contains(err.Error(), "another host") || out.String() != "early\n" {
		t.Fatalf("other host: err %v out %q", err, out.String())
	}
}
