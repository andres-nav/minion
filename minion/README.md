# minion

Sets machine settings for a measurement, puts the originals back (also after a crash), runs a
command between meter reads, and runs whole queues of them. Design: [MINION.md](../docs/MINION.md).
All times are UTC: names use `20261123T142501Z`, every wall-clock epoch has an ISO 8601 `*_utc`
twin (`2026-11-23T14:25:01Z`; monotonic times have none), and log lines start with the UTC time.
Local time is never used.

This slice handles five settings, written in this order:

| setting | files | values |
|---|---|---|
| `cpu.governor` | `cpufreq/policy*/scaling_governor` | from `scaling_available_governors` |
| `cpu.epp` | `cpufreq/policy*/energy_performance_preference` | from `energy_performance_available_preferences`, but not `default` (never read back) |
| `cpu.turbo` | `intel_pstate/no_turbo` (skipped if absent) | `0`, `1` |
| `cpu.freq` | `cpufreq/policy*/scaling_{min,max}_freq`, as objects `policy0/min` and `policy0/max` | kHz within `cpuinfo_{min,max}_freq` |
| `cpu.idle.disable` | `cpu*/cpuidle/state*/disable` | `0`, `1` |

Per policy, `cpu.freq` keeps min ≤ max: a profile that would leave min above max is refused,
and min and max are written in the order that keeps min ≤ max at every step, also on restore
(cpufreq clamps a max below min but keeps the min request). With intel_pstate or amd-pstate in
active mode, the `performance` governor holds EPP at `performance` and refuses other values
(`EBUSY`): a profile that asks for another one there is refused, and a queue holds EPP at
`performance` for such a profile when another profile sets it. Turning turbo off clamps the max
frequency (intel_pstate), so a profile with `no_turbo` 1 fails its read-back check if another
profile sets a higher `cpu.freq` max: set `cpu.freq` in it too.

## Usage

With gru, run these on a host as `gru minion <verb> --host X`:

```sh
minion snapshot                    # current settings as JSON; edit it down to a profile
minion restore                     # put back what a dead queue left applied
minion queue q.json                # a whole queue (below); with gru, as a gru run task
```

`restore` does nothing while a queue holds the queue lock: that queue restores at its end.

A profile has the same shape as a snapshot and lists only the objects to change:

```json
{"cpu.governor": {"policy0": "performance"}, "cpu.idle.disable": {"cpu0/state2": "1"}}
```

Every value is checked before anything is written: unknown settings, objects or values are refused,
and so is what the kernel would refuse or override (above).

## Root

Only `minion serve` writes settings, as root: one per queue (or per `restore`), started as
`sudo -n /usr/local/lib/gru/<its own file name> serve`. `gru setup --host X` installs that root
copy of each build (gru names a build by its version, platform and hash) and a sudoers drop-in
that allows only `minion-* serve` there, also without a tty. serve reads nothing but its pipe:
lines `apply PROFILE` and `restore`, each answered by `ok TEXT` or `error TEXT`, after a first
line that says it runs. It writes only `/sys` and the journal in `/var/lib/minion` (0711: anyone
can see whether `journal.jsonl` exists, a journal is open, but not list or read it). It takes the
journal lock at its first request and holds it to its end, and when the pipe closes (its queue
ended or died, also by `kill -9`) it restores and exits. It blocks on the pipe between requests,
so it never wakes during a run, and it never kills anything. sudo runs in its own process group,
so a stop signal to the queue doesn't reach serve.

