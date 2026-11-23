#!/usr/bin/env bash
# End-to-end test of gru and minion on this machine, with no ssh and no
# root: test/ssh runs the "remote" commands here, and the runner works on a fake
# sysfs and powercap tree. Everything lives in one temp dir, removed at the end.
# Run it with `make e2e`; E2E_KEEP=1 keeps the temp dir. Linux only.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
for b in gru/bin/gru minion/target/release/minion; do
  [ -x "$root/$b" ] || { echo "e2e: no $b, run make build" >&2; exit 2; }
done

T=$(mktemp -d)
export TZ=Europe/Zurich HOME=$T/home GRU_CONFIG=$T/config.json PATH=$T/bin:$PATH
[ "$(date +%z)" != +0000 ] || { echo "e2e: TZ=$TZ did not load (no tzdata?)" >&2; exit 2; }
# test/ssh runs "remote" commands with this home, so ~ differs on both sides.
export E2E_REMOTE_HOME=$T/rhome
mkdir -p "$T/bin" "$HOME" "$E2E_REMOTE_HOME" "$T/proj/work"
ln -s "$root/gru/bin/gru" "$root/minion/target/release/minion" "$root/test/ssh" "$T/bin/"
printf '#!/bin/sh\necho "sudo $*" >> %s/sudo.log\nexit 1\n' "$T" > "$T/bin/sudo"
chmod +x "$T/bin/sudo"

