package notify

import (
	"strings"
	"testing"
)

func TestSubject(t *testing.T) {
	ok := Result{Task: "build", Host: "gpu", Exit: 0, Duration: 5}.Subject()
	if !strings.Contains(ok, "build on gpu ok (exit 0, 5s)") {
		t.Errorf("success subject = %q", ok)
	}
	fail := Result{Task: "test", Host: "gpu", Exit: 1, Duration: 75}.Subject()
	if !strings.Contains(fail, "FAILED (exit 1, 1m15s)") {
		t.Errorf("failure subject = %q", fail)
	}
}

func TestBodyIncludesLogTail(t *testing.T) {
	b := Result{Task: "build", Host: "gpu", LogTail: "line1\nline2\n"}.Body()
	for _, want := range []string{"--- log tail ---", "line1", "line2"} {
		if !strings.Contains(b, want) {
			t.Errorf("body missing %q:\n%s", want, b)
		}
	}
	// no log → no tail section.
	if strings.Contains(Result{Task: "build"}.Body(), "log tail") {
		t.Error("empty log tail should be omitted")
	}
}
