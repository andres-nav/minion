# minion — design

minion is the runner on the measured host. This file gives the reasons for what it does, the
design of what is planned (nothing under [Planned](#planned) is built) and the ranked
[improvements](#improvements-ranked). Usage and formats (queue file, journal, results, events):
[minion/README.md](../minion/README.md). How it works with gru:
[DESIGN.md](DESIGN.md#how-they-connect). Linux and tool facts, with sources:
[RESEARCH.md](RESEARCH.md).

## Why

Measurements, energy above all, depend on the machine's state (governor, turbo, idle states,
uncore, SMT, interrupts, temperature, other users) as much as on the code under test. Today that
state is set by hand with scripts (`host_baseline.sh`, `slot.sh`) that are neither recorded nor
undone. minion records the settings, applies a profile and checks it, runs a queue in randomized
interleaved order, reads its meters only at run edges, and puts every setting back to its original
value, also after a crash.

Goals: reversible state (after `kill -9` or a crash, and after a reboot once the [boot
unit](#boot-unit) exists); repeatable runs; negligible overhead; every result carrying its state,
commit, config, seed, hardware and job id; any command as a workload and meters as plugins; enough
data for the three [research questions](#research-questions). Non-goals: a cluster scheduler,
attributing energy to tenants or processes, a benchmark framework, dashboards, anything but Linux.

## Status

| Part | Built | Planned |
|---|---|---|
| Settings | `cpu.governor`, `cpu.epp`, `cpu.turbo`, `cpu.freq` (min and max), `cpu.idle.disable` | [more settings](#more-settings), [host profile](#host-profile-and-boot-only-checks) |
| Verbs | `snapshot`, `restore`, `run`, `queue`, `reap`, and `serve` (root's side) | `scope`, `msr`, `hold`, `abandon`, `queue --resume` |
| Meters | `time`, `rapl`, `cmd`, and a basic noise record (interrupts, softirqs, context switches, other cgroups' CPU time, logins, load) | [more meters and noise](#meters-and-noise-record) |
| Queue | randomized interleaved rounds under the queue lock, with reap, events and notify; the settle and a basic thermal wait (one baseline per queue) | [queue features](#queue-features), [tighter window](#tighter-window), [isolation](#isolation) |
| Restore | the fsynced journal, keeping what someone else changed; serve restores when its queue dies | the [boot unit](#boot-unit) |
| Tested | queues through gru on a fake sysfs (`make e2e`) | real sysfs writes, RAPL, sudo and ssh |

### Stay out of the measured window

A measured window runs from the meter reads right before the workload starts to those right after
its main process exits. Apart from starting the workload, the runner only waits in `waitpid`: it
does not log, write, send or fork. Settings snapshots, the settle, `run.json`, events,
`notify.cmd` and requests to serve all happen between runs. The `rapl` meter reads `energy_uj`
through sysfs, one `pread` per zone on files opened once per queue, not through perf, which keeps
a periodic timer ([RESEARCH.md](RESEARCH.md#2-linux-mechanisms)).

A queue's workload runs in its own process group with `PR_SET_PDEATHSIG(SIGKILL)`, so it dies
with the runner; a stop signal only makes the runner send `SIGTERM` to that group. The window
ends when the main process exits; what happens to what it left behind is in
[minion/README.md](../minion/README.md#queue). The [tighter window](#tighter-window) waits for
them instead.

### Settings and root

A setting is a plugin over sysfs files, one per object (`policy0`, `cpu3/state2`). Objects are
found under the sysfs root, so a name from the caller is only a key and never becomes a path.
Values are per object, so CPUs that differ are restored exactly, where `slot.sh` has to refuse.
Settings are written in a fixed order, a setting before those whose valid range it defines, and
restored in the same order, not the reverse, which breaks such dependencies
([RESEARCH.md](RESEARCH.md#2-linux-mechanisms)).

Only `minion serve` writes settings: one root process per queue, which restores when its pipe
closes, so also when the queue dies. That is GEOPM Service's rule (save before the first write,
restore when the client is gone) with a pipe for the session. Root never runs user code, reads
user files or kills: serve reads only its pipe, `/sys` and its journal. As root the binary refuses
the path options and the verbs `run`, `queue` and `reap`, and sudo allows only the root-owned
copies that `gru setup` installs ([details](../minion/README.md#root)).

### Journal

GEOPM Service, Slurm and tuned also restore originals, but keep them in memory or in `/run`
([RESEARCH.md](RESEARCH.md#1-tools)). minion keeps them in an on-disk journal, one fsynced line
per step, so they survive a crash. The first apply saves a full snapshot, once, so it also covers
settings changed only as a side effect. Restore keeps what someone else changed during the queue
and reports it, and restoring twice is harmless ([rules](../minion/README.md#journal)). Once
settings with side effects arrive (driver mode, SMT, turbo with frequency limits), a restore must
also mark its start, or a second restore after an interrupted one would take those side effects
for someone else's change.

### One queue at a time, without a service

A queue holds a blocking `flock` on `/run/lock/minion.lock` from start to end. flock fits
([RESEARCH.md](RESEARCH.md#2-linux-mechanisms)): it is freed when its holder dies, also by
`kill -9`, and a waiter sleeps in the kernel with no wakeups. When it is freed, all waiters wake
and one wins (not first come, first served). Any local user can take it and block queues while
they hold it. It replaces hand-made "wait for job X" tasks. The holder writes its queue id
(`turbo-cost-20261123T091200Z`) into the file, so a waiter can log whom it waits for and reap can
spare the holder's workloads ([details](../minion/README.md#queue)). A queue's serve takes the
journal lock only at its first request, which the queue sends once it holds the queue lock, so
serves follow the queues' order and can't deadlock.

There is no service: a gru job hosts the queue ([DESIGN.md](DESIGN.md#how-they-connect)), or
`setsid` or tmux without gru. A job started with setsid stays in the login session's cgroup, so
on a host with logind's `KillUserProcesses=yes` it dies when gru's ssh connection closes; such a
host needs `KillExcludeUsers=` for the user (`gru setup` warns). A queue that dies, also by
`kill -9`, closes its pipe, so its serve restores; the gru job's `always` hook runs `minion
reap` for what its workloads left. For `gru kill`, see
[minion/README.md](../minion/README.md#with-gru). The other cases:

- **serve itself is killed** (by root): the journal stays open. The next queue restores it at
  its start, or `gru minion restore --host X` does; a queue that changes nothing stops until
  then, since it can't read the journal but sees that it is open.
- **Restore fails**: the journal stays open and `restore_failed` is sent. Every later queue tries
  again at its start and runs nothing until it works. No verb gives up on a journal yet.
- **Reboot**: minion writes only sysfs, so the reboot resets every setting to its boot value.
  There is no boot check yet: the next queue restores the old journal like any leftover, writing
  the saved value back where the boot value equals the applied one and keeping the rest, reported
  as changed by someone else. `gru ps` shows the dead job as `lost`.
- **Shutdown** should go like `gru kill` if the restore fits systemd's stop timeout (not tested).

### Queue

Each round runs every (profile, workload) pair once, in a new order drawn from the seed
(randomized interleaved rounds), because fixed and blocked orders bias results
([RESEARCH.md](RESEARCH.md#33-methodology)). Results go to a new directory per queue and are never
overwritten. The runner decides when to notify and a command decides how; with gru the command is
gru's agent, so secrets stay in gru's notify file. Root never notifies.

## Planned

Nothing here is built. Each part names its [plan](#plan) phase. The facts behind these parts are
in [RESEARCH.md](RESEARCH.md#2-linux-mechanisms).

### Tighter window

Phase 5.
- The runner becomes a child subreaper, so the window ends when no workload process is left;
  from [isolation](#isolation) on, when the run's cgroup empties, and `cgroup.kill` ends the rest.
- A per-workload `timeout_s` and a grace period: overruns and a workload that ignores `SIGTERM`
  are killed, and the run is flagged.
- Phase marks: the workload writes a line to a FIFO at `$MINION_MARK` and the runner reads the
  meters. The runner then waits in one `poll()` on a `signalfd` and the FIFO (opened `O_RDWR`, so
  a closed writer can't make it spin).
- The runner sits on a housekeeping CPU with a large timer slack; the workload resets its slack
  to 50 µs before `exec`. Before the start read come `sync` (so writes from between runs are not
  flushed inside the window) and the settle. Helpers run in their own process groups, killed
  before a window starts.

### More settings

Phase 5. Driver mode, SMT, EPB, perf limits, uncore frequency, RAPL limits and group read of
`energy_uj`, kernel knobs (NMI watchdog, KSM, THP, `vm/stat_interval`, NUMA balancing),
`AllowedCPUs` of top-level units, IRQ affinity, movable kernel threads, the workqueue mask, and
guards (irqbalance, tuned, thermald, listed timers).
- **Order.** Guards are stopped first and released last; only those that were active start again.
  Everything else goes dependencies first, both ways: driver mode first, the governor before EPP,
  turbo before frequency and perf limits, SMT before IRQs and kernel threads. After a dependency is
  written, its dependents are written again. A min/max pair keeps min ≤ max at every step.
- **Reboot.** State that a reboot rebuilds (`AllowedCPUs`, IRQ affinity, kernel threads, guards)
  is never put back from an old boot. `AllowedCPUs` is undone with `systemctl set-property
  --runtime`, never `systemctl revert`, which also deletes the admins' drop-ins.
- **Refusals.** Known kernel refusals (managed IRQs, BIOS-locked RAPL limits, turbo disabled in
  BIOS) are recorded and fail the apply only if the profile needs the value. Group read of RAPL
  reopens the CVE-2020-8694 side channel to that group; it is recorded.
- **Root checks.** Only CPUs that exist, values in the hardware's range, units in the root-owned
  `/etc/minion/units`, never every housekeeping CPU offline, and no restriction of other users on
  a machine that has not opted in.

### Host profile and boot-only checks

Phase 5. One host profile per machine type, in the repo, holds the factory value of every
setting, the housekeeping and experiment CPUs, a maximum power per energy zone, the boot-only
expectations and the units that may be paused. A setting a profile doesn't mention is held at its
factory value, as `host_baseline.sh` and `slot.sh` do today. Symbolic values (`hw_max`,
`deepest`) are resolved and recorded. Boot-only settings (kernel command line, isolated CPUs,
kernel, microcode and BIOS versions, idle states, HWP, CPU count and frequency range) are read,
never written; a mismatch refuses the queue before anything is written.

### Isolation

Phase 5.
- `AllowedCPUs` of every top-level unit, movable IRQs, movable kernel threads and the workqueue
  mask go to the housekeeping CPUs, and listed units are paused, all journaled. Managed IRQs,
  per-CPU kernel threads, SMIs and other users are only detected and recorded.
- A delegated cgroup without a unit file (not tried): a `scope` verb moves the runner into a
  transient scope with `Delegate=yes`, split into `hk/` (the runner) and `exp/` (the workload,
  started there with `CLONE_INTO_CGROUP`). `reap` then kills a dead queue's scope. No cpuset
  partitions: they conflict with systemd.
- The lowest CPU of each package stays housekeeping, since on Cascade Lake every RAPL read
  interrupts it. The runner sits on a housekeeping CPU that is not CPU 0 and whose SMT sibling is
  housekeeping too; RQ2 tests the alternatives.
- Isolation keeps other work off the experiment cores, not out of the package counter, so pause
  what can be paused and record the rest. Before each run the runner checks logins, load and
  other cgroups' CPU time: `others: wait` (default), `refuse` or `mark`.

### Meters and noise record

Phase 5. Meters are read only at run edges and phase marks, kept in memory and written after the
window.
- `time` adds `CLOCK_BOOTTIME` (a jump against the monotonic clock shows a suspend) and the NTP
  offsets.
- `rapl` reads each zone from a slow timer, often enough to correct runs of any length for wraps
  (today a run that may have wrapped a counter twice gets no joules for it), and sums one zone per
  package, since zones overlap.
- `nvml`: total energy per GPU, one session per queue through `dlopen`.
- `wall`: a sampler on another machine asks the BMC once a second for the whole queue; edges are
  matched afterwards. BMC power is coarse, so it suits long runs only.
- `perf`: hardware counters, counted and never sampled, one group read per edge.
- Time series, off by default; RQ2 measures their cost.
- More of the noise record, read before the start read and after the end read: foreign processes;
  per CPU the SMI count, APERF/MPERF, throttle counters and temperatures, through an `msr` verb
  that reads only those MSRs and `HWP_REQUEST`; new kernel log lines; the end-of-run verify.
- `host.json` per queue: CPU model and topology, kernel, cmdline, microcode, BIOS, energy zones
  and backend (MSR, TPMI, modeled), NVML and driver versions.

### Queue features

Phase 5.
- A thermal gate per profile (our own rule), replacing the queue's one baseline: the first run of a
  profile waits until package temperatures change less than 0.5 °C/min and records them as its
  baseline; later runs wait until every package is within `band_c` of it, up to a timeout.
- A canary workload every N rounds that pauses the queue when its energy drifts, as the Green
  Metrics Tool does.
- A drift policy: `stop` (default) or `reapply` (what the queue does now).
- `minion queue --resume <dir>` reruns the aborted run as a new attempt and continues.
- Adaptive stopping on the confidence interval of each pair's median, between a minimum and a
  maximum of reps; a `blocked` order, only to measure order effects.
- More events (`run_aborted`, `others_detected`, `canary_drift`, `rebooted`,
  the machine-lock events) and a heartbeat ping, so a service such as Healthchecks.io alerts when
  a machine goes quiet.
- `abandon`, for an admin to close a journal that can't be restored.
- On shared machines, the journal records the caller's uid (`SUDO_UID`) and the boot, and serve
  refuses another user's journal (admins excepted).

### Machine lock (opt-in)

Phase 6. While a queue runs, new ssh logins of other users can be refused, if the admins enable it
in `/etc/minion/config` with a maximum duration and an admin group. Two lines go once into the
account part of `/etc/pam.d/sshd`:

```
account [success=1 default=ignore] pam_access.so accessfile=/run/minion/allow.conf
account [auth_err=bad default=ignore] pam_nologin.so file=/run/minion/nologin
```

- **Lock**: write `allow.conf` (`+:root <holder> (<admin group>):ALL`, then `-:ALL:ALL`) through a
  temporary file and a rename, read it back, then create `nologin` with the message ("locked for
  energy experiments by <holder> until 2026-11-23T18:00:00Z"). **Unlock** removes `nologin`.
- **Fails open**: without `nologin` nothing is refused, and `default=ignore` skips a module that
  fails to load or aborts on a missing file. A broken `allow.conf` lets everyone in, hence the
  read-back.
- **Never locked out**: root, the holder and the admin group, who may also always unlock. A
  reboot clears both files.
- **Hard expiry**: locking also starts `systemd-run --on-active=<duration> minion unlock`, so the
  lock lifts even if the runner is dead.
- **Provisional until checked**: `gru lock-check` opens a fresh login (`ssh -o ControlPath=none`)
  and then runs `minion confirm`, or `unlock` if the login failed. Without a confirmation within
  5 minutes the runner lifts the lock and goes on unlocked.
- **Cannot block** (detected, recorded, then `others` applies): sessions opened before the lock,
  running processes, cron and at jobs, lingering users' services, other network services.

### Several machines

Phase 7. One experiment can span hosts, like the two-machine A/B tests.
- A leader drives followers over ssh, with no new sockets or ports: one session per follower for
  the whole queue (`ssh -T -o ControlPath=none <host> minion hold`, no pty), one JSON line per
  command, each with a reply timeout.
- Each host keeps its own journal and lock; the leader takes all locks in host-name order, so two
  multi-host queues can't deadlock.
- A follower holds its state only while the leader's session lives: `hold` restores and exits at
  EOF on stdin, which sshd gives a non-pty command when the connection ends, or when a lease runs
  out: a follower is a serve whose pipe is the ssh session. If a follower's queue side dies, the
  leader runs `minion reap; minion restore` there.
- At each edge the leader asks every host to read its meters and return them with its
  `CLOCK_REALTIME`, so skew and clock offsets are recorded. Energy is compared only on the same
  machine.

### GPUs and devices

Phase 8. Devices are setting objects like CPUs (`gpu0`). First GPUs, through NVML as root: power
limit, locked clocks and persistence mode, with persistence applied first and restored last, since
without it the power limit is lost when the last client exits. No call to read the locked clocks
was found, so restore unlocks them: exact only if they were unlocked before. ECC and MIG modes
are boot-only: detected and refused on mismatch. Other processes on the GPU, clock-event reasons
and temperature go to the noise record.

### Boot unit

Phase 9. A root service before user sessions that never fails the boot, loads the uncore, RAPL
and coretemp drivers, and restores an open journal only if kernel, cmdline, microcode and BIOS
match it.

## Plan

Phases 2 to 4 are tested on a fake sysfs only; checking them on a real machine comes next.

| Phase | What | Done when |
|---|---|---|
| 1 | gru changes (#3, #6, #7, #8, #10): a state dir per host, unique UTC job ids, an agent file per build, quiet attach, provenance, notify for tasks | done |
| 2 | state engine (#2): the journal, `snapshot`, apply and `restore`; `minion serve` and `gru setup` since | done |
| 3 | `minion run` (#4): `time` and `rapl` at the edges and a drift flag | done |
| 4 | the queue and the gru connection (#9, #11), with `make e2e` (#12) | done |
| 5 | measurement quality: the [tighter window](#tighter-window), [more settings](#more-settings), the [host profile](#host-profile-and-boot-only-checks), [isolation](#isolation), [meters and noise record](#meters-and-noise-record) and [queue features](#queue-features). Started: the noise record (#23), `cpu.freq` and `cpu.epp` (#24), the `cmd` meter (#25), the thermal wait (#27). | 2 profiles × 2 workloads × 10 reps run unattended with full provenance, `strace` shows no runner syscall inside a window but the wakeup that ends it, and the RQ2 overhead experiment has run; `host_baseline.sh` and `slot.sh` retire |
| 6 | the [machine lock](#machine-lock-opt-in) | another account is refused and sees the message while the holder, root and an admin get in; the lock lifts at the end, after `kill -9` of the runner, at expiry and after a reboot; a broken PAM module or `allow.conf` leaves logins open |
| 7 | [several machines](#several-machines) | a two-host queue runs with shared edges, and killing the leader or cutting its network leaves the follower restored within one lease |
| 8 | [GPUs and devices](#gpus-and-devices) | GPU snapshots before and after a queue match, and ECC or MIG mismatches are refused |
| 9 | the [boot unit](#boot-unit) | snapshots match after a reboot mid-queue |
| 10 | RQ3 analysis: the A/A and injected modes, and the Python analysis over `run.json` | the detection rate against the number of runs is measured for two or more injected sizes, with and without the noise record |

## Improvements, ranked

What the tools and the measurement survey in [RESEARCH.md](RESEARCH.md) ask of minion (numbers and
sources there), ranked by strength of evidence and benefit to the research questions per unit of
cost. Cost is in Rust lines in minion (analysis in Python); *here* means testable on the fake
sysfs, *machine* needs a real host.

Already built from the survey: the journal with restore when the client dies, and compare and
restore (GEOPM Service, Slurm and tuned each have part of it); randomized interleaved rounds with
seed, round and position saved, since order bias is large and common on bare metal (Abedi 2017,
Duplyakin 2023); events named after Slurm's mail types, with any push sink (such as Pushover)
through `notify.cmd` or gru's `run` hooks.

1. **Check every energy reading.** Tools still lose wraps, count zones twice and use wrong units,
   and the kernel scaled Sapphire Rapids DRAM about 4× wrong until 2022
   ([3.1](RESEARCH.md#31-meters), [3.4](RESEARCH.md#34-tools)). In `rapl`, flag a zone whose
   counter didn't move in a run of 10 ms or more (stuck, or a 1 J unit), whose mean power is above
   its highest power limit, or whose delta equals another package's (one counter read twice); the
   raw counters and range are already stored. About 40 lines, low risk; here.
2. **A/A and injected-regression modes, and a false-positive rate per host.** They give the
   false-positive and detection rates against the number of runs (RQ3), which nobody reports for
   energy (Laaber 2019, Bulej 2020, Fleming 2023; the gap is stated by EnergyTrackr); without an
   effect-size floor, 49 of 60 planted null changes were flagged ([3.3](RESEARCH.md#33-methodology)).
   Nothing to build for A/A: the same command under two workload names is an A/A pair. The
   analysis applies the test used for real comparisons (a ratio CI bootstrapped over rounds, an
   effect-size floor, Benjamini-Hochberg across comparisons) and reports the false-positive rate;
   A/A queues with `settle_s` 0 and 60 also pick each host's settle, since a 60 s rest did not help
   on the one server measured. About 100 lines of analysis; the rate needs a machine. Phase 10;
   this brings its A/A half forward.
3. **Order-independence check.** Whether a machine carries state between runs (Duplyakin 2023,
   Maricq 2018): run some rounds in a fixed order, then test fixed against random (Kruskal-Wallis)
   and run order against shuffled (Mann-Whitney). Analysis on top of `plan.json`; here. New.
4. **Record what sets a meter's accuracy in `host.json`.** RAPL energy filtering adds 5-15% noise
   at 100 ms and stays on until reboot once SGX or TDX is enabled; kernels changed units and listed
   different AMD zones in powercap and perf until 6.6; mitigations changed run time by 2-11%;
   AMD's energy is modeled (Schöne 2021) ([3.1](RESEARCH.md#31-meters),
   [3.2](RESEARCH.md#32-sources-of-variation)). Add the filtering bit (MSR 0xBC bit 0, through the
   planned `msr` verb, as the Green Metrics Tool does), the zones listed by powercap and by perf,
   the zone backend (MSR, TPMI) and whether energy is modeled, `cpu/vulnerabilities/*`, and the
   wall meter's model and accuracy; with filtering on or unknown, flag runs shorter than 1 s. About
   40 lines, low risk (the MSR needs root); here, except the MSR. In design
   ([meters](#meters-and-noise-record)), new fields.
5. **A warm-up round, a thermal gate and a trend check.** Power drifts for about 7 min at constant
   load, labs pre-heat for 15 min, many benchmarks never reach a steady state, and fixed warm-up
   counts are wrong in most cases ([3.2](RESEARCH.md#32-sources-of-variation),
   [3.3](RESEARCH.md#33-methodology)); Krun and the Green Metrics Tool gate on temperature.
   `warmup_rounds` (default 1): shuffled rounds before the first, marked `warmup` in run.json and
   left out of the analysis; the [per-profile thermal gate](#queue-features), and, like the Green
   Metrics Tool, a warm-up for a cold machine. The analysis runs a change-point test (binary
   segmentation or PELT) on each pair's runs in run order and flags a shift. About 30 lines plus 50
   of analysis, low risk; here. The basic thermal wait is built.
6. **Validate RAPL against the BMC and a wall meter on each host.** Wall against RAPL has a slope
   of about 1.17, what RAPL misses grows with load, AMD's RAPL is a model, and a BMC reported 14 W
   steps over 60 s windows ([3.1](RESEARCH.md#31-meters)); BMC data is best kept off the host and
   matched by time window (Kwollect). A calibration queue in the repo (idle, one core, one socket,
   all cores, a memory-bound and an AVX workload, 5 min each, well above the BMC window) with the
   planned `wall` sampler; the analysis fits wall against RAPL per host, trims each edge by the
   meters' lag and stores slope, offset and residual in the host profile. A queue file and about 80
   lines of analysis once `wall` exists; a machine with a reachable BMC ([open question
   1](#open-questions)).
7. **Idle as a workload in every round, never one subtraction.** Idle energy varies more than a
   light job's, and the first active core adds 22-81 W, so energy minus idle × time is not the
   run's own energy ([3.3](RESEARCH.md#33-methodology)). Nothing to build (`"idle": {"cmd":
   ["sleep", "60"]}` works today): the analysis reports raw energy and, separately, energy minus
   the same round's idle, with its CI. About 30 lines of analysis; here.
8. **Adaptive repetitions that stop on each pair's own precision.** The runs needed differ more
   than 10× between setups (Hoefler 2015, Maricq 2018, Phoronix Test Suite); stopping when a
   comparison looks significant raises false positives about 5×; a relative CI-width rule matched
   full runs best ([3.3](RESEARCH.md#33-methodology)). `reps` may be
   `{"min": 6, "max": 60, "ci_rel": 0.01}`: after each round, stop once every pair's median CI
   half-width is below `ci_rel` of its median, or at `max`; the rule never looks at differences
   between pairs, and plan.json grows a round at a time. About 100 lines, medium risk (plan and
   resume format); here, with fake workloads of known spread. In design ([queue
   features](#queue-features)); adds the precision-only rule.
9. **A canary workload that pauses the queue on drift**, for slow machine drift that statistics
   can't fix (Green Metrics Tool, Daly 2020). In design ([queue features](#queue-features)).
10. **Checks and quality flags kept with the run, never silent drops.** A `verify` command and
    start/end checks from the Green Metrics Tool's list (timers, cron, NTP, swap, RAPL limits,
    suspend and steal time; tuned verifies too), where a jump of `CLOCK_BOOTTIME` against
    `CLOCK_MONOTONIC` is a cheap suspend check; a median/MAD outlier flag, a first-run warning
    (hyperfine) and new `dmesg` lines (Krun). In design (flags, verify); adds the two rules.
11. **Temperatures and fans per run, from the host and the BMC.** Server power rose 16% with CPU
    temperature and 5.6% with warmer rack neighbours, fan control alone moved energy 8.3%, and
    SPECpower requires the inlet temperature ([3.2](RESEARCH.md#32-sources-of-variation),
    [3.3](RESEARCH.md#33-methodology)). The thermal wait already records hwmon temperatures at
    each start; add the BMC's inlet temperature and fan speeds to the `wall` series, matched to
    each run, as covariates for item 13. About 30 lines in the sampler, low risk; a machine with a
    BMC.
12. **Placement in the profile, checked after apply.** One socket instead of two cut the spread
    30× at half load, NUMA placement moved energy 8-13% on Zen 1, and offline SMT siblings kept a
    Rome package out of deep idle (+81 W) ([3.2](RESEARCH.md#32-sources-of-variation)); BenchExec
    measured +22% CPU time from contention and +6.8% from remote memory, on old hardware
    ([1](RESEARCH.md#1-tools)). The workload's CPU list becomes part of the profile and of
    run.json, with host profiles that pick whole physical cores on one socket with local memory;
    after an apply that changes SMT or idle states, a 10 s idle RAPL read is compared with one
    taken before, and a jump is flagged. About 40 lines on top of [isolation](#isolation), medium
    risk; the check here, its numbers on an AMD and an Intel machine.
13. **Covariates named before the RQ3 runs.** No benchmarking work was found that adjusts energy
    with noise covariates; CUPED cuts variance with a covariate measured before the experiment
    (Deng 2013), and regression adjustment with treatment × covariate terms cannot lose asymptotic
    precision ([3.3](RESEARCH.md#33-methodology)). Fix the covariates in advance and use only
    values the change can't affect: the start temperatures (thermal wait), logins and load at the
    start (noise record), and other cgroups' CPU time and interrupts over a window before the
    start read (new: the noise record has only their in-run changes). Report adjusted and
    unadjusted results, judged by item 2 and the injected mode. Analysis, plus the pre-run window;
    here on synthetic data, the effect on a machine. In design ([RQ3](#research-questions)).
14. **Measure and publish the runner's own overhead**, with a calibrate script, as the Green
    Metrics Tool did. In design ([RQ2](#research-questions)).
15. **GPU energy from counters, with a minimum run length.** NVML power covers 25 ms of every
    100 ms on A100 and H100 and integrating it errs by up to 70%; the energy counter reads 0 J for
    windows shorter than its update; old sensors lag by 0.84 s ([3.1](RESEARCH.md#31-meters),
    [3.4](RESEARCH.md#34-tools)). The planned `nvml` meter reads only the energy counter, records
    the driver version, flags runs shorter than 5 s (the paper's minimum for power readings, until
    the counter's own update is measured) and settles between GPU runs. About 10 lines on top of
    phase 8; a GPU machine.
16. **Offline change-point detection across commits**, deterministic and with a minimum history
    (Daly 2020, Fleming 2023). Later, for continuous use.

Avoid: resetting to fixed defaults (pyperf, LIKWID, EnergyTrackr); one-way machine preparation
(the Green Metrics Tool's NOP Linux); periodic samplers that visit every CPU during runs
(turbostat); boot options such as `idle=poll` that change idle energy; the osnoise workload during
a measured run.

## Research questions

- **RQ1: which settings matter, and what they cost.** One profile per setting changed from
  `factory`, plus `stable`, interleaved, with 30 or more runs each. Per profile, report the
  variation (coefficient of variation, nonparametric confidence interval of the median) and the
  cost (energy and time against `factory`). The noise record separates variance due to the
  setting from variance due to interference.
- **RQ2: exact and safe restore, without disturbance.** *Restore:* `minion snapshot` before a
  queue and after its restore must match, also with `kill -9` at random points in apply, run and
  restore, and (with the boot unit) a reboot mid-queue. *Overhead:* fixed workloads and a long idle
  window with (a) no tool, (b) the runner, (c) today's Python sampler plus a per-second `ipmitool`
  fork, (d) time-series mode at a few rates, (e) an idle `hold` session on both hosts. For (b),
  the runner on CPU 0, on a package's lowest CPU (where RAPL is read), on another housekeeping CPU,
  and on an experiment CPU's SMT sibling, with osnoise on and off. Compare energy (RAPL and wall),
  run time, IPIs and timer interrupts on experiment CPUs, and idle-state residency.
- **RQ3: smallest detectable regression.** Two queue modes. *A/A* runs the same code on both
  sides and gives the false-positive rate. *Injected* adds a calibrated extra cost (extra work, a
  frequency cap) and gives the detection rate. Measure both against the number of runs, with and
  without the noise record: flag noisy runs, or adjust with covariates measured before the run,
  which the change can't affect. [RESEARCH.md](RESEARCH.md#conclusions) found no source that
  reports this for energy on one machine.

## Open questions

1. Is each BMC reachable from another machine, and with which credentials?
2. Until the boot unit, a reboot resets values set by hand before a queue (the next queue puts
   back only some of them). With it, the journal restores them, other users' included (the task
   asks for this), only on the same kernel and firmware. Confirm, since a user may expect a reboot
   to clear them.
3. Which timers and services may be paused on the shared machines?
4. Restricting other users (the lock, `AllowedCPUs` on shared slices, RAPL limits) needs root and
   the agreement of the admins and those users. Which machines opt in, with which maximum and
   admin group? On shared machines, keep it opt-in and time-limited.
5. How do we enable the `perf` meter on the Ubuntu hosts: an admin sysctl, or `CAP_PERFMON` for
   the runner?
6. Isolation needs a delegated cgroup; without unit files, the plan is a transient scope. Is a
   transient (not installed) unit acceptable?