cleanup() {
  for p in "$T"/state*/jobs/*/pid; do
    [ -e "${p%pid}status" ] || kill -- -"$(cat "$p")" 2> /dev/null || true
  done
  if [ -n "${E2E_KEEP:-}" ]; then echo "kept $T"; else rm -rf "$T"; fi
}
trap cleanup EXIT

# The fake machine: two CPUs with a governor, turbo and idle states, one RAPL zone.
sys=$T/sys cpu=$T/sys/devices/system/cpu rapl=$T/sys/class/powercap/intel-rapl:0
gov=$cpu/cpufreq/policy0/scaling_governor
put() { mkdir -p "$(dirname "$1")" && printf %s "$2" > "$1"; }
for c in 0 1; do
  put "$cpu/cpufreq/policy$c/scaling_governor" powersave
  put "$cpu/cpufreq/policy$c/scaling_available_governors" "performance powersave"
  for s in 0 1 2; do put "$cpu/cpu$c/cpuidle/state$s/disable" 0; done
done
put "$cpu/intel_pstate/no_turbo" 0
put "$rapl/name" package-0
put "$rapl/energy_uj" 1000000
put "$rapl/max_energy_range_uj" 262143328850
put "$rapl/constraint_0_max_power_uw" 150000000
cp -r "$sys/devices" "$T/sys.orig"

# The project: a queue task on the fake machine, cleaned up by the always hooks.
R="--sysfs $sys --state-dir $T/runner --lock $T/lock"
cat > "$GRU_CONFIG" << EOF
{
  "hosts": {
    "fake": {"remote_dir": "$T/remote/{repo}", "state_dir": "$T/state"},
    "localhost": {"state_dir": "$T/state-local"}
  },
  "notify": {"run": ["echo \"\$GRU_SUBJECT\" >> $T/notify.log"]}
}
EOF
cat > "$T/proj/.gru.json" << EOF
{
  "use": "energy",
  "templates": {"energy": {
    "tasks": {"energy": "minion queue $R \$GRU_ARGS"},
    "always": ["minion reap $R", "minion restore $R"],
    "fetch": ["results"],
    "fetch_into": "$T/fetched/{ts}/{id}"
  }}
}
EOF
# ok prints the governor it runs under and uses 2 J of the fake RAPL zone.
cat > "$T/proj/work/ok.sh" << EOF
cat $gov
echo \$((\$(cat $rapl/energy_uj) + 2000000)) > $rapl/energy_uj
EOF
echo results/ > "$T/proj/.gitignore"
git -C "$T/proj" init -q && git -C "$T/proj" add -A &&
  git -C "$T/proj" -c user.name=e2e -c user.email=e2e@example.invalid commit -qm init

BASE='"base": {}'
PERF='"perf": {"cpu.governor": {"policy0": "performance", "policy1": "performance"},
  "cpu.turbo": {"no_turbo": "1"}, "cpu.idle.disable": {"cpu0/state2": "1"}}'
OK='"ok": {"cmd": ["sh", "work/ok.sh"]}'
FAIL='"fail": {"cmd": ["sh", "-c", "echo boom >&2; exit 3"]}'

# queue NAME REPS PROFILES WORKLOADS writes proj/NAME.json, which notifies through gru.
queue() {
  cat > "$T/proj/$1.json" << EOF
{"name": "$1", "reps": $2, "seed": 7, "settle_s": 0, "out": "results/energy",
 "profiles": {$3}, "workloads": {$4},
 "notify": {"cmd": ["sh", "-c", "\"\$GRU_AGENT\" _agent notify --config \"\$GRU_NOTIFY\" --subject \"\$MINION_EVENT \$MINION_QUEUE\" --body \"\$MINION_DETAIL\""]}}
EOF
}

fail() { echo "not ok: $*"; exit 1; }
# want CODE NAME CMD...: runs CMD into $T/NAME.out, shows it, and wants exit CODE.
want() {
  local w=$1 out=$T/$2.out c=0
  shift 2
  "$@" > "$out" 2>&1 || c=$?
  cat "$out"
  [ "$c" -eq "$w" ] || fail "$*: exit $c, want $w"
}
# has FILE TEXT: FILE, with spaces and newlines removed, contains TEXT.
has() { tr -d ' \n' < "$1" | grep -qF -- "$2" || fail "$1 lacks $2"; }
# wait_for SECONDS CMD...: polls CMD until it succeeds.
wait_for() {
  local n=$(($1 * 10))
  shift
  until "$@" 2> /dev/null; do
    n=$((n - 1))
    [ "$n" -gt 0 ] || fail "timed out waiting for: $*"
    sleep 0.1
  done
}
# exists GLOB: a file matches GLOB.
exists() { ls $1 > /dev/null 2>&1; }
# gone ID: job ID's wrapper has exited.
gone() { ! kill -0 "$(cat "$T/state/jobs/$1/pid")"; }
# started OUT: the job id that `gru run` printed.
started() { sed -n 's/^started \(.*\) on .*/\1/p' "$1"; }

# queue_ok DIR RUNS FAILED: a finished queue's records, with RUNS runs, FAILED failed.
queue_ok() {
  local d=$1 r
  for f in queue.json plan.json events.jsonl; do [ -s "$d/$f" ] || fail "no $d/$f"; done
  [ "$(ls "$d/runs" | wc -l)" -eq "$2" ] || fail "$d: want $2 runs"
  for r in "$d"/runs/*; do
    has "$r/run.json" '"time":{'
    has "$r/run.json" '"rapl":{'
    has "$r/run.json" '"noise":{"cgroups_usec":{'
    has "$r/run.json" '"drift":false'
    case $r in
      *-ok-perf-*) grep -qx performance "$r/stdout" || fail "$r: not under its profile" ;;
      *-ok-base-*) grep -qx powersave "$r/stdout" || fail "$r: not under its profile" ;;
    esac
    case $r in *-ok-*) has "$r/run.json" '"joules":2.0' ;; esac
  done
  [ "$(grep -c '"event":"run_failed"' "$d/events.jsonl")" -eq "$3" ] || fail "$d: want $3 failed"
  grep -q '"event":"queue_done"' "$d/events.jsonl" || fail "$d: no queue_done"
  grep '"event":"restore_ok"' "$d/events.jsonl" | grep -q "end of queue" ||
    fail "$d: the queue didn't restore at its end"
}

# notified QUEUE EVENT...: the notify run hook got each event of the queue.
notified() {
  local q=$1 e
  shift
  for e; do grep -q "^$e $q-" "$T/notify.log" || fail "no $e notification for $q"; done
}

# The fake machine is as it was: settings back, journal closed, lock free, no sudo.
machine_clean() {
  diff -r "$T/sys.orig" "$sys/devices" || fail "settings not restored"
  [ ! -e "$T/runner/journal.jsonl" ] || fail "journal still open"
  flock -n "$T/lock" true || fail "queue lock still held"
  [ ! -e "$T/sudo.log" ] || fail "called sudo: $(cat "$T/sudo.log")"
}

check_remote() {
  queue remote 2 "$BASE, $PERF" "$OK, $FAIL"
  gru run energy --host fake --arg remote.json > "$T/remote.out" 2>&1
  local code=$? id v dest
  cat "$T/remote.out"
  [ "$code" -eq 1 ] || fail "gru run: exit $code, want 1 (a workload fails)"
  id=$(started "$T/remote.out")
  v=$(gru version | cut -d' ' -f2)
  "$T"/state/bin/gru-"$v"-linux-* _agent version | grep -qx "gru $v" ||
    fail "no agent gru-$v-linux-* in $T/state/bin"
  dest=$(echo "$T"/fetched/*/"$id")
  queue_ok "$dest"/results/energy/remote-* 8 4
  has "$dest"/results/energy/remote-*/queue.json "\"GRU_JOB_ID\":\"$id\""
  has "$dest/_gru/run.json" '"exit":1'
  [ -s "$dest/_gru/log" ] || fail "no job log fetched"
  notified remote queue_started run_done run_failed queue_done
  machine_clean

  # A config typo, and a needs dir that would land on the project's own remote
  # dir, are refused before anything runs.
  mkdir -p "$T/dep" "$T/solo"
  echo '{"tasks": {"x": "true"}, "build": "make"}' > "$T/solo/.gru.json"
  (cd "$T/solo" && gru list) 2> "$T/solo.out" && fail "an unknown key was accepted"
  grep -q 'unknown field "build"' "$T/solo.out" || fail "unknown key: no clear message"
  echo "{\"tasks\": {\"x\": \"true\"}, \"host\": \"fake\", \"remote_dir\": \"$T/remote/solo\", \"needs\": [\"dep\"]}" > "$T/solo/.gru.json"
  (cd "$T/solo" && gru run x) 2> "$T/solo.out" && fail "needs into the project dir ran"
  grep -q "would sync into the project" "$T/solo.out" || fail "needs: no clear message"
}