A queue that changes settings starts serve before it takes the queue lock, so without the setup
it stops before touching anything, with exit 77 and "minion needs root on <host>: run `gru setup
--host <host>` once in a terminal". A queue that changes nothing needs no root and calls no sudo,
unless a journal is open: then, after it takes the lock, it restores through serve first, or
without the setup stops with exit 77 and "a previous queue left settings applied on <host>: run
`gru minion restore --host <host>`".

## Run

```sh
minion run [--out run.json] -- <cmd...>
```

Runs the command as the user (refused as root) and reads the meters right before it starts and
right after it exits: no timer and no polling in between, the runner just waits in `waitpid`.
Meters are plugins (`time` always, `rapl` now) and nest: the start reads them last to first and
the end first to last, so `time` is innermost. run.json holds the command, its exit code,
`meters.time` (at each edge `CLOCK_MONOTONIC` and `CLOCK_REALTIME` in ns, read back to back, the
realtime also as `*_utc`, and the duration in seconds from the monotonic clock), `meters.rapl` (per
zone: counters, joules and mean watts, corrected for one counter wrap), the settings snapshot
before and after, and `drift` if they differ. It is created before the command starts and never
overwritten. Zones whose `energy_uj` can't be read (root-only by default) are listed as
`unreadable`. A zone's joules are left out, with a warning, when the run was long enough to wrap
its counter twice at the zone's highest power limit, or when the zone has no power limit. The
command stays in the runner's process group, so Ctrl-C reaches it, and what it leaves running is
left alone (a queue kills it).

`meters.noise`, always there, records what else ran: it is read just outside each edge, outside
the other meters. Per CPU, the change of each `/proc/interrupts` and `/proc/softirqs` row that
changed; the change of `ctxt` in `/proc/stat`; the CPU time (`cpu.stat` `usage_usec`) of each
cgroup beside the runner's: the other children of the root and of each cgroup above the runner's,
such as other users' slices (`cgroups_usec`); and at both edges the users with a logind session
and the load average.

## Queue

```sh
minion queue q.json
```

Runs a whole queue as the user, in the foreground, and puts the settings back at the end:

```json
{
  "name": "turbo-cost",
  "profiles": {
    "factory": {},
    "stable": {"cpu.governor": {"policy0": "performance"}, "cpu.turbo": {"no_turbo": "1"}}
  },
  "workloads": {"ep": {"cmd": ["bin/ep.C.x"]}, "idle": {"cmd": ["sleep", "60"]}},
  "reps": 30,
  "seed": 7,
  "settle_s": 1,
  "meters": ["time", "rapl"],
  "out": "results/energy",
  "notify": {"cmd": ["sh", "-c", "\"$GRU_AGENT\" _agent notify --config \"$GRU_NOTIFY\" --subject \"$MINION_EVENT $MINION_QUEUE\" --body \"$MINION_DETAIL\""]}
}
```

`seed` (default: from the clock), `settle_s` (default 1), `meters` (default `["time", "rapl"]`;
`time` is always read) and `notify` (with `timeout_s`, default 30) are optional. Unknown keys,
settings, objects, values and meters are refused before anything runs. Names take up to 64
letters, digits, `.`, `_` and `-`.

A `cmd` meter, `{"cmd": [...]}` in `meters` (with `timeout_s`, default 10), is a command that
prints `name number` lines, for numbers minion can't read itself. It runs at each edge outside the
other meters, like a workload (in its own process group, killed if the runner dies, marked for
reap), and its group is killed when it exits, or after `timeout_s`. run.json gets `meters.cmd`
with the `start` and `end` numbers and their `delta`. A bad line, a failure or a timeout fails
the run; at the start edge, before the workload starts.

- **Plan.** `reps` rounds; each round runs every (profile, workload) pair once, in an order
  shuffled by Fisher-Yates with splitmix64 from the seed. The whole plan goes to plan.json first.
- **Profiles.** A profile lists only what it changes. Each one is resolved over the settings at
  the start: every object that any profile sets is held at this profile's value, or else at its
  start value, so a run never inherits the settings of the run before it. A profile is applied
  (through serve) only when the machine is not already in it.
- **Queue lock.** A blocking `flock` on `/run/lock/minion.lock` (world-writable, gone at
  reboot), held to the end. A second queue logs the holder's id and sleeps in the kernel until
  the lock is free, then runs. Where `/run/lock` is root's and 0755 (Fedora, RHEL, Amazon Linux),
  root creates the file at boot, e.g. with the tmpfiles.d line
  `f /run/lock/minion.lock 0666 root root -`; until then a queue stops with that hint.
- **Start.** In this order: take the lock; choose the queue id (`<name>-<UTC time>`, or with `-2`,
  `-3`, ... if a queue of that name started in the same second) and write it into the lock; reap
  the leftover workloads of a queue that died; restore a leftover journal (through serve, every
  time).
- **Runs.** Each run: apply if needed, settle, run the workload with `run`'s meters (stdin from
  `/dev/null`, stdout and stderr to files, in its own process group, killed if the runner dies,
  and marked for reap). Right after the end meter read, whatever the workload left running in its
  group is killed (`leftovers` in run.json, and a warning). A run whose settings changed during it
  or differ from its profile is flagged `drift`, and the next run applies its profile again. A
  failing workload is recorded and the queue goes on.
- **Thermal wait.** With `"thermal": {"band_c": 2, "timeout_s": 600}` (those are the defaults),
  the queue reads every hwmon temperature (`class/hwmon/hwmon*/temp*_input`) before its first
  apply, as the baseline. After each settle it waits, checking once a second, until every
  temperature is at most `band_c` above its baseline, up to `timeout_s`; then the run starts
  anyway and `thermal_timeout` is sent. run.json gets `thermal`: the baseline, the temperatures
  at the start, the seconds waited and the sensors still too hot. A stop signal ends the wait.
  One baseline serves every profile, so a profile that idles hotter than `band_c` above the start
  waits for the timeout before each of its runs.
- **End.** At the end, or on SIGTERM, SIGINT or SIGHUP (which also stop the running workload or the
  settle), the queue restores the settings, then releases the lock. It exits 0 when every run
  succeeded and the restore worked, 128 + the signal when stopped, and 1 otherwise.

The queue lock is opened with `O_NONBLOCK` and `O_NOFOLLOW`, and anything but a regular file is
refused, so a FIFO or symlink put in its place can't block or redirect a queue.

`minion reap` (as the user; refused as root) kills the leftover workloads of a queue that
died: each workload has `MINION_WORKLOAD=<lock> <queue id>` in its environment, and so do
its children, so reap kills the user's processes that name this lock and a queue other than the
one holding it. Root never kills anything: the lock file is world-writable. A workload that
clears its environment escapes reap; the cgroup of the isolation phase will close that.

Each queue writes to a new directory, `<out>/<name>-<YYYYMMDDTHHMMSSZ>/`:

```
queue.json    the queue as given; the resolved seed, settle, meters, profiles and
              notify_timeout_s; the settings at the start; the queue's start on both clocks
              (`time`; its end is the queue_done event); the runner's version (gru's), lock
              and whether it used serve; the GRU_* variables (job id, task, host)
