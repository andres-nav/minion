package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/andres-nav/minion/internal/config"
	"github.com/andres-nav/minion/internal/remote"
)

// buildNotify assembles the shell run at the end of a job's script. It resolves
// ${ENV} secrets on this machine (so credentials never touch the config file or
// the repo), gates on the configured `on`, invokes the agent for the email sink,
// and appends any raw `run` commands. invoke is the minion binary to call on the
// target; logPath is the job's log file (passed to the agent for a body tail).
func buildNotify(invoke, logPath string, n *config.Notify) string {
	if n.Email == nil && len(n.Run) == 0 {
		return ""
	}
	var b strings.Builder
	if n.Email != nil {
		if pass := resolveSecret(n.Email.Pass); pass != "" {
			fmt.Fprintf(&b, "export MINION_SMTP_PASS=%s\n", remote.ShellQuote(pass))
		}
	}
	fmt.Fprintf(&b, "if %s; then\n", gate(n.On))
	if n.Email != nil {
		fmt.Fprintf(&b, "  %s || echo '[minion] notify: email failed' >&2\n", emailInvoke(invoke, logPath, n.Email))
	}
	for _, c := range n.Run {
		fmt.Fprintf(&b, "  ( %s ) || echo '[minion] notify: run hook failed' >&2\n", c)
	}
	b.WriteString("fi")
	return b.String()
}

// emailInvoke is the agent call that sends the email. Non-secret fields are
// flags; the password is read from MINION_SMTP_PASS in the environment.
func emailInvoke(invoke, logPath string, e *config.EmailSink) string {
	parts := []string{invoke, "_agent", "notify",
		"--smtp", remote.ShellQuote(e.SMTP),
		"--from", remote.ShellQuote(e.From),
		"--to", remote.ShellQuote(e.To),
	}
	if e.User != "" {
		parts = append(parts, "--user", remote.ShellQuote(e.User))
	}
	if logPath != "" {
		parts = append(parts, "--log", logPath) // unquoted so the remote shell expands ~
	}
	return strings.Join(parts, " ")
}

// gate is the test expression deciding whether to notify, based on `on`.
func gate(on string) string {
	switch on {
	case "failure":
		return `[ "$MINION_EXIT" != 0 ]`
	case "success":
		return `[ "$MINION_EXIT" = 0 ]`
	default: // "always" or unset
		return "true"
	}
}

// resolveSecret expands a ${VAR} or $VAR reference from this machine's
// environment; any other value is returned literally.
func resolveSecret(s string) string {
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		return os.Getenv(s[2 : len(s)-1])
	}
	if strings.HasPrefix(s, "$") {
		return os.Getenv(s[1:])
	}
	return s
}

// cmdNotifyTest sends a synthetic notification through the resolved email config,
// from the target host, to verify credentials and connectivity end to end.
func cmdNotifyTest(args []string) int {
	fs := flag.NewFlagSet("notify-test", flag.ContinueOnError)
	host := fs.String("host", "", "host to send from (a hosts-map name, ssh alias, or localhost)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, r, err := resolveContext()
	if err != nil {
		return errf("%v", err)
	}
	n := cfg.NotifyFor(r.Template)
	if n == nil || n.Email == nil {
		return errf("no email notify configured (add a notify.email block to the template or global config)")
	}

	rt, err := dialRemote(cfg, r, *host)
	if err != nil {
		return errf("%v", err)
	}
	defer rt.close()

	invoke := notifyInvoke(rt)
	var b strings.Builder
	// synthetic job env so the message reads like a real (successful) run.
	b.WriteString("export MINION_JOB_ID=notify-test MINION_TASK=notify-test MINION_HOST=")
	b.WriteString(remote.ShellQuote(rt.alias))
	b.WriteString(" MINION_EXIT=0 MINION_DURATION=0\n")
	if pass := resolveSecret(n.Email.Pass); pass != "" {
		fmt.Fprintf(&b, "export MINION_SMTP_PASS=%s\n", remote.ShellQuote(pass))
	}
	b.WriteString(emailInvoke(invoke, "", n.Email))

	code, err := rt.exec.Run(b.String(), nil, os.Stdout, os.Stderr)
	if err != nil {
		return errf("notify-test: %v", err)
	}
	if code == 0 {
		fmt.Fprintf(os.Stderr, "sent test email to %s via %s\n", n.Email.To, rt.alias)
	}
	return code
}

// notifyInvoke returns the minion binary to call on the target: the uploaded
// agent for a remote, or this executable for a local run.
func notifyInvoke(rt *remoteTarget) string {
	if !rt.local && rt.invoke != "" {
		return rt.invoke
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "minion"
}