check_localhost() {
  queue local 1 "$BASE, $PERF" "$OK, $FAIL"
  gru run energy --host localhost --arg local.json > "$T/local.out" 2>&1
  local code=$?
  cat "$T/local.out"
  [ "$code" -eq 1 ] || fail "gru run: exit $code, want 1 (a workload fails)"
  queue_ok results/energy/local-* 4 2
  has "$T/state-local/jobs/$(started "$T/local.out")/run.json" '"exit":1'
  notified local queue_started run_done run_failed queue_done
  machine_clean
  # A finished job can't be killed: its pid may be another process's now.
  gru kill "$(started "$T/local.out")" --host localhost 2> "$T/kill.out" && fail "killed a finished job"
  grep -q "has finished" "$T/kill.out" || fail "kill: no clear message"
  gru ssh --host localhost 2> "$T/ssh.out" && fail "ssh to this machine ran"
  grep -q "is this machine" "$T/ssh.out" || fail "ssh: no clear message"
}

check_detach() {
  queue slow 1 "$PERF" '"slow": {"cmd": ["sleep", "60"]}'
  local id code crash
  id=$(gru run energy --host fake --arg slow.json --detach) || fail "gru run --detach"
  wait_for 20 grep -qx performance "$gov"
  gru ps --host fake | tee "$T/ps.out" | grep "^$id " | grep -q running ||
    fail "gru ps doesn't show $id running"
  gru attach "$id" --host fake > "$T/attach.out" 2>&1 &
  wait_for 10 grep -q queue_started "$T/attach.out"
  gru kill "$id" --host fake || fail "gru kill"
  wait $!
  code=$?
  cat "$T/attach.out"
  [ "$code" -eq 143 ] || fail "gru attach: exit $code, want 143"
  grep -q '"status":"stopped"' "$T/attach.out" || fail "the queue didn't stop"
  machine_clean

  gru fetch "$id" --host fake || fail "gru fetch $id"
  exists "$T/fetched/*/$id/results/energy/slow-*/events.jsonl" || fail "fetch: no events.jsonl"
  exists "$T/fetched/*/$id/_gru/log" || fail "fetch: no job log"

  # A runner killed mid-run: its workload dies, and the always hooks reap and restore.
  # The sleeps are named after this run, so a parallel e2e can't be mistaken for it.
  local s=1$$
  queue crash 1 "$PERF" "\"slow\": {\"cmd\": [\"sh\", \"-c\", \"sleep ${s}1 & sleep ${s}2\"]}"
  crash=$(gru run energy --host fake --arg crash.json --detach) || fail "gru run --detach"
  wait_for 20 pgrep -x -f "sleep ${s}1"
  pkill -9 -f "minion queue $R crash.json" || fail "no runner to kill"
  wait_for 20 test -e "$T/state/jobs/$crash/status"
  [ "$(cat "$T/state/jobs/$crash/status")" = 137 ] || fail "crashed job: want status 137"
  ! pgrep -a -f "^sleep ${s}[12]$" || fail "the workload outlived its runner"
  machine_clean

  queue next 1 "$PERF" "$OK"
  gru run energy --host fake --arg next.json || fail "the next queue failed"
  machine_clean
}

