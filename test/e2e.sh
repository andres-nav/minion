#!/usr/bin/env bash
# End-to-end test of minion and energy-runner on this machine, with no ssh and no
# root: test/ssh runs the "remote" commands here, and the runner works on a fake
# sysfs and powercap tree. Everything lives in one temp dir, removed at the end.
# Run it with `make e2e`; E2E_KEEP=1 keeps the temp dir.
set -u
root=$(cd "$(dirname "$0")/.." && pwd)
for b in minion/bin/minion runner/target/release/energy-runner; do
  [ -x "$root/$b" ] || { echo "e2e: no $b, run make build" >&2; exit 2; }
done

T=$(mktemp -d)
export TZ=Europe/Zurich HOME=$T/home MINION_CONFIG=$T/config.json PATH=$T/bin:$PATH
mkdir -p "$T/bin" "$HOME" "$T/proj/work"
ln -s "$root/minion/bin/minion" "$root/runner/target/release/energy-runner" "$root/test/ssh" "$T/bin/"
printf '#!/bin/sh\necho "sudo $*" >> %s/sudo.log\nexit 1\n' "$T" > "$T/bin/sudo"
chmod +x "$T/bin/sudo"

cleanup() {
  for p in "$T"/state*/jobs/*/pid; do
    [ -e "${p%pid}status" ] || kill -- -"$(cat "$p")" 2> /dev/null
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

# The project: a queue task on the fake machine, restored by the always hook.
R="--sysfs $sys --state-dir $T/runner --lock $T/lock"
cat > "$MINION_CONFIG" << EOF
{
  "hosts": {
    "fake": {"remote_dir": "$T/remote/{repo}", "state_dir": "$T/state"},
    "localhost": {"state_dir": "$T/state-local"}
  },
  "notify": {"run": ["echo \"\$MINION_SUBJECT\" >> $T/notify.log"]}
}
EOF
cat > "$T/proj/.minion.json" << EOF
{
  "use": "energy",
  "templates": {"energy": {
    "tasks": {"energy": "energy-runner queue $R \$MINION_ARGS"},
    "always": ["energy-runner restore $R"],
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

# queue NAME REPS PROFILES WORKLOADS writes proj/NAME.json, which notifies through minion.
queue() {
  cat > "$T/proj/$1.json" << EOF
{"name": "$1", "reps": $2, "seed": 7, "settle_s": 0, "out": "results/energy",
 "profiles": {$3}, "workloads": {$4},
 "notify": {"cmd": ["sh", "-c", "\"\$MINION_AGENT\" _agent notify --config \"\$MINION_NOTIFY\" --subject \"\$RUNNER_EVENT \$RUNNER_QUEUE\" --body \"\$RUNNER_DETAIL\""]}}
EOF
}

fail() { echo "not ok: $*"; exit 1; }
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
# next_second: sleeps to the start of the next second.
next_second() { sleep "0.$(printf %03d $((999 - 10#$(date +%N) / 1000000)))"; }
# started OUT: the job id that `minion run` printed.
started() { sed -n 's/^started \(.*\) on .*/\1/p' "$1"; }

# queue_ok DIR RUNS FAILED: a finished queue's records, with RUNS runs, FAILED failed.
queue_ok() {
  local d=$1 r
  for f in queue.json plan.json events.jsonl; do [ -s "$d/$f" ] || fail "no $d/$f"; done
  [ "$(ls "$d/runs" | wc -l)" -eq "$2" ] || fail "$d: want $2 runs"
  for r in "$d"/runs/*; do
    has "$r/run.json" '"time":{'
    has "$r/run.json" '"rapl":{'
    has "$r/run.json" '"drift":false'
    case $r in
      *-ok-perf-*) grep -qx performance "$r/stdout" || fail "$r: not under its profile" ;;
      *-ok-base-*) grep -qx powersave "$r/stdout" || fail "$r: not under its profile" ;;
    esac
    case $r in *-ok-*) has "$r/run.json" '"joules":2.0' ;; esac
  done
  [ "$(grep -c '"event":"run_failed"' "$d/events.jsonl")" -eq "$3" ] || fail "$d: want $3 failed"
  grep -q '"event":"queue_done"' "$d/events.jsonl" || fail "$d: no queue_done"
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
  flock -n "$T/lock" true || fail "machine lock still held"
  [ ! -e "$T/sudo.log" ] || fail "called sudo: $(cat "$T/sudo.log")"
}

check_remote() {
  queue remote 2 "$BASE, $PERF" "$OK, $FAIL"
  minion run energy --host fake --arg remote.json > "$T/remote.out" 2>&1
  local code=$? id v dest
  cat "$T/remote.out"
  [ "$code" -eq 1 ] || fail "minion run: exit $code, want 1 (a workload fails)"
  id=$(started "$T/remote.out")
  v=$(minion version | cut -d' ' -f2)
  "$T"/state/bin/minion-"$v"-linux-* _agent version | grep -qx "minion $v" ||
    fail "no agent minion-$v-linux-* in $T/state/bin"
  dest=$(echo "$T"/fetched/*/"$id")
  queue_ok "$dest"/results/energy/remote-* 8 4
  has "$dest"/results/energy/remote-*/queue.json "\"MINION_JOB_ID\":\"$id\""
  has "$dest/_minion/run.json" '"exit":1'
  [ -s "$dest/_minion/log" ] || fail "no job log fetched"
  notified remote queue_started run_done run_failed queue_done
  machine_clean
}

