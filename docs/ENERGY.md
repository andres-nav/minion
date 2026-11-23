# Energy runner — design

**Status:** proposal; nothing is implemented. Survey and sources: [ENERGY-RESEARCH.md](ENERGY-RESEARCH.md).

## Summary

Energy numbers depend on the machine's state (governor, turbo, idle states, uncore, SMT, interrupts,
temperature, other users) as much as on the code under test, and today that state is set by hand
with scripts that are neither recorded nor undone. This design adds a small **runner** on the
measured host. It records every setting, applies a declared profile and checks it. It runs a queue
in randomized interleaved order, reads the meters only at run edges, and stores a noise record with
every result. It pushes notifications between runs, and puts every setting back to its
**original** value, also after a crash or a reboot. minion stays the laptop side. The goals follow
the project task description (M. Friedman, CORES Lab, ETH Zurich, 2026). GEOPM Service, Slurm and
tuned also restore originals, but only from memory or `/run`; an on-disk journal that survives a
reboot is new.

## Goals and non-goals

Goals: declarative, reversible state (the originals come back after `kill -9`, a crash or a
reboot); repeatable runs (isolation, randomized interleaved order with a recorded seed, a thermal
gate, refusal when boot-only settings don't match); negligible overhead; every result carrying its
state, noise record, commit, config, seed, hardware and job id; enough data for the three
[research questions](#research-questions).

Non-goals: scheduling across machines, attributing energy to tenants or processes, a benchmark
framework (workloads stay the user's commands), dashboards, anything but Linux.

## The rule: stay out of the measured window

A *measured window* runs from the counter reads at the start of a run to those at its end. Inside
it the runner does not log, write, send or fork. It sleeps in one `poll()` on the workload cgroup's
`cgroup.events` and a mark FIFO (opened `O_RDWR`, so a closed writer can't make it spin), on a
housekeeping CPU, with a large timer slack and no real-time priority. The only exception is the
slow counter-wrap timer. The window ends when the workload's cgroup is empty, so children (MPI
ranks, `cmd &`) count. Leftovers after a grace period are killed with `cgroup.kill`, and the run is
flagged.

Before `exec`, the child resets its timer slack to the default 50 µs (slack is inherited) and its
signal mask. Before the start read come the noise snapshot, then `sync` (so writes from between
runs are not flushed inside the window), then a short recorded settle.

## Components

| Part | Where | Language | Does |
|---|---|---|---|
| minion | laptop | Go, stdlib only | sync, launch, fetch, notify config |
| runner | host, as the user | Rust | `submit`, queue, meters, noise record, events; `snapshot`, `verify`, `stop`, `resume`, `mark` |
| helper | host, root via one sudoers rule | Rust | the only writer of settings; owns the journal |
| `energy-recover.service` | host, root | the helper | `helper recover`, at boot and when the runner unit fails |
| wall sampler | another machine | the runner binary | reads the BMC over the network during the queue |

- minion stays a user-level CLI with no dependencies. The host side needs root, NVML (a C library)
  and control over every wakeup: Rust has no runtime threads or garbage collector, while an idle
  Go program still wakes every minute or two (`runtime/proc.go`).
- All root actions go through one small helper with an allowlist, like `kvm-grid` today, working
  like GEOPM Service (save before the first write, restore when the client dies). **Root never
  reads user files or runs user code**: `energy-runner submit q.json` reads the queue, host
  profile and notify block as the user and pipes them to `sudo -n energy-helper start`, which
  validates and stores them under `/var/lib/energy-runner/` and starts the runner unit.
- The runner is an instance of a static unit, `energy-runner@<user>.service` (`User=%i`,
  `Slice=energy.slice`, `Delegate=yes`, `LoadCredential=` for notify secrets,
  `TimeoutStopSec=600`, `OnFailure=energy-recover.service`), with two `ExecStopPost=` lines:
  `+energy-helper recover` (root restores) and `energy-runner finish` (the user copies the journal
  into the results and sends pending events). So the queue survives logout, the workload dies
  with the runner (the cgroup is killed), and the restore runs on *any* exit: `ExecStopPost=`
  also runs after an unexpected exit.

## Configuration

JSON, like minion's config: one format for the user and no new dependency in minion. A queue:

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
  "workloads": { "ep": { "cmd": "bin/ep.C.x" }, "idle": { "cmd": "sleep 60" } },
  "reps": 30, "seed": 7,
  "thermal": { "band_c": 3, "timeout_s": 900 },
  "meters": ["rapl", "nvml"],
  "others": "wait", "drift": "stop",
  "out": "results/energy",
  "notify": { "events": ["run_failed", "drift", "restore_failed", "queue_done"] }
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
    fn validate(&self, want: &Values) -> Result<()>;  // the allowlist check, run by the helper
    fn apply(&self, want: &Values) -> Result<()>;
    fn restore(&self, saved: &Values) -> Result<()>;
}
```

Values are per object (CPU, package, uncore domain, IRQ, unit), so CPUs that differ are restored
exactly, where `slot.sh` has to refuse. `expect` knows what dependencies force: EPP is
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
| `gpu.limit` | NVML power limit and locked clocks (recorded first, written later) | – | restore |
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

One journal per machine: `/var/lib/energy-runner/journal.jsonl`, root-owned and on local disk.
Every helper verb takes an `flock` on it, every line is `fsync`ed (the directory too, on create),
and every line carries the `boot_id`:

```
{"t":"open","queue":"turbo-cost-20261123-0912","uid":1000,"kernel":"…","cmdline":"…","microcode":"…","bios":"…"}
{"t":"saved","state":{…every object of every plugin…}}       <- originals, before any write
{"t":"apply","profile":"stable"}                              <- intent, before the writes
{"t":"applied","profile":"stable","state":{…read back…}}      <- after the writes, with side effects
{"t":"drift","state":{…objects someone else changed…}}
{"t":"restored"}
{"t":"close"}
```

- `saved` is a full snapshot at open, so it also covers settings that only change as a side
  effect of our writes. It is never overwritten, except that `drop` settings are saved again when
  a queue resumes after a reboot. `applied` is the full read-back after each apply, with what the
  kernel derived. An incomplete last line is ignored.
- An open journal refuses every other queue: it is the machine lock. Its owner's liveness is
  judged by the unit's state, not a pid. On close it is renamed to `done/<queue>.jsonl` (directory
  `fsync`ed) and `finish` copies it into the results.

### Restore, crashes and reboots

Restore first reads the current state. An object that differs from both the last `applied`
snapshot and its `saved` value was changed by someone else during the queue: it is kept, recorded,
and reported as `restore_conflict`. Every other object gets its `saved` value, dependencies first,
whatever the side effects. If the queue died between an `apply` intent and its `applied` snapshot,
every object is restored. So restoring twice is harmless.

| What happens | Recovery |
|---|---|
| runner killed or crashes, any phase | systemd kills the unit's cgroup (the workload too). `helper recover` restores, unlocks, marks the open run `aborted` and closes the journal. `finish` (as the user) copies it and sends events |
| recover fails or is cut off | `energy-recover.service` retries a few times. If it still fails: unlock, keep the journal open (no new queue on a half-restored machine), send `restore_failed`. An admin repairs and runs `helper abandon` |
| clean shutdown | recover restores only the `restore` settings and leaves the queue `paused` |
| reboot | the lock is gone with `/run`. At boot, `energy-recover.service` runs with `Before=systemd-user-sessions.service` and `TimeoutStartSec=60`, and never fails the boot. It loads the uncore, RAPL and coretemp drivers and waits for their paths. It restores the `restore` settings only if kernel, cmdline, microcode and BIOS match the journal; otherwise it only records. The queue stays `paused`, and `rebooted` is sent once the network is up |
| someone changes a setting | caught by the end-of-run verify and by a full verify before each apply. The verify includes `HWP_REQUEST` through the helper's `msr` verb, which catches direct MSR writes. The run is marked `drifted` with the diff. `drift: stop` (default) stops and restores; `reapply` re-applies once, for agreed slots. A change made and undone inside one run shows only through its effects |

The helper authorizes each verb by `SUDO_UID`. Changing state is allowed for the journal's owner,
the admin group and root. `msr` reads only `SMI_COUNT`, `APERF`, `MPERF`, `THERM_STATUS` and
`HWP_REQUEST`.

It refuses settings that are not compiled in; CPUs that don't exist; values outside the hardware's
range (frequencies within `cpuinfo_*`, uncore within `initial_*`, RAPL limits between a floor and
the default); offlining every housekeeping CPU; units not in the root-owned
`/etc/energy-runner/units`; `AllowedCPUs` below the top level; restricting other users
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
| controls | `AllowedCPUs` of every top-level unit (`system.slice`, `user.slice`, `init.scope`, `machine.slice`) to the housekeeping CPUs; the runner's delegated cgroup split into `hk/` (runner) and `exp/` (workload, started there with `CLONE_INTO_CGROUP`); movable IRQs, `default_smp_affinity`, movable kernel threads and the workqueue mask to housekeeping; listed units paused; all journaled |
| detects only | managed IRQs, per-CPU kernel threads, SMIs, other users' logins and processes, boot-only flags |
| records | all of the above, in every run's noise record |

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
admins enable it in `/etc/energy-runner/config`, with a maximum duration and an admin group. Two
lines are installed once in the account part of `/etc/pam.d/sshd`:

```
account [success=1 default=ignore] pam_access.so accessfile=/run/energy-runner/allow.conf
account [auth_err=bad default=ignore] pam_nologin.so file=/run/energy-runner/nologin
```

sshd runs the PAM account stack for every authentication type, keys included, and PAM files are
read at each login, so nothing restarts.

- **Lock**: write `allow.conf` (`+:root <holder> (<admin group>):ALL`, then `-:ALL:ALL`) through
  a temporary file and a rename, read it back, then create `nologin`. pam_nologin refuses the
  others with `PAM_AUTH_ERR` and shows its text ("locked for energy experiments by <holder> until
  <time>"). **Unlock** removes `nologin`, and is always allowed for the holder, admins and root.
- **Fails open**: without `nologin`, pam_nologin returns `PAM_IGNORE`; `default=ignore` skips any
  other result, including a module that fails to load and the `PAM_ABORT` of pam_access for a
  missing `accessfile` (a tmpfiles.d line creates an empty one at boot). An empty or garbled
  `allow.conf` matches nobody, which pam_access treats as success, so the lock would silently be
  off: hence the read-back. The lock is refused if the admin group has no members. With
  `pamtester` installed, the helper checks that the holder and an admin get in and a non-member
  doesn't.
- **Never locked out**: root (pam_nologin never refuses it), the holder, and the admin group from
  the root-owned config (a queue can't drop it). The BMC console (serial over LAN, virtual
  console) goes through `/etc/pam.d/login`, which is not touched. A reboot clears both files
  (`/run` is in memory).
- **Hard expiry**: locking also starts `systemd-run --on-active=<duration> energy-helper unlock`,
  capped at the configured maximum, so the lock lifts even if the runner is dead.
- **Provisional until checked**: minion opens a fresh login (`ssh -o ControlPath=none <host>
  true`). If it works, minion runs `energy-helper confirm` over the connection it kept open;
  otherwise `unlock`. Without a confirmation within 5 minutes (also when no shared connection is
  left) the runner lifts the lock and continues unlocked. With interactive 2FA the check asks for
  the code.
- **Cannot block** (detected, recorded, then `others` applies): sessions opened before the lock,
  including new sessions over an already authenticated multiplexed connection (sshd
  `MaxSessions`); running processes; cron and at jobs; lingering users' systemd services; other
  network services.

Queue order: quiet-host check, lock, check, apply, runs, restore, unlock. `helper recover`, the
expiry timer and a reboot also unlock.

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
- **One queue per machine**, through the journal lock. This replaces the hand-made wait loops on
  job status files.
- **Stop and resume.** `energy-runner stop` stops the unit, which restores. `energy-runner resume`
  reruns the aborted run as a new attempt (`runs/0001-…-a2/`; the old one is kept) and continues.

## Meters and noise record

Read only at run edges (start, end, phase marks), kept in memory, written after the window.

- **RAPL**: `energy_uj` of the powercap zones, found at runtime with their tree recorded; zones
  overlap (psys covers the packages), so one interface per package is summed. On Xeon 6 with 6.8,
  expect a TPMI zone and no perf `power/` events. Files are opened once and read with `pread`.
  sysfs, not perf: perf sends an IPI when an event opens and keeps a periodic overflow timer.
- **Wrap**: the value wraps at `max_energy_range_uj` (a 32-bit counter): with the common 61 µJ
  unit about 262 kJ, or 21 min at 205 W (computed); Intel's manual says about 60 s at high power.
  A slow timer reads each zone every `max_energy_range_uj / (2 × the zone's maximum power)` (from
  the host profile, since the DRAM zone often has no limit), and each pair of consecutive reads is
  corrected for one wrap.
- **GPU**: NVML total energy (`nvmlDeviceGetTotalEnergyConsumption`, mJ, Volta and newer), one
  session per queue via `dlopen` of `libnvidia-ml.so.1`; clock-event reasons, temperature, power
  limit and locked clocks at edges. `nvmlDeviceGetPowerUsage` is a 1 s average on Ampere and
  newer, so it is not used. The persistence mode is recorded (without it, the driver state is
  rebuilt, costing seconds).
- **Wall power**, out of band: the wall sampler asks the BMC (IPMI-over-LAN DCMI or Redfish) once
  a second for the whole queue; edges are matched afterwards. DCMI gives integer watts over a
  vendor-chosen window, so each sample keeps the reported period (DCMI statistics period, Redfish
  `SensingInterval`). In the prototype the reading changed every 4-5 s and lagged the load, so
  wall energy suits only long runs. In band is a last resort: one long-lived reader, never a fork
  per second (on KCS the `kipmid` kernel thread may poll).
- **Clocks**: each edge records `CLOCK_MONOTONIC` (Python's `time.monotonic`) and `CLOCK_REALTIME`
  to line up with workload logs and BMC samples, and `CLOCK_BOOTTIME`, whose jump against
  `MONOTONIC` reveals a suspend. The host's and the sampler's NTP offsets are recorded.
- **Phase marks**: the workload writes a line to the FIFO at `$RUNNER_MARK`, and the runner reads
  the counters.
- **Time series** (optional, off by default): a timer at a declared period. Frequent sampling
  raises idle energy (Raffin & Trystram 2025), and RQ2 measures it.

The noise record is read before the start read (then `sync` and the settle) and after the end
read, outside the window:
- CPU time of every other cgroup (`cpu.stat`), foreign processes, logins, the load average;
- `/proc/interrupts` (timer, IPIs, device IRQs) and `/proc/softirqs`;
- per CPU: SMI count, APERF/MPERF (average frequency), throttle counters and temperatures, via the
  helper's `msr` verb (one sudo call per edge, one IPI per CPU; if RQ2 shows a cost, one
  long-lived root reader per queue replaces it);
- new kernel log lines, the end-of-run verify, and the wall-energy window.

On 5.15, `scaling_cur_freq` and `/proc/cpuinfo` are never read during a run: each read interrupts
the CPU and may sleep 10 ms. osnoise is optional, off by default, and 6.8 only. With
`NO_OSNOISE_WORKLOAD` it runs no busy threads and counts IRQ, softirq, NMI and thread noise, but
its tracepoints write a trace buffer inside the window, so RQ2 measures its cost. Its default mode
occupies every CPU it measures, so it only serves as a probe before the queue.

## Events and notifications

The runner appends events to `events.jsonl`, and root-side recovery to a spool under
`/var/lib/energy-runner` that `finish` sends. Events go out only between runs, right after a run
and before the thermal gate:

`queue_started`, `run_done`, `run_failed`, `run_aborted`, `drift`, `others_detected`,
`thermal_timeout`, `canary_drift`, `restore_ok`, `restore_conflict`, `restore_failed`,
`rebooted`, `queue_paused`, `queue_done`, `lock_taken`, `lock_lifted` (done, failed, killed,
expired), `lockout_check_failed`, and an optional daily `heartbeat`.

Sinks are minion's: email through the minion agent (`_agent notify`) and `run` hooks (e.g. a
`curl` webhook), filtered by `notify.events`. Secrets come from the laptop and reach the unit via
`LoadCredential=` from a 0600 file on local disk (a `rebooted` notice needs them too). The laptop
never polls: `minion fetch` and an on-demand status read are the only logins, and the noise record
shows logins during a run. A dead machine can't notify, so optionally the runner pings a heartbeat
URL at queue start, after each run and at the end, and a service such as Healthchecks.io alerts
when a ping is late.

## Results

```
<out>/<queue-id>/
  queue.json      the queue as given, with resolved values and the seed
  plan.json       the full order: round and position of every run
  host.json       CPU model and topology, kernel, cmdline, microcode, BIOS, boot-only checks,
                  energy zone tree and backend (MSR, TPMI, modeled), NVML and driver versions,
                  runner and helper versions, git commit and dirty flag (from minion)
  journal.jsonl   the helper's journal for this queue
  events.jsonl
  wall/           BMC samples from the wall sampler
  runs/0001-ep-stable-r03-a1/
    run.json      round, position, attempt, workload, profile, three clocks at each edge and mark,
                  exit, energy per zone, thermal wait, state before and after, noise record, flags,
                  minion job id, SLURM id if any
    stdout, stderr, the workload's own files
```

Runs only ever go to new directories. `out` must be on local disk, like every runner path. It is
relative to the project's `remote_dir`, so minion's existing `fetch` globs bring everything back.

## Research questions

- **RQ1: which settings matter, and what they cost.** One profile per setting changed from
  `factory`, plus `stable`, interleaved, with 30 or more runs each. Per profile, report the
  variation (coefficient of variation, nonparametric confidence interval of the median) and the
  cost (energy and time against `factory`). The noise record separates variance due to the setting
  from variance due to interference.
- **RQ2: exact and safe restore, without disturbance.** *Restore:* `runner snapshot` before a
  queue and after its restore must match, also with `kill -9` at random points in apply, run and
  restore, and with a reboot mid-queue. *Overhead:* fixed workloads and a long idle window with
  (a) no tool, (b) the runner, (c) today's Python sampler plus a per-second `ipmitool` fork,
  (d) time-series mode at a few rates. For (b), the runner on CPU 0, on a package's lowest CPU
  (where RAPL is read), on another housekeeping CPU, and on an experiment CPU's SMT sibling, with
  osnoise on and off. Compare energy (RAPL and wall), run time, IPIs and timer interrupts on
  experiment CPUs, and idle-state residency.
- **RQ3: smallest detectable regression.** Two queue modes. *A/A* runs the same code on both sides
  and gives the false-positive rate. *Injected* adds a calibrated extra cost (extra work, a
  frequency cap) and gives the detection rate. Measure both against the number of runs, with and
  without the noise record: flag noisy runs, or adjust with covariates measured before the run,
  which the change can't affect. No prior work reports this for energy on one machine. The
  analysis is Python over `run.json`.

## Minion changes

Verified in the code. Each one is small and useful without the runner.

1. **Job state on local disk.** Jobs live in `~/.minion/jobs` (`jobsDir`, `JobDir`,
   `LatestJobCommand` and `PsCommand` in `internal/remote/remote.go`), and the agent in
   `~/.minion/bin/minion` (`remoteBin` in `internal/ssh/ssh.go`). With an NFS home, logs are
   written over NFS during a run. The job script, which exports the SMTP password and is written
   before its `chmod 700`, is stored there. Every machine sees the others' jobs. Fix: add
   `state_dir` to `Host` (`internal/config/config.go`, default `~/.minion`), and set `umask 077`
   first.
2. **Jobs per host.** Record `hostname` in the job dir. `ps`, and `attach` or `fetch` without an id
   (`LatestJobCommand`), then see only this host's jobs. `kill` refuses another host's job: today
   it reads that host's pid, signals an unrelated local process, and writes 143 into the other
   job's `status` (`KillCommand`).
3. **Unique job ids.** `randomName` (`internal/cli/names.go`) has 40 × 40 = 1600 names, and the
   `mkdir -p` in `LaunchCommand` then reuses an old job's `status`, `run.json` and log. Fix: a plain
   `mkdir` with retry, and the time in the id.
4. **Agents never rewritten in place.** With one shared path, bootstrapping an arm64 host
   overwrites the amd64 agent that other machines run, and dev builds re-upload on every command
   (`EnsureAgent`, `upload`). Fix: upload to `<state_dir>/bin/minion-<version>-<goos>-<goarch>`,
   and never rewrite an existing file.
5. **Quiet attach.** `AttachCommand` loops on `sleep 1` beside `tail -f`, and on NFS GNU `tail`
   also polls every second. Fix: `_agent follow <jobdir>`, with inotify on the log (on local disk)
   and a pidfd on the job (`internal/cli/agent.go`).
6. **Provenance.** `run.json` uses `date +%s` and is never fetched (`Script` in `remote.go`,
   `pullResults` in `internal/cli/fetch.go`). Fix: record nanosecond times, kernel and hostname, and
   fetch `run.json` and the log into `_minion/`.
7. **Notify for tasks.** Write the resolved notify block to a 0600 file in the job dir. Export its
   path as `MINION_NOTIFY`, and the agent path as `MINION_AGENT` (`buildNotify` in
   `internal/cli/run.go`). The secrets stay out of the environment.
8. **Lockout check.** `minion lock-check` logs in once with `-o ControlPath=none`, which bypasses
   the user's and minion's shared connections. It then runs `energy-helper confirm`, or `unlock` if
   the login failed, over the kept connection (`internal/ssh`).

Launching needs no new verb. A task such as `"energy": "energy-runner submit energy/turbo.json"`
starts the unit and returns, so the minion job's notification only means "submitted".
`energy-runner stop`, not `minion kill`, stops a queue, and `minion fetch` brings the results back.

## Plan

1. **minion fixes** (items 1-7). Done when two hosts sharing a home each list only their own
   jobs, `kill` refuses another host's job, ids never collide, `attach` causes no periodic wakeups
   on the host (`strace -f -c`), fetched results contain `run.json` and the log, and `make test`
   passes.
2. **State engine**: the helper, the journal, the cpufreq, idle, turbo, uncore and RAPL plugins,
   `snapshot`/`verify`/`apply`/`restore`/`recover`, and the units. Done when, on a test machine,
   the snapshots before and after a queue match, both normally and after `kill -9` in each phase
   and after a reboot, and `host_baseline.sh` and `slot.sh` can be retired.
3. **Queue, meters, noise record, events, isolation.** Done when a queue of 2 profiles,
   2 workloads and 10 reps runs unattended with full provenance, notifications arrive only between
   runs, and the RQ2 overhead experiment has run.
4. **Machine lock** (opt-in, item 8). Done when, on a test machine, another account is refused
   and sees the message while the holder, root and an admin get in; the lock lifts at the end,
   after `kill -9` of the runner, at expiry and after a reboot; and a broken PAM module or
   `allow.conf` leaves logins open.
5. **RQ3**: the A/A and injected modes and the Python analysis. Done when the detection rate
   against the number of runs is measured for two injected sizes or more, with and without the
   noise record.

## Open questions

1. Where should the runner live: its own repository (recommended: another language, the
   project's deliverable) or `runner/` here?
2. Can the admins install the helper, its sudoers rule and the units on each host?
3. Is each BMC reachable from another machine, and with which credentials?
4. After a reboot the journal restores values set by hand, other users' included (the task asks
   for this), and only on the same kernel and firmware. Confirm, since a user may expect a reboot
   to clear them.
5. Which timers and services may be paused on the shared machines?
6. Restricting other users (the lock, `AllowedCPUs` on shared slices, RAPL limits) needs root and
   the agreement of the admins and those users. Which machines opt in, with which maximum and
   admin group? On shared machines, keep it opt-in and time-limited.
7. The Cascade Lake host runs kernel 5.15: no osnoise no-workload mode, and frequency reads
   interrupt the CPU read. Move it to 24.04 (6.8), or only record the gap?
8. A name: `energy-runner` is a placeholder.