check_two() {
  # Each holding workload waits for its go file, so the order doesn't depend on timing.
  queue first 1 "$PERF" "\"hold\": {\"cmd\": [\"sh\", \"-c\", \"until [ -e $T/go1 ]; do sleep 0.1; done\"]}"
  queue second 1 "$PERF" "\"hold\": {\"cmd\": [\"sh\", \"-c\", \"until [ -e $T/go2 ]; do sleep 0.1; done\"]}"
  queue third 1 "$PERF" "$OK"
  local a b c q
  a=$(gru run energy --host fake --arg first.json --detach) || fail "gru run first"
  wait_for 20 grep -q first- "$T/lock"
  b=$(gru run energy --host fake --arg second.json --detach) || fail "gru run second"
  # A waiting queue killed: its always hook runs while the first queue holds the lock.
  c=$(gru run energy --host fake --arg third.json --detach) || fail "gru run third"
  wait_for 20 grep -q "waiting for queue" "$T/state/jobs/$c/log"
  gru kill "$c" --host fake || fail "gru kill third"
  wait_for 20 gone "$c"
  grep -qx performance "$gov" || fail "the killed third queue's hook undid the first's profile"
  ! exists "$T/remote/proj/results/energy/third-*" || fail "the killed third queue left results"
  touch "$T/go1"
  wait_for 30 exists "$T/remote/proj/results/energy/second-*/runs/0001-*/stdout"
  # The first job's always hook, run late, must not undo the second queue's profile.
  minion restore $R || fail "late restore"
  grep -qx performance "$gov" || fail "a late restore undid the second queue's profile"
  touch "$T/go2"
  wait_for 30 test -e "$T/state/jobs/$a/status"
  wait_for 30 test -e "$T/state/jobs/$b/status"
  cat "$T/state/jobs/$a/log" "$T/state/jobs/$b/log"
  [ "$(cat "$T/state/jobs/$a/status") $(cat "$T/state/jobs/$b/status")" = "0 0" ] ||
    fail "a queue failed"
  q=$(echo "$T"/remote/proj/results/energy/second-*)
  grep '"event":"queue_started"' "$q/events.jsonl" | grep -q waited ||
    fail "the second queue didn't wait for the lock"
  queue_ok "$q" 1 0
  machine_clean
}

