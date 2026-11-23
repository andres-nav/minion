# energy-runner

Sets machine settings for a measurement, puts the originals back (also after a crash), runs a
command between meter reads, and runs whole queues of them. Design: [ENERGY.md](../docs/ENERGY.md).
All times are UTC: names use `20261123T142501Z`, every wall-clock epoch has an ISO 8601 `*_utc`
twin (`2026-11-23T14:25:01Z`; monotonic times have none), and log lines start with the UTC time.
Local time is never used.

This first slice handles three settings, written in this order:

| setting | files | values |
|---|---|---|
| `cpu.governor` | `cpufreq/policy*/scaling_governor` | from `scaling_available_governors` |
| `cpu.turbo` | `intel_pstate/no_turbo` (skipped if absent) | `0`, `1` |
| `cpu.idle.disable` | `cpu*/cpuidle/state*/disable` | `0`, `1` |

## Usage

```sh
energy-runner snapshot                    # current settings as JSON
energy-runner snapshot > p.json           # edit p.json down to what should change, then:
sudo -n energy-runner apply < p.json      # save the originals, apply, read back
energy-runner status                      # what differs from the originals
sudo -n energy-runner restore             # put the originals back
```

A profile has the same shape as a snapshot and lists only the objects to change:

```json
{"cpu.governor": {"policy0": "performance"}, "cpu.idle.disable": {"cpu0/state2": "1"}}
```

Every value is checked before anything is written: unknown settings, objects or values are
refused. `apply` and `restore` read only stdin and `/sys`, and write only `/sys` and the journal in
`/var/lib/energy-runner`.

## Run

```sh
energy-runner run [--out run.json] -- <cmd...>
```

Runs the command as the user (refused as root) and reads the meters right before it starts and
right after it exits: no timer and no polling in between, the runner just waits in `waitpid`.
Meters are plugins (`time` always, `rapl` now). run.json holds the command, its exit code,
`meters.time` (at each edge `CLOCK_MONOTONIC` and `CLOCK_REALTIME` in ns, read back to back, the
realtime also as `*_utc`, and the duration in seconds from the monotonic clock), `meters.rapl` (per
zone: counters, joules and mean watts, corrected for one counter wrap), the settings snapshot
before and after, and `drift` if they differ. It is never overwritten. Zones whose `energy_uj`
can't be read (root-only by default) are listed as `unreadable`. A zone's joules are left out, with
a warning, when the run was long enough to wrap its counter twice at the zone's highest power
limit, or when the zone has no power limit.

## Queue

```sh
energy-runner queue q.json
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
  "notify": {"cmd": ["sh", "-c", "\"$MINION_AGENT\" _agent notify --config \"$MINION_NOTIFY\" --subject \"$RUNNER_EVENT $RUNNER_QUEUE\" --body \"$RUNNER_DETAIL\""]}
}
```

`seed` (default: from the clock), `settle_s` (default 1), `meters` (default `["time", "rapl"]`;
`time` is always read) and `notify` (with `timeout_s`, default 30) are optional. Unknown keys,
settings, objects, values and meters are refused before anything runs. Names take letters,
digits, `.`, `_` and `-`.

- **Plan.** `reps` rounds; each round runs every (profile, workload) pair once, in an order
  shuffled by Fisher-Yates with splitmix64 from the seed. The whole plan goes to plan.json first.
- **Profiles.** A profile lists only what it changes. Each one is resolved over the settings at
  the start: every object that any profile sets is held at this profile's value, or else at its
  start value, so a run never inherits the settings of the run before it. A profile is applied
  (`sudo -n energy-runner apply`, profile on stdin) only when the machine is not already in it.
- **Machine lock.** A blocking `flock` on `/run/lock/energy-runner.lock` (world-writable, gone at
  reboot), held to the end. The holder writes its queue id into the file; a second queue logs
  that id and sleeps in the kernel until the lock is free, then runs.
