package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTailFile(t *testing.T) {
	dir := t.TempDir()

	// empty path and missing file → "".
	if got := tailFile("", 10); got != "" {
		t.Errorf("tailFile(\"\") = %q, want empty", got)
	}
	if got := tailFile(filepath.Join(dir, "nope"), 10); got != "" {
		t.Errorf("tailFile(missing) = %q, want empty", got)
	}

	// A log larger than maxTail: tailFile must return exactly the last n lines
	// (dropping the partial first line of the bounded read), not the whole file.
	big := filepath.Join(dir, "big.log")
	const total = 20000
	var b strings.Builder
	for i := 0; i < total; i++ {
		b.WriteString("line ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(big, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(big); fi.Size() <= 64<<10 {
		t.Fatalf("test file too small (%d bytes) to exercise the bounded read", fi.Size())
	}

	got := tailFile(big, 30)
	lines := strings.Split(got, "\n")
	if len(lines) != 30 {
		t.Fatalf("got %d lines, want 30:\n%s", len(lines), got)
	}
	if want := "line " + strconv.Itoa(total-1); lines[len(lines)-1] != want {
		t.Errorf("last line = %q, want %q", lines[len(lines)-1], want)
	}
	if want := "line " + strconv.Itoa(total-30); lines[0] != want {
		t.Errorf("first tailed line = %q, want %q", lines[0], want)
	}

	// A file with fewer than n lines → all of it, no trailing blank line.
	small := filepath.Join(dir, "small.log")
	if err := os.WriteFile(small, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := tailFile(small, 30); got != "a\nb\nc" {
		t.Errorf("small tail = %q, want %q", got, "a\nb\nc")
	}
}