# gru's commands and options on a small project that sets every template key.
check_cli() {
  local p=$T/cli rd=$E2E_REMOTE_HOME/gru/cli id v h
  mkdir -p "$p/src" "$T/clidep" "$HOME/creds"
  echo one > "$p/src/a.txt"
  echo junk > "$p/skip.tmp"
  echo out/ > "$p/.gitignore"
  echo dep > "$T/clidep/dep.txt"
  echo tok > "$HOME/creds/token"
  cat > "$p/.gru.json" << EOF
{
  "use": "cli",
  "hosts": {
    "fake2": {"ssh": "fake2-alias", "state_dir": "$T/state"},
    "homevar": {"remote_dir": "\$HOME/hv/{repo}"},
    "unsetvar": {"remote_dir": "/tmp/\$E2E_UNSET/{repo}"},
    "down": {"ssh": "down.invalid"}
  },
  "templates": {"cli": {
    "host": "fake2",
    "env": ["GREETING=hello world"],
    "exclude": ["*.tmp"],
    "needs": ["clidep"],
    "push": ["~/creds"],
    "before": ["echo before \$GREETING"],
    "on_success": ["echo success"],
    "on_failure": ["echo failure \$GRU_EXIT \$GRU_DURATION"],
    "always": ["echo always \$GRU_STATUS"],
    "fetch": ["out"],
    "fetch_into": "$T/cfetched/{id}",
    "notify": {"on": "failure", "run": ["echo \"\$GRU_SUBJECT \$TOKEN\" >> $T/cli-notify.log"],
      "env": {"TOKEN": "\${E2E_TOKEN}"}},
    "tasks": {
      "hello": "mkdir -p out && echo \"\$GREETING \$GRU_ARGS\" | tee out/hello.txt",
      "fail": "sleep 1; exit 4",
      "show": "cat src/a.txt"
    }
  }}
}
EOF
  git -C "$p" init -q && git -C "$p" add -A &&
    git -C "$p" -c user.name=e2e -c user.email=e2e@example.invalid commit -qm init
  cd "$p"
  export E2E_TOKEN=tok123

  gru list | tee "$T/list.out"
  for w in "template: cli" "host:     fake2" "remote:   gru/cli" "hello" "show"; do
    grep -qF "$w" "$T/list.out" || fail "list lacks $w"
  done
  gru list --json | grep -q '"template": "cli"' || fail "list --json"

  # A run syncs (exclude, needs, push), runs the hooks with env and --arg, fetches.
  want 0 hello gru run hello --arg "a b"
  for w in "before hello world" "hello world a b" success "always ok"; do
    grep -qx "$w" "$T/hello.out" || fail "run hello: no $w"
  done
  id=$(started "$T/hello.out")
  [ -s "$rd/src/a.txt" ] && [ ! -e "$rd/skip.tmp" ] || fail "sync: a.txt not sent, or skip.tmp sent"
  [ -s "$E2E_REMOTE_HOME/gru/clidep/dep.txt" ] || fail "needs: clidep not synced"
  [ -s "$E2E_REMOTE_HOME/creds/token" ] || fail "push: ~/creds not copied"
  grep -qx "hello world a b" "$T/cfetched/$id/out/hello.txt" || fail "fetch_into: no out/hello.txt"
  has "$T/cfetched/$id/_gru/run.json" '"exit":0'
  [ ! -e "$T/cli-notify.log" ] || fail "notify on: failure fired for a success"
  gru fetch "$id" --into "$T/into" || fail "gru fetch --into"
  [ -s "$T/into/out/hello.txt" ] && [ -s "$T/into/_gru/run.json" ] || fail "fetch --into: files missing"

  want 4 fail gru run fail
  grep -Eqx "failure 4 [12]" "$T/fail.out" && grep -qx "always fail" "$T/fail.out" ||
    fail "run fail: hooks, or GRU_DURATION not the job's seconds"
  grep -q "FAILED (exit 4.* tok123$" "$T/cli-notify.log" || fail "notify: no failure message with the token"
  want 1 nope gru run nope
  grep -q nope "$T/nope.out" || fail "unknown task: no message"
  want 2 none gru run
  mkdir -p "$T/notasks" && git -C "$T/notasks" init -q
  (cd "$T/notasks" && gru list) | grep -q "^no tasks for $T/notasks" || fail "list: no tasks message"
  (cd "$T/notasks" && gru run x 2>&1) | grep -q "no tasks for $T/notasks" || fail "run: no tasks message"

  # --no-sync runs the code already there; sync sends it; --delete removes stale files.
  # (Sync compares size and mtime in seconds: a same-size edit in the same second is missed.)
  echo second > src/a.txt
  want 0 show gru run show --no-sync
  grep -qx one "$T/show.out" || fail "--no-sync synced"
  # local is localhost: the job runs here, in the project dir, with nothing to sync or fetch.
  want 0 here gru run show --host local --no-sync
  grep -qx second "$T/here.out" || fail "--host local: not run in the project dir"
  gru sync --host local | grep -q "nothing to sync" || fail "sync --host local"
  want 1 fetchlocal gru fetch --host localhost
  grep -q "fetch is for remote hosts" "$T/fetchlocal.out" || fail "fetch --host localhost: no clear message"
  gru sync || fail "gru sync"
  grep -qx second "$rd/src/a.txt" || fail "sync: a.txt not updated"
  echo stale > "$rd/stale.txt"
  echo keep > "$rd/keep.tmp"
  gru sync --delete || fail "gru sync --delete"
  [ ! -e "$rd/stale.txt" ] && [ -e "$rd/keep.tmp" ] || fail "sync --delete: stale kept, or excluded removed"
  gru sync --host homevar && [ -s "$E2E_REMOTE_HOME/hv/cli/src/a.txt" ] || fail "remote_dir with \$HOME"
  exists "$E2E_REMOTE_HOME/.gru/bin/gru-*" || fail "no agent in the default state dir ~/.gru"
  gru sync --host unsetvar 2> "$T/unset.out" && fail "remote_dir with an unset variable synced"
  grep -q E2E_UNSET "$T/unset.out" || fail "unset variable: no clear message"
  gru sync --host rawalias || fail "a host that is only an ssh alias"

  # Without an id, fetch and attach take this project's latest job; ps shows its jobs.
  want 0 hello2 gru run hello
  id=$(started "$T/hello2.out")
  rm -rf "$T/cfetched"
  gru fetch || fail "gru fetch (latest)"
  [ -s "$T/cfetched/$id/out/hello.txt" ] || fail "fetch: not into the latest job's dir"
  want 0 attach gru attach
  grep -q "hello world" "$T/attach.out" || fail "attach (latest): no log"
  gru ps > "$T/cli-ps.out"
  cat "$T/cli-ps.out"
  grep -q "^$id .*hello .*exit 0" "$T/cli-ps.out" || fail "ps: no $id"
  ! grep -q energy "$T/cli-ps.out" || fail "ps: another project's jobs"
  gru ps --all | grep -q energy || fail "ps --all: no other project's jobs"
  want 2 kill gru kill
  want 1 nojobs gru attach --host homevar
  grep -q "no jobs of this project on homevar" "$T/nojobs.out" || fail "attach with no jobs: no clear message"

  # doctor, ssh, version and help.
  want 1 doctor gru doctor
  grep -q "^host fake2 .*OK" "$T/doctor.out" || fail "doctor: fake2 not OK"
  grep -q "^host down .*FAIL .*down.invalid" "$T/doctor.out" || fail "doctor: down not FAIL"
  grep -q "^1 problem" "$T/doctor.out" || fail "doctor: not 1 problem"
  gru doctor --notify --host fake2 || fail "doctor --notify"
  grep -q "notify-test on fake2-alias ok.* tok123$" "$T/cli-notify.log" || fail "doctor --notify: no message"
  echo pwd | gru ssh > "$T/ssh.out" || fail "gru ssh"
  grep -qx "$rd" "$T/ssh.out" || fail "ssh: not in the project dir"
  v=$(gru version)
  [ "$(gru --version)" = "$v" ] && [ "$(gru -v)" = "$v" ] || fail "version"
  for h in help -h --help; do gru $h | grep -q "^usage: gru" || fail "gru $h"; done
  want 2 noargs gru
  grep -q "usage: gru" "$T/noargs.out" || fail "no args: no usage"
  want 2 unknown gru bogus

  # The config from $XDG_CONFIG_HOME/gru when GRU_CONFIG is unset, found by detect.
  mkdir -p "$T/xdg/gru" "$T/xdgproj"
  touch "$T/xdgproj/xdg.marker"
  echo '{"templates": {"x": {"detect": ["xdg.marker"], "tasks": {"t": "true"}}}}' > "$T/xdg/gru/config.json"
  cd "$T/xdgproj"
  env -u GRU_CONFIG XDG_CONFIG_HOME="$T/xdg" gru list | grep -q "template: x" || fail "XDG_CONFIG_HOME not used"
  env -u GRU_CONFIG XDG_CONFIG_HOME="$T/xdg" gru sync 2> "$T/nohost.out" && fail "synced with no host"
  grep -q "no host configured" "$T/nohost.out" || fail "no host: no clear message"
  # A template whose host is this machine lists the project dir as where tasks run.
  echo '{"host": "localhost", "tasks": {"t": "true"}}' > .gru.json
  gru list --json | grep -q "\"remote_dir\": \"$T/xdgproj\"" || fail "list: a local host's dir is not the project"
}