plan.json     the seed and every run: id, round, position, workload, profile
events.jsonl  one line per event: event, time, time_utc, queue, run, status, detail
runs/0001-ep-stable-r01/
  run.json    as for `run` (cmd, exit, meters, state before and after, drift), plus
              leftovers, the queue, run id, round, position, workload and profile
  stdout, stderr
```

### Events

Each event is appended to events.jsonl and logged. Then, and only between runs, `notify.cmd` runs
once with the event in `MINION_EVENT`, `MINION_QUEUE`, `MINION_RUN`, `MINION_STATUS` and
`MINION_DETAIL`. It runs like a workload (in its own process group, killed if the runner dies,
marked for reap), and its group is killed when it exits, or after `notify.timeout_s` if it still
runs. A failing or killed notify command is logged and never changes the queue's outcome.

| event | status | detail |
|---|---|---|
| `queue_started` | | the number of runs, and the wait for the lock |
| `run_done`, `run_failed` | `exit N`, `signal N` or `error` | seconds, or the error |
| `drift` | | the settings changed or differ from profile P |
| `thermal_timeout` | | the sensors still above the start + `band_c`, after how long |
| `restore_ok`, `restore_failed` | `ok`, `failed` | leftover journal or end of queue; what was kept, or the error |
| `queue_done` | `ok`, `failed`, `stopped` | runs done and failed, and why it stopped |

### With gru

The queue runs as one detached gru job, so `gru ps`, `attach`, `kill` and the log apply to
it. In the project's `.gru.json` (or its template):

```json
{
  "tasks": {"energy": "\"$GRU_MINION\" queue $GRU_ARGS"},
  "always": ["\"$GRU_MINION\" reap"]
}
```

```sh
gru setup --host X                  # once per host and build, in a terminal
gru run energy --arg q.json --detach
```

gru uploads minion with its agent and gives its path in `GRU_MINION`. `gru kill` sends SIGTERM:
the queue stops its workload and restores. If the runner dies without restoring (`kill -9`, a
crash), its workload and notify die with it, its serve restores, and the `always` hook reaps
their children. If another queue was waiting and took the lock first, that queue reaps before
its first run, and the hook leaves it alone.

## Journal

`journal.jsonl` in the state dir, one fsynced JSON line per step: `open`, `saved` (the full
original state, written once), `apply` (the intent), `applied` (the read-back), `restored`,
`close`. In `applied`, an object the profile doesn't set that someone else changed since the last
apply keeps its earlier value, so restore keeps theirs. Each line has `time` (epoch seconds) and
`time_utc`. On close it moves to `done/<YYYYMMDDTHHMMSS.nnnnnnnnnZ>.jsonl`. `restore` puts back
every saved value except objects that differ from both the saved and the last applied value:
someone else changed those, so they are kept and reported. After a crash between `apply` and
`applied`, every object is restored. Restoring twice is harmless.

## Test

```sh
cargo test     # on a fake sysfs and powercap tree in a temp dir; one test reads the real /sys
cargo clippy --all-targets -- -D warnings
```

`--sysfs DIR`, `--state-dir DIR`, `--lock FILE` and `--lib DIR` (where the root copies are)
point the runner at a test tree. They are refused as root, and serve gets them too. The unit
tests run serve on a thread; `make e2e` runs it through a sudo stand-in.
