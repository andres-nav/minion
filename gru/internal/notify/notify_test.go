package notify

import "strings"

import "testing"

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

func TestBuildMessageLeavesDotStuffingToSendMail(t *testing.T) {
	// smtp.SendMail writes through textproto.DotWriter, which dot-stuffs on the
	// wire. buildMessage must NOT stuff leading dots itself, or they'd be doubled.
	msg := string(buildMessage("a@x", []string{"b@x"}, "subj", "normal\n.hidden command"))
	if !strings.Contains(msg, "\r\n.hidden command\r\n") {
		t.Errorf("leading dot should be left intact for SendMail to stuff:\n%q", msg)
	}
	if strings.Contains(msg, "..hidden command") {
		t.Errorf("buildMessage double-stuffs; DotWriter will stuff again:\n%q", msg)
	}
	if !strings.Contains(msg, "Subject: subj\r\n") || !strings.Contains(msg, "To: b@x\r\n") {
		t.Errorf("headers malformed:\n%q", msg)
	}
	// header/body separated by a blank CRLF line.
	if !strings.Contains(msg, "\r\n\r\nnormal\r\n") {
		t.Errorf("missing header/body separator:\n%q", msg)
	}
}

func TestSplitHostPortIPv6(t *testing.T) {
	host, port, err := splitHostPort("[::1]:587")
	if err != nil || host != "::1" || port != "587" {
		t.Errorf("splitHostPort(\"[::1]:587\") = %q, %q, %v", host, port, err)
	}
	if _, _, err := splitHostPort("hostonly"); err == nil {
		t.Error("expected error for address without a port")
	}
}

func TestEmailSendValidates(t *testing.T) {
	if err := (Email{From: "a@x", To: []string{"b@x"}}).Send("s", "b"); err == nil {
		t.Error("expected error when Addr is empty")
	}
	if err := (Email{Addr: "hostonly", From: "a@x", To: []string{"b@x"}}).Send("s", "b"); err == nil {
		t.Error("expected error when Addr lacks a port")
	}
}

func TestEmailLogin(t *testing.T) {
	if got := (Email{From: "a@x", Pass: "pw"}).login(); got != "a@x" {
		t.Errorf("password-only login = %q, want sender", got)
	}
	if got := (Email{From: "a@x", User: "u", Pass: "pw"}).login(); got != "u" {
		t.Errorf("explicit login = %q, want u", got)
	}
	if got := (Email{From: "a@x"}).login(); got != "" {
		t.Errorf("no-auth login = %q, want empty", got)
	}
}