# Every name and readable time the checks above left is UTC: it ends in Z and is
# within 15 min of now.
check_utc() {
  local t s now bad=0 n=0
  gru ps --host fake --all > "$T/ps-fake.out"
  gru ps --host localhost --all > "$T/ps-local.out"
  cat "$T"/ps-*.out
  now=$(date +%s)
  for t in $({
    ls "$T"/state*/jobs "$T"/fetched "$T"/runner/done "$T"/remote/proj/results/energy \
      "$T"/proj/results/energy
    cat "$T"/ps-*.out "$T"/state*/jobs/*/log
    find "$T"/remote/proj/results "$T"/proj/results -name '*.json*' -exec cat {} +
  } | grep -oE '[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}(:[0-9]{2})?(\.[0-9]+)?Z?|[0-9]{8}[T-][0-9]{6}(\.[0-9]+)?Z?' |
    tr ' ' _ | sort -u); do
    s=$(echo "$t" | sed -E 's/^(....)-?(..)-?(..)[T_-]?(..):?(..).*/\1-\2-\3 \4:\5/')
    s=$(date -u -d "$s" +%s 2> /dev/null || echo 0)
    n=$((n + 1))
    if [ "${t%Z}" = "$t" ] || [ $((now - s)) -gt 900 ] || [ $((s - now)) -gt 60 ]; then
      echo "not UTC: $t"
      bad=1
    fi
  done
  [ "$bad" -eq 0 ] || fail "times not in UTC (TZ=$TZ)"
  [ "$n" -ge 10 ] || fail "only $n times found"
}

# A broken queue file is refused before anything runs, with a message.
check_exit() {
  queue unknown 1 "$PERF" "$OK"
  sed -i 's/"reps"/"bogus": 1, "reps"/' unknown.json
  queue badvalue 1 "$PERF" "$OK"
  sed -i 's/"performance"/"warp"/' badvalue.json
  printf '{"name": ' > broken.json
  for f in 'broken.json:broken.json: EOF while parsing' 'unknown.json:unknown key "bogus"' \
    'badvalue.json:"warp" is not allowed' 'missing.json:missing.json: No such file'; do
    want 1 broken gru run energy --host fake --arg "${f%%:*}"
    grep -qF -- "${f#*:}" "$T/broken.out" || fail "${f%%:*}: no clear message"
  done
  ! exists "$T/remote/proj/results/energy/unknown-*" || fail "a refused queue ran"
  ! exists "$T/remote/proj/results/energy/badvalue-*" || fail "a refused queue ran"
  machine_clean

  # The privileged verbs by hand, with the profile and the queue id on stdin.
  echo '{"cpu.governor": {"policy0": "performance"}}' | minion apply $R || fail "minion apply"
  grep -qx performance "$gov" || fail "apply: governor not set"
  minion status $R | tr -d ' \n' | grep -q '"policy0":"performance"' || fail "status: no change shown"
  echo '{"queue": "nope"}' | minion restore --held $R 2> "$T/held.out" && fail "restore --held ran for another queue"
  grep -q "doesn't hold the queue lock" "$T/held.out" || fail "restore --held: no clear message"
  minion restore $R || fail "minion restore"
  machine_clean
}

check_snapshot() {
  minion snapshot > "$T/snapshot.json" || fail "minion snapshot on /sys"
  cat "$T/snapshot.json"
  for k in cpu.governor cpu.turbo cpu.idle.disable; do has "$T/snapshot.json" "\"$k\":{"; done
  [ ! -d /sys/devices/system/cpu/cpu0/cpuidle/state0 ] || has "$T/snapshot.json" '"cpu0/state0":'
  minion run --out "$T/run.json" -- true || fail "minion run on /sys"
  has "$T/run.json" '"time":{'
}

failed=0
for c in "remote:remote path (fake ssh)" "localhost:--host localhost" \
  "detach:detach, ps, attach, kill and a crashed runner" "two:two queues and a late restore" \
  "cli:every gru option" "utc:every time in UTC (TZ=$TZ)" \
  "exit:exit codes, broken queue files, privileged verbs" "snapshot:snapshot and run on the real /sys"; do
  SECONDS=0
  if (cd "$T/proj" && "check_${c%%:*}") > "$T/${c%%:*}.log" 2>&1; then
    echo "PASS ${c#*:} ($SECONDS s)"
  else
    echo "FAIL ${c#*:}"
    tail -n 25 "$T/${c%%:*}.log" | sed 's/^/    /'
    failed=$((failed + 1))
  fi
done
echo "e2e: $failed failed"
[ "$failed" -eq 0 ]
