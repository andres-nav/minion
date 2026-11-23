# minion — design

minion is the runner on the measured host. This file gives the reasons for what it does and the
design of what is planned; nothing under [Planned](#planned) is built. Usage and formats (queue
file, journal, results, events): [minion/README.md](../minion/README.md). How it works with gru:
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

## What is built

- Settings: `cpu.governor`, `cpu.epp`, `cpu.turbo`, `cpu.freq` (min and max) and
  `cpu.idle.disable`.
- Verbs: `snapshot`, `apply`, `restore`, `status`, `run`, `queue` and `reap`.
- Meters: `time`, `rapl` and `cmd`.
- Queue: randomized interleaved rounds under the queue lock, with reap, events and notify.
- Around each run: the settings snapshot and a basic noise record (interrupts, softirqs, context
  switches, other cgroups' CPU time, logins and load).
- Restore: from the fsynced journal, keeping what someone else changed.
- Between runs: the settle.

`make e2e` runs queues through gru on a fake sysfs; real sysfs writes, RAPL, sudo and ssh are not
tested yet.

### Stay out of the measured window

A measured window runs from the meter reads right before the workload starts to those right after
its main process exits. Apart from starting the workload, the runner only waits in `waitpid`: it
does not log, write, send or fork. Settings snapshots, the settle, `run.json`, events,
`notify.cmd` and sudo calls all happen between runs. The `rapl` meter reads `energy_uj` through
sysfs, one `pread` per zone on files opened once per queue, not through perf, which keeps a
periodic timer ([RESEARCH.md](RESEARCH.md#2-linux-mechanisms)).

A queue's workload runs in its own process group with `PR_SET_PDEATHSIG(SIGKILL)`, so it dies
with the runner; a stop signal only makes the runner send `SIGTERM` to that group. The window
ends when the main process exits; what happens to what it left behind is in
[minion/README.md](../minion/README.md#queue). The [tighter window](#tighter-window) waits for
them instead.

### Settings and privileged verbs

A setting is a plugin over sysfs files, one per object (`policy0`, `cpu3/state2`). Objects are
found under the sysfs root, so a name from the caller is only a key and never becomes a path.
Values are per object, so CPUs that differ are restored exactly, where `slot.sh` has to refuse.
Settings are written in a fixed order, a setting before those whose valid range it defines, and
restored in the same order, not the reverse, which breaks such dependencies
([RESEARCH.md](RESEARCH.md#2-linux-mechanisms)).

`apply` and `restore` are the only writers. A queue runs them as `sudo -n minion apply --held`
and `sudo -n minion restore --held`, through one sudoers rule, much like GEOPM Service (save
before the first write, restore when the client is gone). Root never runs user code or reads the
queue file: the queue pipes what a verb needs on stdin. As root the binary refuses the path
options (`--sysfs`, `--state-dir`, `--lock`, `--out`) and the verbs `run`, `queue` and `reap`.

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
(`turbo-cost-20261123T091200Z`) into the file, so a waiter can log whom it waits for, and the
privileged verbs can refuse a sudo child that outlived its queue
([details](../minion/README.md#queue)).

There is no service: a gru job hosts the queue ([DESIGN.md](DESIGN.md#how-they-connect)), or
`setsid` or tmux without gru. A job started with setsid stays in the login session's cgroup, so
on a host with logind's `KillUserProcesses=yes` it dies when gru's ssh connection closes; such a
host needs `KillExcludeUsers=` for the user. Two paths clean up after a crash: the next queue
reaps and restores a leftover at its start, and the gru job's `always` hooks run `minion reap`
and `sudo -n minion restore`. For `gru kill` and `kill -9`, see
[minion/README.md](../minion/README.md#with-gru). The other cases:

- **The whole job is killed, or the runner ran without gru**: no hook runs. The journal stays
  open until the next queue reaps and restores, or someone runs `minion reap; sudo -n minion
  restore`.
- **Restore fails**: the journal stays open and `restore_failed` is sent. Every later queue tries
  again at its start and runs nothing until it works. No verb gives up on a journal yet.
- **Reboot**: minion writes only sysfs, so the reboot resets every setting to its boot value.
  There is no boot check yet: the next queue restores the old journal like any leftover, writing
  the saved value back where the boot value equals the applied one and keeping the rest, reported
  as changed by someone else. `gru ps` keeps showing the dead job as running.
- **Shutdown** should go like `gru kill` if the restore fits systemd's stop timeout (not tested).

### Queue

Each round runs every (profile, workload) pair once, in a new order drawn from the seed
(randomized interleaved rounds), because fixed and blocked orders bias results
([RESEARCH.md](RESEARCH.md#3-methodology)). Results go to a new directory per queue and are never
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
  to 50 µs before `exec`. Before the start read come the noise record, `sync` (so writes from
  between runs are not flushed inside the window) and the settle. Helpers run in their own process
  groups, killed before a window starts.

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
  partitions: they conflict with systemd, and 5.15 has no `isolated` partitions.
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
- A thermal gate per profile (our own rule): the first run of a profile waits until package
  temperatures change less than 0.5 °C/min and records them as its baseline; later runs wait until
  every package is within `band_c` of it, up to a timeout, then run flagged.
- A canary workload every N rounds that pauses the queue when its energy drifts, as the Green
  Metrics Tool does.
- A drift policy: `stop` (default) or `reapply` (what the queue does now).
- `minion queue --resume <dir>` reruns the aborted run as a new attempt and continues.
- Adaptive stopping on the confidence interval of each pair's median, between a minimum and a
  maximum of reps; a `blocked` order, only to measure order effects.
- More events (`run_aborted`, `others_detected`, `thermal_timeout`, `canary_drift`, `rebooted`,
  the machine-lock events) and a heartbeat ping, so a service such as Healthchecks.io alerts when
  a machine goes quiet.
- `abandon`, for an admin to close a journal that can't be restored.
- On shared machines, the journal records the caller's uid and the boot, and `--held` checks
  `SUDO_UID` against it (admins excepted), since anyone can read the queue id.

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
  out. If a follower dies, the leader runs `minion reap; sudo -n minion restore` there.
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

1. **gru changes**: done (#3, #6, #7, #8, #10; [list](DESIGN.md#status-and-plan)).
2. **State engine**: done (#2): the journal, `snapshot`, `apply`, `restore`, `status`, three
   settings.
3. **`minion run`**: done (#4): `time` and `rapl` at the edges and a drift flag.
4. **Queue and the gru connection**: done (#9, #11), with `make e2e` (#12).
5. **Measurement quality**: the [tighter window](#tighter-window), [more
   settings](#more-settings), the [host profile](#host-profile-and-boot-only-checks),
   [isolation](#isolation), [meters and noise record](#meters-and-noise-record) and [queue
   features](#queue-features). Done when 2 profiles × 2 workloads × 10 reps run unattended with
   full provenance, `strace` shows no runner syscall inside a window but the wakeup that ends it,
   and the RQ2 overhead experiment has run; `host_baseline.sh` and `slot.sh` retire.
6. **[Machine lock](#machine-lock-opt-in)**. Done when another account is refused and sees the
   message while the holder, root and an admin get in; the lock lifts at the end, after `kill -9`
   of the runner, at expiry and after a reboot; and a broken PAM module or `allow.conf` leaves
   logins open.
7. **[Several machines](#several-machines)**. Done when a two-host queue runs with shared edges,
   and killing the leader or cutting its network leaves the follower restored within one lease.
8. **[GPUs and devices](#gpus-and-devices)**. Done when GPU snapshots before and after a queue
   match and ECC or MIG mismatches are refused.
9. **[Boot unit](#boot-unit)**. Done when snapshots match after a reboot mid-queue.
10. **RQ3 analysis**: the A/A and injected modes, and the Python analysis over `run.json`. Done
    when the detection rate against the number of runs is measured for two or more injected
    sizes, with and without the noise record.

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
5. The Cascade Lake host runs kernel 5.15: no osnoise no-workload mode, and frequency reads
   interrupt the CPU. Move it to Ubuntu 24.04 (6.8), or only record the gap?
6. How do we enable the `perf` meter on the Ubuntu hosts: an admin sysctl, or `CAP_PERFMON` for
   the runner?
7. Isolation needs a delegated cgroup; without unit files, the plan is a transient scope. Is a
   transient (not installed) unit acceptable?
