# minion — design

**Status:** the state engine, `run` and `queue` are in `minion/`; the rest is proposed. How minion
works with gru: [DESIGN.md](DESIGN.md). Sources: [RESEARCH.md](RESEARCH.md).

## Summary

Measurements, energy above all, depend on the machine's state (governor, turbo, idle states, uncore,
SMT, interrupts, temperature, other users) as much as on the code under test, and today that state
is set by hand with scripts that are neither recorded nor undone. This design adds a small runner,
**minion**, on the measured host, run as one detached gru job (no service). It records every
setting, applies a declared profile and checks it, runs a queue in randomized interleaved order,
reads its meters only at run edges, stores a noise record with every result, notifies between runs,
and puts every setting back to its **original** value, also after a crash. Energy is the first use
case: workloads are any command and meters are plugins. The goals follow the project task
description (M. Friedman, CORES Lab, ETH Zurich, 2026). GEOPM Service, Slurm and tuned also restore
originals, but only from memory or `/run`; an on-disk journal is new.

## Goals and non-goals

Goals: declarative, reversible state (the originals come back after `kill -9` or a crash; after a
reboot once the boot unit exists); repeatable runs (isolation, randomized interleaved order with a
recorded seed, a thermal gate, refusal when boot-only settings don't match); negligible overhead;
every result carrying its state, noise record, commit, config, seed, hardware and job id; any
command as a workload; enough data for the three [research questions](#research-questions).

Non-goals: a cluster scheduler (several machines only inside one experiment), attributing energy
to tenants or processes, a benchmark framework (workloads stay the user's commands), dashboards,
anything but Linux.

## The rule: stay out of the measured window

A *measured window* runs from the counter reads at the start of a run to those at its end. Inside it
the runner does not log, write, send or fork. It sleeps in one `poll()` on a `signalfd` for
`SIGCHLD` and a mark FIFO (opened `O_RDWR`, so a closed writer can't make it spin), on a
housekeeping CPU, with a large timer slack and no real-time priority. The only exception is the slow
counter-wrap timer. The runner is a child subreaper (`PR_SET_CHILD_SUBREAPER`): orphans come back to
it, so the window ends when no workload process is left, and children (MPI ranks, `cmd &`) count.
Helpers (`notify.cmd`, the `cmd` meter, `sudo` verbs) run only between runs, in their own process
group, which is killed before a window starts; a leader's `hold` sessions are not counted. Leftovers
after a grace period, or a run past its `timeout_s`, are killed and the run is flagged. The
workload's first process gets `PR_SET_PDEATHSIG(SIGKILL)`. From the isolation phase on, the workload
also has its own cgroup, which `cgroup.kill` ends.

Before `exec`, the child resets its timer slack to the default 50 µs (slack is inherited) and its
signal mask. Before the start read come the noise snapshot, then `sync` (so writes from between
runs are not flushed inside the window), then a short recorded settle.

## Components

| Part | Where | Language | Does |
|---|---|---|---|
| minion | host, as the user | Rust | `queue`, `run`, `hold`, meters, noise record, events; `snapshot`, `status`, `mark` |
| privileged verbs | the same binary, as `sudo -n minion <verb>` (one sudoers rule) | Rust | the only writer of settings; own the journal |
| wall sampler | another machine | the minion binary | reads the BMC over the network during the queue |

- **A gru job hosts the queue; no systemd units for now** ([how](DESIGN.md#how-they-connect)).
  Keep the task one command, so the shell execs the runner and the wrapper's hooks run only after
  it exits. Without gru, the runner works the same under `setsid` or tmux, minus the hooks. The
  runner and `hold` treat `SIGTERM`, `SIGINT` and `SIGHUP` alike. setsid stays in the login
  session's cgroup: with logind's `KillUserProcesses=yes` (upstream default; Ubuntu builds `no`),
  the job dies when gru's launch connection closes, so such a host needs `KillExcludeUsers=` for
  the user. Check each host.
- **Privileged verbs live in the runner binary for now**, with an allowlist like `kvm-grid` today,
  working like GEOPM Service (save before the first write, restore when the client dies). **Root
  never reads user files or runs user code**: the runner reads the queue and host profile as the
  user and pipes what a verb needs on stdin. As root the binary runs only the allowlisted verbs,
  reads input from stdin only, refuses `--sysfs` and `--state-dir`, and blocks `SIGINT`,
  `SIGTERM` and `SIGHUP` while a verb runs. A separate helper can come back if needed.

## Configuration

JSON, like gru's config: one format for the user and no new dependency in gru. A queue:

```json
{
  "name": "turbo-cost",
  "host_profile": "profiles/xeon6-2s.json",
  "profiles": {
    "factory":  {},
    "no-turbo": { "cpu.turbo": "off" },
    "stable":   { "cpu.governor": "performance", "cpu.turbo": "off",
                  "cpu.idle.disable": ["deepest"], "uncore.freq": "initial_max" }
  },
  "workloads": { "ep": { "cmd": "bin/ep.C.x", "timeout_s": 600 }, "idle": { "cmd": "sleep 60" } },
  "reps": 30, "seed": 7,
  "thermal": { "band_c": 3, "timeout_s": 900 },
  "meters": ["rapl", "nvml"],
  "others": "wait", "drift": "stop",
  "out": "results/energy",
  "notify": { "cmd": ["sh", "-c", "\"$GRU_AGENT\" _agent notify --config \"$GRU_NOTIFY\" …"] }
}
```

The **host profile** (one per machine type, in the repo) holds the `factory` value of every
setting, the housekeeping and experiment CPUs, the idle-state names (on Xeon 6, upstream 6.8 may
show only ACPI names), a maximum power per energy zone, the boot-only expectations and the units
that may be paused. A queue profile lists only what differs, so every run has a complete declared
state: an unmentioned setting is held at its factory value, as `host_baseline.sh` and `slot.sh` do
today. Symbolic values (`hw_max`, `initial_max`, `deepest`) are resolved and recorded; unknown
names are refused.

## State engine

### Settings are plugins

```rust
trait Setting {
    fn id(&self) -> &'static str;                     // "cpu.governor"
    fn after(&self) -> &'static [&'static str];       // settings that define this one's valid range
    fn guard(&self) -> bool;                          // a daemon we stop so it can't undo our writes
    fn on_reboot(&self) -> OnReboot;                  // Restore | Drop
    fn read(&self) -> Result<Values>;                 // per object: "cpu3" -> "powersave"
    fn expect(&self, want: &Values, deps: &State) -> Values; // what the kernel will show
    fn validate(&self, want: &Values) -> Result<()>;  // the allowlist check, run as root
    fn apply(&self, want: &Values) -> Result<()>;
    fn restore(&self, saved: &Values) -> Result<()>;
}
```

Values are per object (CPU, package, uncore domain, IRQ, unit, GPU), so CPUs that differ are
restored exactly, where `slot.sh` has to refuse. `expect` knows what dependencies force: EPP is
`performance` under the `performance` governor (and is not written), and the maximum frequency is
capped under `no_turbo`. Offline CPUs (SMT off) are skipped; their cpufreq files return `EBUSY`.

| id | path | after | reboot |
|---|---|---|---|
| `cpu.driver_mode` | `intel_pstate/status` | – | restore |
| `cpu.smt` | `cpu/smt/control` | driver_mode | restore |
| `cpu.governor`, `cpu.epp`, `cpu.epb` | `cpufreq/policy*/{scaling_governor,energy_performance_preference}`, `cpu*/power/energy_perf_bias` | smt; epp after governor | restore |
| `cpu.turbo`, `cpu.perf_pct`, `cpu.hwp_boost` | `intel_pstate/{no_turbo,min_perf_pct,max_perf_pct,hwp_dynamic_boost}` | driver_mode; perf_pct after turbo | restore |
| `cpu.freq` | `scaling_{min,max}_freq` | driver_mode, turbo | restore |
| `cpu.idle.disable` | `cpu*/cpuidle/state*/disable`, on every CPU | smt | restore |
| `uncore.freq` | `intel_uncore_frequency/*/{min,max}_freq_khz`, packages before domains | – | restore |
| `rapl.limit`, `rapl.readable` | `intel-rapl:*/constraint_*`; group read of `energy_uj` | – | restore |
| `gpu.*` | NVML power limit, locked clocks, persistence mode ([Devices](#devices)) | – | restore |
| `kernel.*` | `nmi_watchdog`, KSM `run`, THP, `vm/stat_interval`, `numa_balancing`, osnoise options | – | restore |
| `cpus.units` | `systemctl set-property --runtime <top-level unit> AllowedCPUs=` | – | drop |
| `irq.affinity` | `/proc/irq/*/smp_affinity_list`, `default_smp_affinity` | smt, irqbalance | drop |
| `kthreads`, `wq.cpumask` | movable kernel threads (by pid, start time, name); `workqueue/cpumask` (hex) | smt | drop |
| guards | `systemctl mask --runtime` and `stop` irqbalance, tuned, thermald, listed timers | – | drop |

- `drop`: a reboot rebuilds that state (IRQ numbers, cgroups, units), so old values must not
  come back. `AllowedCPUs` is undone with `set-property --runtime <unit> AllowedCPUs=<old or
  empty>`, never `systemctl revert`, which also deletes the admins' permanent drop-ins.
- Known refusals are recorded, and fail the apply only if the profile needs the value: managed
  IRQs (e.g. NVMe queues) `EIO`, BIOS-locked RAPL limits `EACCES`, turbo disabled in BIOS `EPERM`.
- `rapl.readable` gives one group read access, which reopens the CVE-2020-8694 side channel to
  that group; it is recorded.

### Order

- **Guards first, released last.** Daemons that would undo our writes (irqbalance rewrites
  affinities every 10 s) are masked and stopped before anything else. Only the ones that were
  active start again at the end. An active daemon that is not a listed guard fails the apply.
- **Everything else: dependencies first, in both directions**, because a dependency defines the
  valid range of what depends on it. The driver mode goes first (an active/passive switch resets
  all cpufreq and intel_pstate settings); the governor before EPP (EPP writes return `EBUSY` under
  `performance`); turbo before frequency and perf limits (`no_turbo` clamps them and doesn't raise
  `min_perf_pct` again); SMT before IRQs and kernel threads (offlining a CPU resets their affinity
  for good; cpufreq, idle-state and EPB settings survive). After a dependency is written, its
  dependents are written again.
- Min/max pairs are one plugin that keeps min ≤ max at every step (raise: max first; lower: min
  first); the Cascade Lake uncore driver doesn't check it.

*This changes the first design, which said "restore in reverse order". Reverse order breaks
exactly these pairs; `slot.sh` already restores uncore packages before domains.*

### Journal

One journal per machine: `/var/lib/minion/journal.jsonl`, root-owned, world-readable (it
holds no secrets) and on local disk. Every privileged verb holds an `flock` on
`/var/lib/minion/lock` while it runs, every line is `fsync`ed (the directory too, on
create), and every line carries the `boot_id`, `time` (epoch seconds) and `time_utc`:

```
{"t":"open","queue":"turbo-cost-20261123T091200Z","uid":1000,"job":"…","kernel":"…","cmdline":"…","microcode":"…","bios":"…"}
{"t":"saved","state":{…every object of every plugin…}}       <- originals, before any write
{"t":"apply","profile":"stable"}                              <- intent, before the writes
{"t":"applied","profile":"stable","state":{…read back…}}      <- after the writes, with side effects
{"t":"drift","state":{…objects someone else changed…}}
{"t":"restored"}
{"t":"close"}
```

- `saved` is a full snapshot at open, so it also covers settings that only change as a side effect
  of our writes. It is never overwritten. `applied` is the full read-back after each apply, with
  what the kernel derived. An incomplete last line is ignored.
- A journal open while no queue holds the [queue lock](#queue), or from another boot, is a
  *leftover*. On close it moves to `done/<YYYYMMDDTHHMMSS.nnnnnnnnnZ>.jsonl` (directory
  `fsync`ed) and the runner copies it into the results.

### Restore, crashes and reboots

Restore first reads the current state. An object that differs from both the last `applied` snapshot
and its `saved` value was changed by someone else during the queue: it is kept, recorded, and
reported as `restore_conflict`. Every other object gets its `saved` value, dependencies first,
whatever the side effects. If the queue died between an `apply` intent and its `applied` snapshot,
every object is restored. So restoring twice is harmless. (Once settings with side effects arrive,
such as driver mode, SMT, or turbo with frequency limits, a restore must also mark its start, or a
second restore after an interrupted one would take those side effects for someone else's change.)
Until the boot unit exists, a journal from another boot is never written back.

Without a service, two paths clean up after a crash:
- **The queue itself.** It starts in this order: take the [queue lock](#queue); choose its id and
  write it into the lock; `reap` a dead queue's workloads; `restore --held` a leftover journal,
  every time. Only then does it `apply --held` its profiles. At its end, or on `SIGTERM`,
  `SIGINT` or `SIGHUP`, it stops its workload and restores its own state.
- **The `always` hooks**, `minion reap` then plain `restore`. Both act only while no queue
  holds the lock; otherwise that queue already reaped and restored at its start, and a late
  restore would only undo its profile.
- **Stray calls.** A queue's `sudo` child can outlive it: sudo starts in milliseconds, and neither
  `PR_SET_PDEATHSIG` nor an inherited fd survives its setuid exec. So `apply` and `restore` check
  the queue lock while they hold the journal lock. With `--held`, stdin names the caller's queue
  (`{"queue": ID}`, plus `"profile"` for apply), and they act only while that queue holds the
  lock. Without it, they act only while no queue does; a hand-run `apply` leaves a leftover. A
  stray call that saw a dead queue's id before the next queue wrote its own is undone by that
  queue's restore at start.
- **Leftover workloads.** A workload gets `PR_SET_PDEATHSIG(SIGKILL)` and
  `MINION_WORKLOAD=<lock> <queue id>` in its environment, which its children inherit.
  `reap`, as the user and refused as root, kills the user's processes that name this lock and a
  queue other than the holder. Root never kills anything, since the lock file is world-writable.
  A workload that clears its environment escapes `reap` until the isolation phase's cgroup.

| What happens | Recovery |
|---|---|
| `gru kill`, `SIGTERM`, `SIGINT`, `SIGHUP`, shutdown | `gru kill` signals the job's process group. The workload has its own group, so the runner forwards the signal to it (a leader's ssh sessions also sit in their own groups, so it can still reach its followers). The runner stops the workload, restores, sends events, releases the lock and exits; the hooks then do nothing. This must fit systemd's stop timeout (90 s default); while the system is stopping, only the `restore` settings are put back, and guards and `drop` settings are left to the shutdown |
| runner killed with `-9` or crashes, any phase | the workload's first process dies with the runner (`PDEATHSIG`). The lock is free, so the `always` hooks reap the workload's children and restore; if a waiting queue took the lock first, that queue reaps and restores before its first run, and the hooks leave it alone. Root writes no user files and sends nothing: gru's own failure notice reports it, with the restore output in the log, and `--resume` treats a run without an end record as aborted and copies the closed journal from `done/`. |
| the whole job is killed, or the runner was started by hand | no hook runs; the journal stays open until the next queue reaps and restores, or someone runs `minion reap; sudo -n minion restore` |
| restore fails | the journal stays open, so every later start tries again and refuses to run on a half-restored machine, and sends `restore_failed`. An admin repairs and runs `minion abandon` |
| reboot | **not covered until the boot unit** ([Plan](#plan)). The runner makes no change that survives a reboot (sysfs, `/proc`, NVML and `--runtime` systemd state only), so every setting comes back at its boot value, but values set by hand before the queue are lost. The next start sees a journal from another boot, writes nothing, records the state, closes it and sends `rebooted`, to whoever starts next. `gru ps` keeps showing the dead job as running (no status was written) |
| someone changes a setting | caught by the end-of-run verify and by a full verify before each apply. The verify includes `HWP_REQUEST` through the `msr` verb, which catches direct MSR writes. The run is marked `drifted` with the diff. `drift: stop` (default) stops and restores; `reapply` re-applies once, for agreed slots. A change made and undone inside one run shows only through its effects |

The sudoers rule allows `apply --held`, `restore --held` and `restore`. A follow-up for shared
machines: `--held` should also check `SUDO_UID` against the journal's `uid` (admins excepted), since
anyone can read the queue id. The holder, admins, root and the expiry timer may always `unlock` the
[machine lock](#machine-lock-opt-in). `msr` reads only `SMI_COUNT`, `APERF`, `MPERF`, `THERM_STATUS`
and `HWP_REQUEST`.

The verbs refuse settings that are not compiled in; CPUs that don't exist; values outside the
hardware's range (frequencies within `cpuinfo_*`, uncore within `initial_*`, RAPL limits between a
floor and the default); offlining every housekeeping CPU; units not in the root-owned
`/etc/minion/units`; `AllowedCPUs` below the top level; restricting other users
(`AllowedCPUs` of shared slices, paused units, RAPL limits, the lock) on a machine that has not
opted in or for longer than its maximum; writes while another queue's journal is open; queues whose
boot-only checks fail.

### Boot-only settings: detect, record, refuse

Read, never written: `/proc/cmdline` (`isolcpus`, `nohz_full`, `rcu_nocbs`, `irqaffinity`,
`intel_pstate`, `intel_idle.max_cstate`, `mitigations`), `/sys/devices/system/cpu/{isolated,
nohz_full}`, `workqueue/cpumask_requested`, kernel, microcode and BIOS versions, idle states, HWP,
CPU count and frequency range. Firmware power options show only through their effects. A mismatch
with the host profile refuses the queue before anything is written.

## Isolation

| | What |
|---|---|
| controls | `AllowedCPUs` of every top-level unit (`system.slice`, `user.slice`, `init.scope`, `machine.slice`) to the housekeeping CPUs; the queue's delegated cgroup (below) split into `hk/` (runner) and `exp/` (workload, started there with `CLONE_INTO_CGROUP`); movable IRQs, `default_smp_affinity`, movable kernel threads and the workqueue mask to housekeeping; listed units paused; all journaled |
| detects only | managed IRQs, per-CPU kernel threads, SMIs, other users' logins and processes, boot-only flags |
| records | all of the above, in every run's noise record |

- **The delegated cgroup, without a unit file** (not yet tried): the `scope` verb moves the runner
  into a transient scope in `minion.slice` (systemd's bus API, as `systemd-run --scope`), outside
  the restricted `user.slice`, with `Delegate=yes`, and gives the user its directory,
  `cgroup.procs`, `cgroup.threads` and `cgroup.subtree_control`. `reap` then kills a dead queue's
  scope (`cgroup.kill`), as the user, with no environment marker to escape.
- The runner owns its delegated subtree, so placing a workload needs no root call. `AllowedCPUs`
  never moves kernel threads (they live in the root cgroup), hence the `kthreads` plugin.
- No cgroup cpuset partitions: on 6.8 an `isolated` partition must sit directly under the root
  cgroup or needs writes into systemd's cgroups, which systemd forbids; 5.15 has no `isolated`.
- **CPU choice.** On Cascade Lake every RAPL read and uncore write runs on the package's
  lowest-numbered CPU, and every core-temperature read on that core, through an IPI (a cross-CPU
  interrupt), so the lowest CPU of each package is always housekeeping. On Xeon 6, 6.8 should read
  RAPL over TPMI (memory-mapped, no IPI; check on the host). The runner sits on a housekeeping CPU
  that is not CPU 0 and whose SMT sibling is housekeeping too; RQ2 tests the alternatives.
- Isolation keeps other work off the experiment cores, **not out of the package counter**, which
  includes the housekeeping cores. So we pause what can be paused and record the rest.
- Other users' running work can't be stopped. Before each run the runner checks logins, load and
  other cgroups' CPU time: `others: wait` (default) waits for quiet, `refuse` stops, `mark` runs
  and marks. Agreed time slots stay a human agreement.

### Machine lock (opt-in)

While a queue runs, *new* ssh logins of other users can be refused. The lock is off unless the
admins enable it in `/etc/minion/config`, with a maximum duration and an admin group. Two
lines go once into the account part of `/etc/pam.d/sshd`; sshd runs that stack for every
authentication type, keys included, and reads PAM files at each login, so nothing restarts:

```
account [success=1 default=ignore] pam_access.so accessfile=/run/minion/allow.conf
account [auth_err=bad default=ignore] pam_nologin.so file=/run/minion/nologin
```

- **Lock**: write `allow.conf` (`+:root <holder> (<admin group>):ALL`, then `-:ALL:ALL`) through a
  temporary file and a rename, read it back, then create `nologin`. pam_nologin refuses the others
  with `PAM_AUTH_ERR` and shows its text ("locked for energy experiments by <holder> until
  2026-11-23T18:00:00Z"). **Unlock** removes `nologin`; the holder, admins and root may always
  unlock.
- **Fails open**: without `nologin`, pam_nologin returns `PAM_IGNORE`; `default=ignore` skips any
  other result, including a module that fails to load and pam_access's `PAM_ABORT` for a missing
  `accessfile` (a tmpfiles.d line creates an empty one at boot). An empty or garbled `allow.conf`
  matches nobody, which pam_access treats as success, hence the read-back. The lock is refused if
  the admin group is empty. With `pamtester`, the `lock` verb checks that the holder and an admin
  get in and a non-member doesn't.
- **Never locked out**: root (pam_nologin never refuses it), the holder, and the admin group from
  the root-owned config. The BMC console goes through `/etc/pam.d/login`, which is not touched. A
  reboot clears both files (`/run` is in memory).
- **Hard expiry**: locking also starts `systemd-run --on-active=<duration> minion unlock`,
  capped at the configured maximum, so the lock lifts even if the runner is dead.
- **Provisional until checked**: gru opens a fresh login (`ssh -o ControlPath=none <host>
  true`), then runs `minion confirm` over its kept connection, or `unlock` if the login
  failed. Without a confirmation within 5 minutes the runner lifts the lock and continues
  unlocked. With interactive 2FA the check asks for the code.
- **Cannot block** (detected, recorded, then `others` applies): sessions opened before the lock,
  including new sessions over an authenticated multiplexed connection; running processes; cron and
  at jobs; lingering users' systemd services; other network services.

Queue order: quiet-host check, lock, check, apply, runs, restore, unlock. `restore`, the expiry
timer and a reboot also unlock.

## Queue

- **Order.** Every (workload, profile, rep) runs in randomized interleaved rounds: each round runs
  every (workload, profile) once, in a new order from the seeded generator. The whole order goes
  to `plan.json` first, so a resume continues it. A `blocked` order (all A, then all B) exists
  only to measure order effects. Later: stop a pair once the confidence interval of its median is
  within a target, between a minimum and a maximum of reps (Hoefler & Belli 2015; CONFIRM).
- **Thermal gate, per profile** (C-states off idle hotter; our own rule): the first run of a
  profile waits until package temperatures change less than 0.5 °C/min and records them as its
  baseline; later runs wait until every package is within `band_c` of it (Krun uses ±3 °C), up to
  `timeout_s`, then run flagged. The wait and start temperature are recorded.
- **Between runs**: verify, apply the next profile's diff, verify, send events, thermal gate.
- **Canary** (optional): a fixed workload every N rounds. If its energy drifts past a bound, the
  queue pauses and notifies, as the Green Metrics Tool does with its control workload.
- **One queue at a time; the others wait.** Every `queue` (later `hold`) first takes a blocking
  `flock` on `/run/lock/minion.lock` and keeps it to the end. The file is created writable
  by all, is gone at reboot, is opened `O_NONBLOCK` and `O_CLOEXEC`, and must be a regular file,
  so a FIFO in its place can't block a restore. The holder writes its queue id into it, chosen
  after taking the lock (`<name>-<UTC time>`, with `-2`, `-3`, ... for a same-second name). A
  multi-host leader takes its own lock in host-name order with its followers'. A waiter sleeps in
  the kernel with no wakeups and logs the holder's queue id. The lock is freed when its holder dies,
  also by `kill -9`; then all waiters wake and one wins (not first come, first served). Any local
  user can take it and block queues for as long as they hold it. This replaces hand-made "wait for
  job X" tasks.
- **Stop and resume.** `gru kill <id>` (or `SIGTERM`) stops the queue, which restores.
  `minion queue --resume <dir>` reruns the aborted run as a new attempt
  (`runs/0001-…-a2/`; the old one is kept) and continues.

## Meters and noise record

Meters are small plugins, read only at run edges (start, end, phase marks), kept in memory and
written after the window. A queue lists the ones it uses; `time` is always on.

- **`time`**: each edge records `CLOCK_MONOTONIC` (Python's `time.monotonic`) and `CLOCK_REALTIME`
  to line up with workload logs and BMC samples, and `CLOCK_BOOTTIME`, whose jump against
  `MONOTONIC` reveals a suspend. The host's and the sampler's NTP offsets are recorded.
- **`rapl`**: `energy_uj` of the powercap zones, found at runtime with their tree recorded; zones
  overlap (psys covers the packages), so one interface per package is summed. On Xeon 6 with 6.8,
  expect a TPMI zone and no perf `power/` events. Files are opened once and read with `pread`.
  sysfs, not perf: perf sends an IPI when an event opens and keeps a periodic overflow timer.
- **Wrap**: the value wraps at `max_energy_range_uj` (a 32-bit counter): with the common 61 µJ
  unit about 262 kJ, or 21 min at 205 W (computed); Intel's manual says about 60 s at high power.
  A slow timer reads each zone every `max_energy_range_uj / (2 × the zone's maximum power)` (from
  the host profile, since the DRAM zone often has no limit), and each pair of consecutive reads is
  corrected for one wrap.
- **`nvml`**: total energy per GPU (`nvmlDeviceGetTotalEnergyConsumption`, mJ since the driver
  loaded, Volta and newer), one session per queue via `dlopen` of `libnvidia-ml.so.1`.
  `nvmlDeviceGetPowerUsage` is a 1 s average on Ampere and newer, so it is not used. GPU state
  and noise: [Devices](#devices).
- **`wall`**, out of band: the wall sampler asks the BMC (IPMI-over-LAN DCMI or Redfish) once
  a second for the whole queue; edges are matched afterwards. DCMI gives integer watts over a
  vendor-chosen window, so each sample keeps the reported period (DCMI statistics period, Redfish
  `SensingInterval`). In the prototype the reading changed every 4-5 s and lagged the load, so
  wall energy suits only long runs. In band is a last resort: one long-lived reader, never a fork
  per second (on KCS the `kipmid` kernel thread may poll).
- **`perf`**: hardware counters (cycles, instructions, cache misses) counted, never sampled:
  `perf_event_open` on the workload with `enable_on_exec` and `inherit`, so it and its later
  children count from `exec`. A read sums live children, so a phase-mark read sends an IPI to each
  CPU running the workload; one group read per edge returns every counter. Events beyond the free
  counters (the NMI watchdog holds one) are multiplexed by a per-tick timer, so `time_enabled` and
  `time_running` are kept to show it didn't happen. Upstream `perf_event_paranoid` 2 allows
  user-space counts, but Ubuntu 24.04 kernels build with `CONFIG_SECURITY_PERF_EVENTS_RESTRICT=y`,
  which restricts unprivileged perf further (see the open questions).
- **`cmd`**: a user command printing `name value` lines (device counters, app metrics), run before
  the start read and after the end read, never at marks (that would fork inside the window).
- **Phase marks**: the workload writes a line to the FIFO at `$MINION_MARK`, and the runner reads
  the counters.
- **Time series** (optional, off by default): a timer at a declared period. Frequent sampling
  raises idle energy (Raffin & Trystram 2025), and RQ2 measures it.

The noise record is read before the start read (then `sync` and the settle) and after the end
read, outside the window:
- CPU time of every other cgroup (`cpu.stat`), foreign processes, logins, the load average;
- `/proc/interrupts` (timer, IPIs, device IRQs) and `/proc/softirqs`;
- per CPU: SMI count, APERF/MPERF (average frequency), throttle counters and temperatures, via the
  `msr` verb (one sudo call per edge, one IPI per CPU; if RQ2 shows a cost, one long-lived root
  reader per queue replaces it);
- new kernel log lines, the end-of-run verify, and the wall-energy window.

On 5.15, `scaling_cur_freq` and `/proc/cpuinfo` are never read during a run: each read interrupts
the CPU and may sleep 10 ms. osnoise is optional, off by default, and 6.8 only. With
`NO_OSNOISE_WORKLOAD` it runs no busy threads and counts IRQ, softirq, NMI and thread noise, but
its tracepoints write a trace buffer inside the window, so RQ2 measures its cost. Its default mode
occupies every CPU it measures, so it only serves as a probe before the queue.

## Devices

Devices are plugin objects like CPUs (`gpu0`), with the same save, apply, verify and restore. The
first set is GPUs, through NVML as root:
- **State**: power limit (`nvmlDeviceSetPowerManagementLimit`), locked clocks
  (`nvmlDeviceSetGpuLockedClocks`, Volta and newer) and persistence mode; none survives a reboot.
  Persistence is applied first and restored last: without it the GPU is torn down when the last
  client exits, such as a short-lived sudo verb, and the power limit goes with it.
  No NVML call that reads the locked clocks was found, so restore unlocks them
  (`nvmlDeviceResetGpuLockedClocks`): exact only if they were unlocked before.
- **Boot-only**, detected and refused on mismatch: ECC mode (changes apply at the next reboot) and
  MIG mode (changes need a GPU reset), current and pending.
- **Noise**: other processes on the GPU (`nvmlDeviceGetComputeRunningProcesses`), clock-event
  reasons and temperature at the edges. The meter is `nvml`.

Other devices later, for example a NIC's interrupt coalescing and IRQ affinity, or a disk's I/O
scheduler and read-ahead.

## Several machines (later phase)

One experiment can span hosts, like the two-machine A/B tests (one machine parks work, the other
is switched off, wall power on both). The simplest correct mechanism:
- **A leader drives followers over SSH**, with no new sockets or ports: one session per follower
  for the whole queue (`ssh -T -o ControlPath=none -o ServerAliveInterval=0 <host> minion
  hold`, no pty), one JSON line per command, each with a reply timeout.
- **Each host keeps its own state engine, journal and lock.** `hold` takes its host's lock and uses
  its own privileged verbs. The leader takes all locks, its own included, in host-name order, so
  two multi-host queues can't deadlock.
- **A follower holds its state only while the leader's session lives.** `hold` restores and exits
  at EOF on stdin: when the connection ends, sshd closes a non-pty session's pipes and sends no
  signal. Each leader line also sets a lease, the longest planned silence (a run's `timeout_s` or
  the thermal gate's, plus a margin), for a leader host that dies without closing the connection.
  A late reply flags the run, and the leader stops and restores everywhere. If a follower dies,
  the leader runs `minion reap; sudo -n minion restore` there over a new session
  (they act only on a leftover and never wait on the lock).
- **Shared edges.** The leader asks every host to read its meters at each edge, then reads its own;
  each host returns its readings and `CLOCK_REALTIME`, so the round trip (skew) and clock offset
  are recorded, with each NTP offset. A follower running part of the workload sends one line when
  it ends, the only traffic inside the window. The leader's plan and seed are stored on every host.
- **Cost.** Between lines sshd waits with no timeout (`ClientAliveInterval` defaults to 0) and TCP
  keepalive starts after 2 h of silence, so the session should cause no periodic wakeups; it still
  holds processes and a connection on each host, and RQ2 measures it.
- **Energy is compared only on the same machine**; results from several machines are per host.

## Events and notifications

The runner decides when, a command decides how. Every event goes to `events.jsonl`. Only outside
measured windows (after a run before the thermal gate, or while waiting, starting or exiting), it
runs `notify.cmd` (an argv) once per event, in its own process group, killed after
`notify.timeout_s` (default 30), with the event in `MINION_EVENT`, `MINION_QUEUE`, `MINION_RUN`,
`MINION_STATUS` and `MINION_DETAIL`:

`queue_started` (with the wait for the lock), `run_done`, `run_failed`, `run_aborted`, `drift`,
`others_detected`, `thermal_timeout`, `canary_drift`, `restore_ok`, `restore_conflict`,
`restore_failed`, `rebooted`, `queue_paused`, `queue_done`, `lock_taken`, `lock_lifted` (done,
failed, killed, expired), `lockout_check_failed`, and an optional daily `heartbeat`.

With gru the command is gru's agent ([DESIGN.md](DESIGN.md#how-they-connect)), which reads the 0600
notify file at `$GRU_NOTIFY` (gru change 7), so secrets stay out of the queue and the environment.
Without gru it is any script, e.g. `curl` to a webhook. Root never notifies (it runs no user code):
after a `kill -9`, gru's job notice reports the failure, and a leftover is reported by the runner
that finds it. The laptop never polls: `gru fetch` and an on-demand status read are the only logins.
A dead machine can't notify, so optionally the runner pings a heartbeat URL at queue start, after
each run and at the end, and a service such as Healthchecks.io alerts when a ping is late.

## Results

```
<out>/turbo-cost-20261123T091200Z/
  queue.json      the queue as given, with resolved values and the seed
  plan.json       the full order: round and position of every run
  host.json       CPU model and topology, kernel, cmdline, microcode, BIOS, boot-only checks,
                  energy zone tree and backend (MSR, TPMI, modeled), NVML and driver versions,
                  runner version, gru job id, git commit and dirty flag (from gru's env)
  journal.jsonl   this queue's journal
  events.jsonl
  wall/           BMC samples from the wall sampler
  runs/0001-ep-stable-r03-a1/
    run.json      round, position, attempt, workload, profile, three clocks at each edge and mark,
                  exit, energy per zone, thermal wait, state before and after, noise record, flags,
                  gru job id, SLURM id if any
    stdout, stderr, the workload's own files
```

Runs only ever go to new directories. `out` must be on local disk, like every runner path. It is
relative to the project's `remote_dir` and under `results/`, so a `fetch` glob brings everything
back. The queue dir ends in its start time (`20261123T091200Z`, `-2` for a same-second name).
`run.json` holds both clocks at each edge, `CLOCK_MONOTONIC` and `CLOCK_REALTIME` in epoch ns, the
latter also as `*_utc`; `queue.json` holds the queue's start the same way, and its end is the
`queue_done` event. Every runner log line starts with the UTC time.

## Research questions

- **RQ1: which settings matter, and what they cost.** One profile per setting changed from
  `factory`, plus `stable`, interleaved, with 30 or more runs each. Per profile, report the
  variation (coefficient of variation, nonparametric confidence interval of the median) and the
  cost (energy and time against `factory`). The noise record separates variance due to the setting
  from variance due to interference.
- **RQ2: exact and safe restore, without disturbance.** *Restore:* `runner snapshot` before a
  queue and after its restore must match, also with `kill -9` at random points in apply, run and
  restore, and (with the boot unit) a reboot mid-queue. *Overhead:* fixed workloads and a long
  idle window with (a) no tool, (b) the runner, (c) today's Python sampler plus a per-second
  `ipmitool` fork, (d) time-series mode at a few rates, (e) an idle `hold` session on both hosts.
  For (b), the runner on CPU 0, on a package's lowest CPU (where RAPL is read), on another
  housekeeping CPU, and on an experiment CPU's SMT sibling, with osnoise on and off. Compare
  energy (RAPL and wall), run time, IPIs and timer interrupts on experiment CPUs, and idle-state
  residency.
- **RQ3: smallest detectable regression.** Two queue modes. *A/A* runs the same code on both sides
  and gives the false-positive rate. *Injected* adds a calibrated extra cost (extra work, a
  frequency cap) and gives the detection rate. Measure both against the number of runs, with and
  without the noise record: flag noisy runs, or adjust with covariates measured before the run,
  which the change can't affect. No prior work reports this for energy on one machine. The
  analysis is Python over `run.json`.

## gru changes

Each one is small and useful without the runner. Items 1-8 are merged (PRs #3, #6, #7, #8, #10)
and described in [GRU.md](GRU.md).

1. **Job state on local disk**: `state_dir` per host (default `~/.gru`), under `umask 077`.
2. **Jobs per host**: `ps` and the latest-job lookup see only this host's jobs; `kill` refuses
   another host's job.
3. **Unique job ids**: a random name plus the start time, made with a plain `mkdir`.
4. **Agents never rewritten in place**: each agent build gets its own file.
5. **Quiet attach**: `_agent follow` with inotify on the log and a pidfd on the job.
6. **Provenance**: `run.json` has nanosecond times, kernel and hostname and is fetched into
   `_gru/`; jobs see `GRU_GIT` and `GRU_DIRTY`.
7. **Notify for tasks**: the notify block is a 0600 file at `$GRU_NOTIFY`, and
   `$GRU_AGENT _agent notify` sends a given subject and body, so the runner can use it.
8. **UTC everywhere**: job ids and `{ts}` as `20261123T142501Z`, `ps` in UTC with the `Z`.
9. **Lockout check** (later): `gru lock-check` logs in once with `-o ControlPath=none`,
   bypassing shared connections, then runs `minion confirm`, or `unlock` if the login
   failed, over the kept connection (`gru/internal/ssh`).

## Plan

Each phase has a "done when", checked on a test machine.

1. **gru fixes** (merged, items 1-8). Done when ids, `{ts}` and `ps` are UTC and `make test`
   passes.
2. **State engine** (merged, PR #2): the journal, the verbs `snapshot`, `apply`, `restore`,
   `status`, and three settings: governor, turbo, idle states. Done when snapshots before and after
   match, also after `kill -9` at each step, and as root the binary refuses the test overrides.
2b. **`minion run`** (first slice merged, PR #4: `time` and `rapl` at the edges, a drift
   flag): one workload under one profile, the subreaper, the window rule. Done when `run.json` has
   energy per zone and the three clocks, `strace` shows no runner syscall inside the window but
   the wakeup that ends it, and the state is restored after success, failure and `SIGTERM`.
3. **Queue and the gru connection** (merged, PRs #9 and #11; `make e2e` runs it on a fake sysfs,
   PR #12): `queue`, `plan.json`, lock waiting, restore
   first, `reap`, the queue-id checks, `notify.cmd`. Done when two queues started
   together run one after the other, a `kill -9` of the runner at any point leaves the machine
   restored, notifications arrive only outside windows, and `gru fetch` brings back results with
   job id and commit.
4. **Meters, noise record, isolation**: `nvml`, `wall`, `perf`, `cmd`, the thermal gate, the other
   settings, the delegated scope. Done when 2 profiles × 2 workloads × 10 reps run unattended with
   full provenance and the RQ2 overhead experiment has run; `host_baseline.sh` and `slot.sh` retire.
5. **Machine lock** (opt-in, item 9). Done when another account is refused and sees the message
   while the holder, root and an admin get in; the lock lifts at the end, after `kill -9` of the
   runner, at expiry and after a reboot; and a broken PAM module or `allow.conf` leaves logins open.
6. **Several machines**. Done when a two-host queue runs with shared edges, and killing the leader
   or cutting its network leaves the follower restored within one lease.
7. **GPU and device state**. Done when GPU snapshots before and after a queue match and ECC or MIG
   mismatches are refused.
8. **Boot unit**: a root service before user sessions that never fails the boot, loads the uncore,
   RAPL and coretemp drivers, and restores an open journal's `restore` settings only if kernel,
   cmdline, microcode and BIOS match. Done when snapshots match after a reboot mid-queue.
9. **RQ3**: the A/A and injected modes and the Python analysis. Done when the detection rate
   against the number of runs is measured for two or more injected sizes, with and without the
   noise record.

## Open questions

1. Can the admins install the runner binary and its sudoers rule on each host (no units for now)?
2. Is each BMC reachable from another machine, and with which credentials?
3. Until the boot unit, a reboot loses values set by hand before a queue. With it, the journal
   restores them, other users' included (the task asks for this), only on the same kernel and
   firmware. Confirm, since a user may expect a reboot to clear them.
4. Which timers and services may be paused on the shared machines?
5. Restricting other users (the lock, `AllowedCPUs` on shared slices, RAPL limits) needs root and
   the agreement of the admins and those users. Which machines opt in, with which maximum and
   admin group? On shared machines, keep it opt-in and time-limited.
6. The Cascade Lake host runs kernel 5.15: no osnoise no-workload mode, and frequency reads
   interrupt the CPU read. Move it to 24.04 (6.8), or only record the gap?
7. How do we enable the `perf` meter on the Ubuntu hosts: an admin sysctl, or `CAP_PERFMON` for
   the runner?
8. Isolation needs a delegated cgroup; without unit files, the plan is a transient scope. Is a
   transient (not installed) unit acceptable?