check_localhost() {
  queue local 1 "$BASE, $PERF" "$OK, $FAIL"
  minion run energy --host localhost --arg local.json > "$T/local.out" 2>&1
  local code=$?
  cat "$T/local.out"
  [ "$code" -eq 1 ] || fail "minion run: exit $code, want 1 (a workload fails)"
  queue_ok results/energy/local-* 4 2
  has "$T/state-local/jobs/$(started "$T/local.out")/run.json" '"exit":1'
  notified local queue_started run_done run_failed queue_done
  machine_clean
}

check_detach() {
  queue slow 1 "$PERF" '"slow": {"cmd": ["sleep", "60"]}'
  local id code crash
  id=$(minion run energy --host fake --arg slow.json --detach) || fail "minion run --detach"
  wait_for 20 grep -qx performance "$gov"
  minion ps --host fake | tee "$T/ps.out" | grep "^$id " | grep -q running ||
    fail "minion ps doesn't show $id running"
  minion attach "$id" --host fake > "$T/attach.out" 2>&1 &
  wait_for 10 grep -q queue_started "$T/attach.out"
  minion kill "$id" --host fake || fail "minion kill"
  wait $!
  code=$?
  cat "$T/attach.out"
  [ "$code" -eq 143 ] || fail "minion attach: exit $code, want 143"
  grep -q '"status":"stopped"' "$T/attach.out" || fail "the queue didn't stop"
  machine_clean

  minion fetch "$id" --host fake || fail "minion fetch $id"
  exists "$T/fetched/*/$id/results/energy/slow-*/events.jsonl" || fail "fetch: no events.jsonl"
  exists "$T/fetched/*/$id/_minion/log" || fail "fetch: no job log"

  # A runner killed mid-run, without restoring: the always hook restores.
  queue crash 1 "$PERF" '"slow": {"cmd": ["sleep", "10"]}'
  crash=$(minion run energy --host fake --arg crash.json --detach) || fail "minion run --detach"
  wait_for 20 grep -qx performance "$gov"
  pkill -9 -f "energy-runner queue .*crash.json" || fail "no runner to kill"
  wait_for 20 test -e "$T/state/jobs/$crash/status"
  [ "$(cat "$T/state/jobs/$crash/status")" = 137 ] || fail "crashed job: want status 137"
  machine_clean

  queue next 1 "$PERF" "$OK"
  minion run energy --host fake --arg next.json || fail "the next queue failed"
  machine_clean
}

check_two() {
  queue first 1 "$PERF" '"hold": {"cmd": ["sleep", "3"]}'
  queue second 1 "$PERF" '"hold": {"cmd": ["sleep", "4"]}'
  queue third 1 "$PERF" "$OK"
  local a b c q
  a=$(minion run energy --host fake --arg first.json --detach) || fail "minion run first"
  wait_for 20 grep -q first- "$T/lock"
  b=$(minion run energy --host fake --arg second.json --detach) || fail "minion run second"
  # A waiting queue killed: its always hook runs while the first queue holds the lock.
  c=$(minion run energy --host fake --arg third.json --detach) || fail "minion run third"
  wait_for 20 grep -q "waiting for queue" "$T/state/jobs/$c/log"
  minion kill "$c" --host fake || fail "minion kill third"
  wait_for 20 gone "$c"
  grep -qx performance "$gov" || fail "the killed third queue's hook undid the first's profile"
  ! exists "$T/remote/proj/results/energy/third-*" || fail "the killed third queue left results"
  wait_for 30 test -e "$T/state/jobs/$a/status"
  wait_for 20 exists "$T/remote/proj/results/energy/second-*/runs/0001-*/stdout"
  # The first job's always hook, run late, must not undo the second queue's profile.
  energy-runner restore $R || fail "late restore"
  grep -qx performance "$gov" || fail "a late restore undid the second queue's profile"
  wait_for 30 test -e "$T/state/jobs/$b/status"
  cat "$T/state/jobs/$a/log" "$T/state/jobs/$b/log"
  [ "$(cat "$T/state/jobs/$a/status") $(cat "$T/state/jobs/$b/status")" = "0 0" ] ||
    fail "a queue failed"
  q=$(echo "$T"/remote/proj/results/energy/second-*)
  grep '"event":"queue_started"' "$q/events.jsonl" | grep -q waited ||
    fail "the second queue didn't wait for the lock"
  queue_ok "$q" 1 0
  machine_clean

  # The same queue twice in one second: both run, each into its own dir.
  queue twice 1 "$PERF" "$OK"
  next_second
  a=$(minion run energy --host fake --arg twice.json --detach) || fail "minion run twice"
  b=$(minion run energy --host fake --arg twice.json --detach) || fail "minion run twice"
  wait_for 30 test -e "$T/state/jobs/$a/status"
  wait_for 30 test -e "$T/state/jobs/$b/status"
  cat "$T/state/jobs/$a/log" "$T/state/jobs/$b/log"
  [ "$(cat "$T/state/jobs/$a/status") $(cat "$T/state/jobs/$b/status")" = "0 0" ] ||
    fail "the same queue twice: a queue failed"
  [ "$(ls -d "$T"/remote/proj/results/energy/twice-* | wc -l)" -eq 2 ] || fail "want 2 twice-* dirs"
  machine_clean
}