- **Runs.** First, a journal left open by a crash is restored (`sudo -n energy-runner restore
  --held`).
  Then each run: apply if needed, settle, run the workload with `run`'s meters (stdin from
  `/dev/null`, stdout and stderr to files, in its own process group). A run whose settings
  changed during it or differ from its profile is flagged `drift`, and the next run applies its
  profile again. A failing workload is recorded and the queue goes on.
- **End.** At the end, or on SIGTERM, SIGINT or SIGHUP (which also stop the running workload), the
  queue restores the settings, then releases the lock. It exits 0 when every run succeeded and
  the restore worked, 128 + the signal when stopped, and 1 otherwise.

`apply`, `restore` and `restore --held` need a sudoers rule that lets the user run them without a
password; sudo finds `energy-runner` on its `secure_path`, so install it there, owned by root.

`restore` first takes the machine lock without waiting. While a queue holds it, `restore` does
nothing and says so: that queue restored any leftover journal when it started, so a late restore
would only undo its profile. `--held` tells `restore` that the caller is the queue holding the
lock. The lock is opened with `O_NONBLOCK`, and anything but a regular file is refused, so a FIFO
put in its place can't block a restore.

Each queue writes to a new directory, `<out>/<name>-<YYYYMMDDTHHMMSSZ>/`:

```
queue.json    the queue as given; the resolved seed, settle, meters and profiles; the settings
              at the start; the queue's start on both clocks (`time`; its end is the
              queue_done event); the runner version; the MINION_* variables (job id, task, host)
plan.json     the seed and every run: id, round, position, workload, profile
events.jsonl  one line per event: event, time, time_utc, queue, run, status, detail
runs/0001-ep-stable-r01/
  run.json    as for `run` (cmd, exit, meters, state before and after, drift), plus the
              queue, run id, round, position, workload and profile
  stdout, stderr
```

### Events

Each event is appended to events.jsonl and logged. Then, and only between runs, `notify.cmd` runs
once with the event in `RUNNER_EVENT`, `RUNNER_QUEUE`, `RUNNER_RUN`, `RUNNER_STATUS` and
`RUNNER_DETAIL`. It runs in its own process group, which is killed if it still runs after
`notify.timeout_s`. A failing or killed notify command is logged and never changes the queue's
outcome.

| event | status | detail |
|---|---|---|
| `queue_started` | | the number of runs, and the wait for the lock |
| `run_done`, `run_failed` | `exit N`, `signal N` or `error` | seconds, or the error |
| `drift` | | the profile |
| `restore_ok`, `restore_failed` | `ok`, `failed` | leftover journal or end of queue; what was kept, or the error |
| `queue_done` | `ok`, `failed`, `stopped` | runs done and failed, and why it stopped |

### With minion

The queue runs as one detached minion job, so `minion ps`, `attach`, `kill` and the log apply to
it. In the project's `.minion.json` (or its template):

```json
{
  "tasks": {"energy": "energy-runner queue $MINION_ARGS"},
  "always": ["sudo -n energy-runner restore"]
}
```

```sh
minion run energy --arg q.json --detach
```

`minion kill` sends SIGTERM: the queue stops its workload and restores. If the runner dies
without restoring (`kill -9`, a crash), the `always` hook restores. If another queue was waiting
and took the lock first, that queue restores before its first run, and the hook does nothing.

## Journal

`journal.jsonl` in the state dir, one fsynced JSON line per step: `open`, `saved` (the full
original state, written once), `apply` (the intent), `applied` (the read-back), `restored`,
`close`. Each line has `time` (epoch seconds) and `time_utc`. On close it moves to
`done/<YYYYMMDDTHHMMSS.nnnnnnnnnZ>.jsonl`. `restore` puts back every saved value except
objects that differ from both the saved and the last applied value: someone else changed those, so
they are kept and reported. After a crash between `apply` and `applied`, every object is restored.
Restoring twice is harmless.

## Test

```sh
cargo test     # on a fake sysfs and powercap tree in a temp dir; one test reads the real /sys
cargo clippy -- -D warnings
```

`--sysfs DIR`, `--state-dir DIR` and `--lock FILE` point the runner at a test tree. They are
refused as root. With any of them, `queue` calls `apply` and `restore` in-process instead of
through sudo, so a whole queue runs without root.
