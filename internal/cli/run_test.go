package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/andres-nav/minion/internal/remote"
)

// takenExec reports the first n launches as taken ids.
type takenExec struct {
	localExec
	n int
}

func (e *takenExec) Run(string, io.Reader, io.Writer, io.Writer) (int, error) {
	if e.n == 0 {
		return 0, nil
	}
	e.n--
	return remote.IDTaken, nil
}

func TestLaunchDrawsAnotherIDWhenTaken(t *testing.T) {
	var ids []string
	script := func(id, _ string) string { ids = append(ids, id); return "" }
	id, err := launch(&takenExec{n: 2}, "~/.minion", "20261123-120000", script)
	if err != nil || len(ids) != 3 || id != ids[2] {
		t.Fatalf("id %q err %v tries %v", id, err, ids)
	}
	if !strings.HasSuffix(id, "-20261123-120000") {
		t.Errorf("id %q lacks the time", id)
	}
	if _, err := launch(&takenExec{n: 5}, "~/.minion", "20261123-120000", script); err == nil {
		t.Error("want an error when every id is taken")
	}
}