# Every name and readable time the checks above left is UTC: it ends in Z and is
# within 15 min of now.
check_utc() {
  local t s now bad=0
  minion ps --host fake --all > "$T/ps-fake.out"
  minion ps --host localhost --all > "$T/ps-local.out"
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
    if [ "${t%Z}" = "$t" ] || [ $((now - s)) -gt 900 ] || [ $((s - now)) -gt 60 ]; then
      echo "not UTC: $t"
      bad=1
    fi
  done
  [ "$bad" -eq 0 ] || fail "times not in UTC (TZ=$TZ)"
}

check_exit() {
  queue allok 1 "$BASE, $PERF" "$OK"
  minion run energy --host fake --arg allok.json || fail "a queue that succeeds: exit $?"
  queue failing 1 "$BASE, $PERF" "$OK, $FAIL"
  minion run energy --host fake --arg failing.json > "$T/failing.out" 2>&1
  local code=$?
  cat "$T/failing.out"
  [ "$code" -eq 1 ] || fail "a queue with a failing run: exit $code, want 1"
  queue_ok "$T"/remote/proj/results/energy/failing-* 4 2
  # A broken queue file is refused before anything runs, with a message.
  printf '{"name": ' > broken.json
  sed 's/"reps"/"bogus": 1, "reps"/' allok.json > unknown.json
  sed 's/"performance"/"warp"/' allok.json > badvalue.json
  for f in "broken.json:EOF" 'unknown.json:unknown key "bogus"' 'badvalue.json:"warp" is not allowed' \
    "missing.json:No such file"; do
    minion run energy --host fake --arg "${f%%:*}" > "$T/broken.out" 2>&1
    code=$?
    cat "$T/broken.out"
    [ "$code" -eq 1 ] || fail "${f%%:*}: exit $code, want 1"
    grep -qF -- "${f#*:}" "$T/broken.out" || fail "${f%%:*}: no clear message"
  done
  machine_clean
}

check_snapshot() {
  energy-runner snapshot > "$T/snapshot.json" || fail "energy-runner snapshot on /sys"
  cat "$T/snapshot.json"
  for k in cpu.governor cpu.turbo cpu.idle.disable; do has "$T/snapshot.json" "\"$k\":{"; done
  [ ! -d /sys/devices/system/cpu/cpu0/cpuidle/state0 ] || has "$T/snapshot.json" '"cpu0/state0":'
  energy-runner run --out "$T/run.json" -- true || fail "energy-runner run on /sys"
  has "$T/run.json" '"time":{'
}

failed=0
n=0
for c in "remote:remote path through test/ssh" "localhost:--host localhost" \
  "detach:detach, ps, attach, kill, crash" "two:queues sharing the machine lock" \
  "utc:every time in UTC (TZ=$TZ)" "exit:exit codes and broken queue files" \
  "snapshot:energy-runner snapshot and run on the real /sys"; do
  n=$((n + 1))
  start=$(date +%s)
  if (cd "$T/proj" && "check_${c%%:*}") > "$T/check.log" 2>&1; then
    echo "PASS $n ${c#*:} ($(($(date +%s) - start)) s)"
  else
    echo "FAIL $n ${c#*:}"
    tail -n 25 "$T/check.log" | sed 's/^/    /'
    failed=$((failed + 1))
  fi
done
echo "e2e: $((n - failed)) of $n checks passed"
[ "$failed" -eq 0 ]
