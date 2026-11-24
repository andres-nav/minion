package cli

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/andres-nav/minion/gru/internal/remote"
)

// needsRoot is minion's exit code when it must change settings and `gru
// setup` hasn't let it.
const needsRoot = 77

// setupScript runs on the host as the user, with the uploaded minion as $1
// and install or remove as $2, and does root's part with one sudo password.
// Root reads only a copy in /tmp (the home may be NFS), checks its own copy
// against the hash in the name, and renames it to /usr/local/lib/gru/<name>;
// it adds the sudoers lines, which also let serve start without a tty, once
// visudo accepts them. Removing takes them away, and the copies with the last
// user's. GRU_TEST_DESTDIR, refused as root, prefixes both paths in tests.
const setupScript = `set -e
u=$(id -un)
case $u in *[!A-Za-z0-9._-]* | '') echo "gru: $u can't go in a sudoers line" >&2; exit 1 ;; esac
t=$(mktemp /tmp/gru-minion.XXXXXX)
trap 'rm -f "$t"' EXIT
[ "$2" = remove ] || cat "$1" > "$t"
if busctl get-property org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager \
  KillUserProcesses 2> /dev/null | grep -q true; then
  echo "gru: logind has KillUserProcesses=yes: detached jobs die with the ssh session (ask an admin for KillExcludeUsers=$u)" >&2
fi
sudo sh -c '
set -e
[ -z "$1" ] || [ "$(id -u)" != 0 ] || { echo "GRU_TEST_DESTDIR is refused as root" >&2; exit 1; }
lib=$1/usr/local/lib/gru d=$1/etc/sudoers.d
if [ "$5" = remove ]; then
  rm -f "$d/gru-minion-$2"
  ls "$d"/gru-minion-* > /dev/null 2>&1 || { rm -f "$lib"/minion-*; rmdir "$lib" 2> /dev/null || true; }
  exit
fi
umask 022
mkdir -p "$lib" "$d"
f=$(mktemp "$lib/.new.XXXXXX")
cat "$3" > "$f"
[ "$(sha256sum < "$f" | cut -c1-12)" = "${4##*-}" ] || { rm -f "$f"; echo "$4 changed on the way" >&2; exit 1; }
chmod 755 "$f"
mv -f "$f" "$lib/$4"
f=$(mktemp "$d/.gru-minion.XXXXXX")
a=GRU_MINION_$(echo "$2" | tr a-z.- A-Z__)
printf "Cmnd_Alias %s = %s serve\nDefaults!%s !requiretty\n%s ALL=(root) NOPASSWD: %s\n" \
  "$a" "$lib/minion-*" "$a" "$2" "$a" > "$f"
chmod 440 "$f"
visudo -cf "$f" > /dev/null 2>&1 || sed -i "/^Defaults/d" "$f" # sudo-rs: no requiretty, nor tty needed
visudo -cf "$f" > /dev/null || { rm -f "$f"; exit 1; }
mv -f "$f" "$d/gru-minion-$2"
' sh "${GRU_TEST_DESTDIR-}" "$u" "$t" "${1##*/}" "$2"
`

// cmdSetup lets minion do root's part on the host, or with --remove stops it.
func cmdSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	host := hostFlag(fs)
	remove := fs.Bool("remove", false, "remove minion's root copies and sudoers line")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !terminal() {
		return errf("gru setup asks for the sudo password: run it in a terminal")
	}
	t, err := connect(*host, true)
	if err != nil {
		return errf("%v", err)
	}
	defer t.Close()
	return setup(t, *remove)
}

func setup(t *target, remove bool) int {
	if t.minion == "" {
		return errf("gru has no minion for %s", t.alias)
	}
	mode := map[bool]string{false: "install", true: "remove"}[remove]
	return exit(t.Interactive("sh -c " + remote.ShellQuote(setupScript) + " sh " + t.minion + " " + mode))
}

// cmdMinion runs minion on the host with args, in the project's dir there.
func cmdMinion(args []string) int {
	host := ""
	if i := slices.Index(args, "--host"); i >= 0 && i+1 < len(args) {
		host = args[i+1]
		args = slices.Delete(args, i, i+2)
	}
	t, err := connect(host, true)
	if err != nil {
		return errf("%v", err)
	}
	defer t.Close()
	if t.minion == "" {
		return errf("gru has no minion for %s", t.alias)
	}
	cmd := fmt.Sprintf("cd %s 2>/dev/null; GRU_HOST=%s %s", remote.RemotePath(t.dir), remote.ShellQuote(t.alias), t.minion)
	for _, a := range args {
		cmd += " " + remote.ShellQuote(a)
	}
	return exit(t.Run(cmd, os.Stdin, os.Stdout, os.Stderr))
}

// terminal reports whether stdin is a terminal, as `test -t 0` says.
func terminal() bool {
	c := exec.Command("test", "-t", "0")
	c.Stdin = os.Stdin
	return c.Run() == nil
}

// yes asks prompt and reads a yes from stdin.
func yes(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt)
	a, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(a)), "y")
}
