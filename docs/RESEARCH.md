# minion — research

What existing tools and Linux mechanisms offer for the design in [MINION.md](MINION.md), and
which features are worth adding. Searched 2026-11-23; tool docs and source were read from each
project's default branch on that date and may change.

Evidence tags: **measured** (real hardware), **trace** (analysis of recorded data), **simulated**,
**claimed** (stated without data), **source** (read in code or docs), **unverified** (no source
found or not read). Kernel facts are from upstream tags v6.8 and v5.15; Ubuntu kernels carry
backports, so each must still be checked on the host.

## Conclusions

- **No tool does the whole job.** Restoring the *original* values exists in GEOPM Service, Slurm
  and tuned, but all three keep the originals in memory or in `/run`, so none restores after a
  reboot. Most tuning tools (pyperf, LIKWID, cpupower, EnergyTrackr, partly temci) "reset" to fixed
  defaults. Only EnergyTrackr and temci shuffle the run order. The Green Metrics Tool has the best
  checks, queue and notifications but sets almost nothing. The on-disk journal and compare and
  restore are new, and so is recovery after a reboot, which comes with a later boot unit.
- **No source measures the smallest detectable energy regression on one machine**, or how many runs
  it takes. EnergyTrackr's own preprint says it has no ground truth. A/A and injected-regression
  modes turn RQ3 into a number per machine and workload.
- **Order and repetition need care on bare metal** (Duplyakin 2023, Maricq 2018; numbers in
  [section 3](#3-methodology)).
- **Linux facts that changed the design** (details in [section 2](#2-linux-mechanisms)):
  - restore order is "dependencies first", not reverse;
  - cpuset partitions conflict with systemd and are missing on 5.15;
  - `systemctl revert` also deletes the admins' settings;
  - RAPL reads on Cascade Lake interrupt the package's lowest CPU;
  - osnoise has a no-workload mode only from 6.8 on;
  - a missing `pam_access` file aborts PAM;
  - without a service, `flock` (freed when the holder dies, no wakeups while waiting) and the EOF
    sshd gives a non-pty command when the connection ends suffice for waiting queues and
    follower hosts.
- The ranked feature list is in [section 4](#4-features-worth-adding-ranked).

## 1. Tools

| Tool | Sets state | Restores original | Survives crash / reboot | Energy | Queue | Notify | Random order |
|---|---|---|---|---|---|---|---|
| pyperf `system` | governor, turbo, min freq, IRQs | no, fixed values | – | – | – | – | – |
| Green Metrics Tool | checks only | – | job state in a DB | RAPL (MSR), AC meters, IPMI, NVML | yes | email | – |
| benchkit | freq, irqbalance, NUMA balancing | no | resumes from results | none found | campaigns | – | no |
| EnergyTrackr | governor, freq, uncore, turbo, boot options | fixed values | – | `perf stat` at run edges | batches | – | **yes** |
| BenchExec | per-run cgroups, cores | n/a | – | RAPL via cpu-energy-meter | yes | – | unverified |
| temci | governor, turbo, HT, ASLR, cpuset | partly | – | – | blocks | – | **yes** |
| tuned | many | **yes** | crash: unverified; reboot: no | – | – | D-Bus | n/a |
| Slurm | cpufreq per job step | **yes** | lock files only | RAPL, IPMI, XCC, GPU | yes | email, `strigger` | – |
| LIKWID | core/uncore freq, governor, turbo | fixed values | – | RAPL (MSR) | – | – | n/a |
| GEOPM Service | allowlisted controls | **yes**, also if the client dies | `/run`, lost at reboot | not reviewed | – | – | n/a |
| turbostat, rtla | read only | n/a | n/a | RAPL, APERF/MPERF / OS noise | – | – | n/a |
| Scaphandre, PowerAPI, Kepler, perun | – | n/a | n/a | RAPL (+NVML, BMC) sampling | – | – | – |
| hyperfine | – | n/a | n/a | – | – | – | no |
| Phoronix Test Suite | – | – | – | RAPL/hwmon, IPMI, WattsUp | Phoromatic | Pushover, email | unverified |

Notes, by tool (all **source** unless tagged):
- **pyperf**: `system reset` writes fixed values (powersave, `cpuinfo_min_freq`, turbo on, IRQs on
  all CPUs). Its per-run metadata (CPU config, frequency, temperature, load, boot time) is a
  ready-made noise-record list. [_system.py](https://raw.githubusercontent.com/psf/pyperf/main/pyperf/_system.py),
  [docs](https://pyperf.readthedocs.io/en/latest/system.html).
- **Green Metrics Tool**: checks timers, cron, watchdogs, NTP, swap, RAPL limits, frequency
  within ±10 MHz, SMT, turbo and SSH sessions before a run, and suspend and steal time after it.
  It has a temperature gate (sleep 60 s when hot, 5 min warm-up when cold, give up after 10
  tries) and a control workload every 6 h that pauses the queue when its std dev exceeds 1%. Its
  overhead was measured on 2 machines at 99 ms sampling: 0.4-0.9% under load (**measured**,
  2024-02-16). [checks](https://docs.green-coding.io/docs/cluster/machine-baseline-checks/),
  [accuracy control](https://docs.green-coding.io/docs/cluster/accuracy-control/),
  [overhead](https://docs.green-coding.io/docs/measuring/metric-providers/overhead-of-measurement-providers/).
- **benchkit** (Paolillo et al. 2026): `predverifydo` checks settings and fixes them; a campaign
  resumes by skipping records already in the results; no restore code. The paper text was not
  read (ACM returned 403). [repo](https://github.com/open-s4c/benchkit).
- **EnergyTrackr** (Bechet et al. 2026): shuffles (commit × repetition) inside a batch, reads
  `perf stat -e power/energy-pkg/` at run edges, and checks idle stability first (5 warm-up plus
  30 one-second samples, abort if a modified z-score exceeds 3.5). `revert-setup` restores fixed
  values. Its first setup adds `idle=poll`, which keeps cores busy-polling (our reading: that
  distorts idle energy). [repo](https://github.com/snail-unamur/energytrackr).
- **BenchExec** (Beyer et al. 2019): assigns whole physical cores on one socket with local memory.
  Measured on one 2× Xeon E5-2650 v2: contention +22% CPU time, remote memory +6.8% (**measured**,
  old hardware). [resources](https://github.com/sosy-lab/benchexec/blob/main/doc/resources.md).
- **temci**: restores the per-CPU governor and the HT cores it disabled, but always turns turbo on
  and sets ASLR to 1; shuffles blocks by default; the README says it is no longer maintained.
  [run_driver_plugin.py](https://raw.githubusercontent.com/parttimenerd/temci/master/temci/run/run_driver_plugin.py).
- **tuned**: saves each value before writing it and rolls back on a profile switch or
  `tuned-adm off`, but not at shutdown; the originals are "kept in RAM". `tuned-adm verify`
  compares live settings with the profile.
  [base.py](https://raw.githubusercontent.com/redhat-performance/tuned/master/tuned/plugins/base.py),
  [tuned-adm.8](https://raw.githubusercontent.com/redhat-performance/tuned/master/man/tuned-adm.8).
- **Slurm**: saves the original governor and frequencies once and never overwrites them. Per-CPU
  owner lock files make only the last job that changed a CPU restore it. Its RAPL plugin must poll
  within 300 s because the counters overflow. Mail types (BEGIN, END, FAIL, TIME_LIMIT_90, ...)
  make a ready event list. [srun](https://slurm.schedmd.com/srun.html),
  [acct_gather.conf](https://slurm.schedmd.com/acct_gather.conf.html),
  [sbatch](https://slurm.schedmd.com/sbatch.html).
- **GEOPM Service**: the closest model for the privileged verbs. When a client starts writing, it saves
  every supported control to `/run/geopm`; it restores them when the session ends, also when the
  client process dies; one writer at a time; allowlists in `/etc/geopm`.
  [service](https://geopm.github.io/service.html).
- **turbostat**: command mode reads counters at the start and end of a command. Periodic mode
  migrates itself to every CPU at each interval (our reading of the source), so it must not run
  during a measured run. [man page](https://man.archlinux.org/man/turbostat.8).
- **rtla osnoise**: by default it occupies the CPUs it measures; useful as a probe before a queue.
  [rtla-osnoise](https://docs.kernel.org/tools/rtla/rtla-osnoise.html).
- **LIKWID**: `-reset` sets fixed values (performance, full range, turbo off).
  [likwid-setFrequencies](https://github.com/RRZE-HPC/likwid/wiki/likwid-setFrequencies).
- **Scaphandre, PowerAPI (SmartWatts), Kepler, perun, CodeCarbon, PowerJoular**: periodic RAPL
  (and NVML, BMC) samplers that attribute power to processes or containers. They control no state,
  and no overhead number was found for them (**unverified**).
  [Kepler config](https://sustainable-computing.io/kepler/usage/configuration/),
  [perun](https://perun.readthedocs.io/en/latest/configuration.html).
- **hyperfine**: flags outliers with a modified z-score and warns when the *first* run is an
  outlier; it runs commands in input order. [repo](https://github.com/sharkdp/hyperfine).
- **Phoronix Test Suite**: adds runs while the std dev is above 3.5%, up to 2× the planned count
  (**claimed** defaults), and pushes results through Pushover.
  [docs](https://github.com/phoronix-test-suite/phoronix-test-suite/blob/master/documentation/phoronix-test-suite.md).
- **Testbeds**: CloudLab asks for versioned profiles that pin the image and hardware type.
  Grid'5000's Kwollect keeps meter data off the node and queries it by job and time window; its
  docs call BMC metrics "quite unreliable" (BMC every 5 s, wattmeters at 50 Hz). Chameleon
  collects IPMI power. [CloudLab](https://docs.cloudlab.us/repeatable-research.html),
  [Kwollect](https://www.grid5000.fr/w/Monitoring_Using_Kwollect),
  [Chameleon](https://chameleoncloud.readthedocs.io/en/latest/technical/power_monitoring/index.html).
- **Maricq et al. 2018 (CONFIRM)**: randomize order, use nonparametric CIs, base the number of
  runs on past data, and "don't assume independence: check". One benchmark changed later STREAM
  results until a reboot (**measured**, 835 CloudLab servers).
  [paper](https://www.usenix.org/system/files/osdi18-maricq.pdf).
- **EnergAt** (He, Friedman, Rekatsinas 2023) attributes energy to threads on shared hosts; only
  the abstract was read. [arXiv](https://arxiv.org/abs/2307.05647).
- **Healthchecks.io**: a dead-man's switch that alerts when a ping is late (period plus grace
  time). [docs](https://healthchecks.io/docs/).

## 2. Linux mechanisms

Kernel sources are cited by file at the [v6.8 tag](https://raw.githubusercontent.com/torvalds/linux/v6.8/Documentation/admin-guide/cgroup-v2.rst)
(the same URL prefix with another path); "root" means writing needs root. Nothing in this table
survives a reboot unless it says so.

| Mechanism | Path or command | Facts and gotchas (6.8; 5.15 where different) | Source |
|---|---|---|---|
| cpuset partitions | `cpuset.cpus.partition` | `isolated` since 6.1, `cpuset.cpus.exclusive` since 6.7, removal from unbound workqueues since 6.8. They don't move timers or RCU. A local partition must be a child of the root cgroup; otherwise ancestors need `cpus.exclusive`, i.e. writes into systemd's cgroups, which systemd forbids. 5.15: only `root`/`member`. | `cgroup-v2.rst`, `kernel/cgroup/cpuset.c`; [systemd delegation](https://systemd.io/CGROUP_DELEGATION/) |
| AllowedCPUs | `systemctl set-property --runtime <unit> AllowedCPUs=` | Writes `/run/systemd/system.control/<unit>.d/50-AllowedCPUs.conf`, gone at reboot. Undo by assigning empty or the old value; `systemctl revert` also removes the `/etc` drop-ins. Kernel threads stay in the root cgroup and are not moved. | [systemctl](https://www.freedesktop.org/software/systemd/man/255/systemctl.html), [resource-control](https://www.freedesktop.org/software/systemd/man/255/systemd.resource-control.html) |
| IRQ affinity | `/proc/irq/N/smp_affinity_list`, `effective_affinity_list`, `default_smp_affinity` | Managed IRQs refuse writes with `EIO`. When all of an IRQ's CPUs go offline, its affinity resets to all CPUs and does not come back. The default mask only affects IRQs set up later. | `kernel/irq/proc.c`, `kernel/irq/cpuhotplug.c` |
| irqbalance | `systemctl stop irqbalance` | Rewrites affinities every 10 s; bans only `isolated` and `nohz_full` CPUs, so it ignores cpuset partitions. | [irqbalance.1](https://raw.githubusercontent.com/Irqbalance/irqbalance/master/irqbalance.1) |
| boot-only flags | `/proc/cmdline`, `/sys/devices/system/cpu/{isolated,nohz_full}` | `isolated` shows `isolcpus` (domain) only, not cpuset partitions; `rcu_nocbs` has no sysfs file (boot log, `rcuo*` threads). | `drivers/base/cpu.c` |
| osnoise | `/sys/kernel/tracing/osnoise/options` | The default workload takes 100% of each measured CPU. `NO_OSNOISE_WORKLOAD` (6.8) starts no threads; the tracepoints still time IRQ, softirq, NMI and thread noise, but there is no HW-noise count. Overhead **unverified**. 5.15 has no options file. | [osnoise-tracer.rst](https://raw.githubusercontent.com/torvalds/linux/v6.8/Documentation/trace/osnoise-tracer.rst), `kernel/trace/trace_osnoise.c` |
| RAPL via sysfs | `/sys/class/powercap/*/energy_uj` | MSR backend: the read runs on the package's `lead_cpu` (normally its lowest CPU) via `rdmsrl_safe_on_cpu`, an IPI unless the reader is on that CPU. TPMI backend (Xeon 6): a plain MMIO read. The 6.8 MSR and perf tables list no Granite Rapids (through 6.11), so expect only TPMI there. | `drivers/powercap/intel_rapl_{common,msr,tpmi}.c`, `arch/x86/lib/msr-smp.c` |
| RAPL wrap | `max_energy_range_uj` | The hardware counter is 32-bit and sysfs wraps at the range. For example, 2^32 × 61 µJ ≈ 262 kJ, about 21 min at 205 W (computed). Intel's manual says about 60 s at high power (quoted by Raffin & Trystram 2025). | `intel_rapl_common.c` |
| RAPL via perf | `power/energy-pkg/` | System-wide only; the IPI is at event open; a pinned hrtimer (≈655 s at ESU 14) accumulates 64 bits, so no wrap. Needs `CAP_PERFMON`. | `arch/x86/events/rapl.c` |
| RAPL access | `energy_uj` mode | Root-only since commit 949dd01 (CVE-2020-8694, Platypus); group read reopens that side channel. | [commit](https://github.com/torvalds/linux/commit/949dd0104c496fa7c14991a23c03c62e44637e71), [INTEL-SA-00389](https://www.intel.com/content/www/us/en/security-center/advisory/intel-sa-00389.html) |
| SMI, APERF/MPERF | `/dev/cpu/N/msr` (0x34, 0xE7/0xE8) or `perf -e msr/smi/` | `CAP_SYS_RAWIO`; one IPI per read on another CPU. | `arch/x86/kernel/msr.c`, `arch/x86/events/msr.c` |
| current frequency | `scaling_cur_freq`, `/proc/cpuinfo` | 6.8: APERF/MPERF from the last tick, no IPI. 5.15: an IPI to the CPU and up to 10 ms of sleep per read. | `arch/x86/kernel/cpu/aperfmperf.c` at both tags |
| throttling, temperature | `thermal_throttle/*_count`, coretemp `temp*_input` | Throttle counters are per-CPU variables (no IPI). A coretemp read is an IPI to that core, cached 1 s. | `drivers/thermal/intel/therm_throt.c`, `drivers/hwmon/coretemp.c` |
| NVML | `nvmlDeviceGetTotalEnergyConsumption` | mJ since the driver loaded, Volta and newer. `GetPowerUsage` is a 1 s average on Ampere and newer. Without persistence, the driver state is torn down at the last client and re-init costs seconds. Counter update rate and call cost **unverified**. | [NVML](https://docs.nvidia.com/deploy/nvml-api/latest/api/group__nvmlDeviceQueries.html), [persistence](https://docs.nvidia.com/deploy/driver-persistence/latest/background.html) |
| NVML settings | `SetPowerManagementLimit`, `SetGpuLockedClocks`, `SetPersistenceMode`, `SetEccMode`, `SetMigMode` | All need root. The power limit (Kepler and newer), like persistence mode, lives only while the GPU is initialized: without persistence it is lost when the last client exits, and always at reboot or driver unload; locked clocks (Volta and newer) go back to default at reboot or driver reload and are undone by `ResetGpuLockedClocks`; persistence resets to off at every reboot. An ECC change takes effect after the next reboot; a MIG change may reset or unbind the GPU. `GetEccMode` and `GetMigMode` return current and pending. No query for the current locked clocks was found (**unverified** that none exists). `GetComputeRunningProcesses` lists compute contexts only. | [commands](https://docs.nvidia.com/deploy/nvml-api/latest/api/group__nvmlDeviceCommands.html), [MIG](https://docs.nvidia.com/deploy/nvml-api/latest/api/group__nvmlMultiInstanceGPU.html), [data persistence](https://docs.nvidia.com/deploy/driver-persistence/latest/data-persistence.html), [queries](https://docs.nvidia.com/deploy/nvml-api/latest/api/group__nvmlDeviceQueries.html) |
| perf counting | `perf_event_open`, `sample_period` 0 | A counting event has no sample period, so no overflow samples. `inherit` counts children created after the open; `enable_on_exec` starts counting at `execve`. The man page says `inherit` doesn't work with `PERF_FORMAT_GROUP`, but v6.8 refuses only `inherit` with `PERF_SAMPLE_READ`, and `perf_read_group` sums the children. A read sums the live children, each read of an active event being an IPI to its CPU (`perf_event_read`). More events than counters are multiplexed by an hrtimer every `1000/HZ` ms by default; `time_enabled` ≠ `time_running` shows it. `perf_event_paranoid` defaults to 2 (user space only); per-CPU events need `CAP_PERFMON` or a value below 1. Ubuntu 24.04 kernels set `CONFIG_SECURITY_PERF_EVENTS_RESTRICT=y` (config annotations); its exact effect was not read (**unverified**). | [perf_event_open(2)](https://man7.org/linux/man-pages/man2/perf_event_open.2.html), `kernel/events/core.c`, [perf-security.rst](https://raw.githubusercontent.com/torvalds/linux/v6.8/Documentation/admin-guide/perf-security.rst), [Ubuntu annotations](https://kernel.ubuntu.com/git/ubuntu/ubuntu-noble.git/plain/debian.master/config/annotations) |
| flock | `flock(fd, LOCK_EX)` | Blocks until the lock is free: the waiter sleeps on a wait queue, and a release wakes every blocked waiter, which retry (no first-come order). Works whatever the open mode. Freed when the last descriptor of the open file description closes, so also when the holder dies; kept across `fork` and `execve` unless the fd is `O_CLOEXEC`. | [flock(2)](https://man7.org/linux/man-pages/man2/flock.2.html), `fs/locks.c` |
| subreaper, parent death | `PR_SET_CHILD_SUBREAPER`, `PR_SET_PDEATHSIG` | Orphans are reparented to the nearest living subreaper, which can `wait` for them; not inherited by children. The parent-death signal fires when the parent *thread* exits, is cleared in a forked child and on set-user-ID exec. | [PR_SET_CHILD_SUBREAPER](https://man7.org/linux/man-pages/man2/PR_SET_CHILD_SUBREAPER.2const.html), [PR_SET_PDEATHSIG](https://man7.org/linux/man-pages/man2/PR_SET_PDEATHSIG.2const.html) |
| SSH session end | sshd, `ClientAliveInterval`, `ServerAliveInterval`, `TCPKeepAlive` | When the connection ends, sshd exits and closes a non-pty command's pipes: the command sees EOF and gets no signal. With a pty, closing the master hangs the terminal up (`SIGHUP`). Both alive intervals default to 0 (no messages). `TCPKeepAlive` defaults to yes, but Linux sends the first probe after `tcp_keepalive_time`, 7200 s by default. | [session.c](https://raw.githubusercontent.com/openssh/openssh-portable/master/session.c), [serverloop.c](https://raw.githubusercontent.com/openssh/openssh-portable/master/serverloop.c), [sshd_config(5)](https://man7.org/linux/man-pages/man5/sshd_config.5.html), [ssh_config(5)](https://man7.org/linux/man-pages/man5/ssh_config.5.html), [tcp(7)](https://man7.org/linux/man-pages/man7/tcp.7.html) |
| logout | logind `KillUserProcesses=` | `yes` kills the session scope, setsid'd processes included; `no` abandons the scope. Upstream default `yes`; Debian and Ubuntu 24.04 build with `-Ddefault-kill-user-processes=false`. | [logind.conf](https://www.freedesktop.org/software/systemd/man/255/logind.conf.html), [Ubuntu debian/rules](https://git.launchpad.net/ubuntu/+source/systemd/plain/debian/rules?h=ubuntu/noble) |
| delegation | cgroup v2, transient scopes | Delegating to a user means giving write access to the directory and its `cgroup.procs`, `cgroup.threads` and `cgroup.subtree_control`. Scopes are created only through systemd's bus API and manage processes started elsewhere. `cpu.stat` exists without the cpu controller. Creating a delegated scope for another user's process was not tried (**unverified**). | `cgroup-v2.rst`, [systemd.scope](https://www.freedesktop.org/software/systemd/man/255/systemd.scope.html) |
| BMC | DCMI Get Power Reading; Redfish | DCMI gives integer watts over a vendor-defined window, plus a statistics period, and no energy counter. Redfish `Sensor` has `SensingInterval`, `ReadingTime`, `AveragingInterval`. In band, KCS without interrupts runs the polling `kipmid` thread. iDRAC window **unverified**. | [DCMI 1.5](https://www.intel.com/content/dam/www/public/us/en/documents/technical-specifications/dcmi-v1-5-rev-spec.pdf), [Sensor schema](https://redfish.dmtf.org/schemas/v1/Sensor.v1_10_0.json), `Documentation/driver-api/ipmi.rst` |
| waiting | `pidfd_open` (5.3), `waitid(P_PIDFD)` (5.4), inotify | inotify misses changes made on other NFS clients and doesn't work on `/proc` or `/sys`. GNU `tail -f` polls every second on remote file systems. | [pidfd_open(2)](https://man7.org/linux/man-pages/man2/pidfd_open.2.html), [inotify(7)](https://man7.org/linux/man-pages/man7/inotify.7.html), [tail.c](https://raw.githubusercontent.com/coreutils/coreutils/master/src/tail.c) |
| timer slack | `prctl(PR_SET_TIMERSLACK)` | Real-time tasks get no slack. Slack is inherited through `fork` and kept across `execve`; 0 resets to a "default" that is itself copied from the parent, so a child must set a value (init's is 50 µs). | `kernel/time/hrtimer.c`, [PR_SET_TIMERSLACK](https://man7.org/linux/man-pages/man2/PR_SET_TIMERSLACK.2const.html) |
| end of a run | `cgroup.events` (`populated`) | Children can outlive the main process; the cgroup's `populated 0` is the reliable end, and `cgroup.kill` ends the rest. | `cgroup-v2.rst` |
| background noise | `kernel/nmi_watchdog`, `kernel/watchdog_cpumask`, `mm/ksm/run`, `mm/transparent_hugepage/*`, `vm/stat_interval`, `vm/dirty_writeback_centisecs`, `workqueue/cpumask` (hex), `kernel/numa_balancing`, `kernel/timer_migration` | NMI watchdog holds a perf counter per CPU. Never read `vm/stat_refresh`, which runs work on every CPU. Per-CPU kernel threads can't move (`PF_NO_SETAFFINITY`). | `Documentation/admin-guide/sysctl/{kernel,vm}.rst`, `mm/vmstat.c` |
| order: intel_pstate | `intel_pstate/status`, EPP, `no_turbo` | Switching active/passive resets all settings. EPP > 0 under `performance` returns `EBUSY`. `no_turbo` clamps the frequency limits and restores them only if they weren't written in between. Turbo disabled in BIOS: `EPERM`. | [intel_pstate.rst](https://raw.githubusercontent.com/torvalds/linux/v6.8/Documentation/admin-guide/pm/intel_pstate.rst), `drivers/cpufreq/intel_pstate.c` |
| order: SMT, uncore, idle, RAPL | `smt/control`, `intel_uncore_frequency/*`, `cpuidle/state*/disable`, `constraint_*` | Through offline/online, cpufreq policies, idle `disable` flags and EPB survive, IRQ affinity doesn't. The Cascade Lake uncore driver doesn't check min ≤ max; TPMI refuses cluster values outside the package limits. An idle state disabled on one CPU is still used by the others. BIOS-locked RAPL limits return `EACCES`. | [uncore](https://raw.githubusercontent.com/torvalds/linux/v6.8/Documentation/admin-guide/pm/intel_uncore_frequency_scaling.rst), [cpuidle](https://raw.githubusercontent.com/torvalds/linux/v6.8/Documentation/admin-guide/pm/cpuidle.rst), `intel_rapl_common.c` |
| systemd unit | `ExecStopPost=+…`, `Delegate=yes`, `User=` | `ExecStopPost=` also runs after an unexpected exit; `+` runs it with full privileges; it runs under the `TimeoutStopSec=` timer (90 s default). With `User=`, the delegated cgroup is chowned to that user; `cpuset` can be delegated; a process can't stay in a cgroup that has children. | [systemd.service](https://www.freedesktop.org/software/systemd/man/255/systemd.service.html), [service.c](https://raw.githubusercontent.com/systemd/systemd/v255/src/core/service.c), [delegation](https://systemd.io/CGROUP_DELEGATION/) |
| expiry timer | `systemd-run --on-active=` | Creates a transient timer plus service. | [systemd-run](https://www.freedesktop.org/software/systemd/man/255/systemd-run.html) |
| PAM lock | `pam_access accessfile=`, `pam_nologin file=` | A missing custom accessfile makes pam_access return `PAM_ABORT` (linux-pam 1.5.3, the Ubuntu 24.04 version). pam_nologin returns `PAM_AUTH_ERR` for non-root users when the file exists and `PAM_IGNORE` when it doesn't, never refuses root, and shows the file's text. A custom accessfile with no matching line gives success, so a broken file opens the lock. pam_succeed_if has no file test. `default=` in `[value=action]` covers `abort`. sshd runs the account stack for every authentication type. | [pam_access.c](https://raw.githubusercontent.com/linux-pam/linux-pam/v1.5.3/modules/pam_access/pam_access.c), [pam_nologin.c](https://raw.githubusercontent.com/linux-pam/linux-pam/v1.5.3/modules/pam_nologin/pam_nologin.c), [pam_nologin(8)](https://man7.org/linux/man-pages/man8/pam_nologin.8.html), [access.conf(5)](https://man7.org/linux/man-pages/man5/access.conf.5.html), [pam.conf(5)](https://man7.org/linux/man-pages/man5/pam.conf.5.html), [sshd_config(5)](https://man7.org/linux/man-pages/man5/sshd_config.5.html) |
| Go runtime | `runtime/proc.go` | When idle, sysmon parks for up to `forcegcperiod/2` (1 min); a GC is forced every 2 min. | [proc.go](https://raw.githubusercontent.com/golang/go/master/src/runtime/proc.go) |

## 3. Methodology

- **When to stop.**
  - Hoefler & Belli 2015 recompute a nonparametric CI of the median after each run and stop when
    it is narrow enough; at least 6 runs are needed for that CI.
  - CONFIRM (Maricq 2018) estimates the runs needed for a ±1% median CI from past data. The
    count reached 670 in one case and varied >10× between setups (**measured**).
  - The Phoronix Test Suite stops at 3.5% std dev with a 2× cap (**claimed**).
  - Kalibera & Jones 2013 plan repetitions per level (iteration, process, build) from a pilot
    run and report effect size with a CI (**measured**, old hardware).
- **Order.**
  - Abedi & Brecht 2017: fixed and blocked orders "falsely report" a 38% difference between
    identical systems; randomized multiple interleaved trials (RMIT) do not (**trace**, 2010 EC2
    data).
  - Duplyakin 2023: order biased results by 50% or more and could change the conclusion in 72%
    of cases. Their method runs fixed and random orders side by side and compares them with
    Kruskal-Wallis (**measured**, 1,700+ bare-metal servers).
  - Mytkowicz 2009: environment size and link order alone swing a speedup from 0.92 to 1.10
    (**measured**, old hardware).
- **Thermal state.**
  - Krun (Barrett 2017) waits until every sensor is within ±3 °C of its boot value (timeout 1 h),
    checks `dmesg` and records APERF/MPERF per run (**measured** system).
  - On one CPU, run-to-run power correlates with temperature (>84% at full load), and identical
    CPU models differ by up to 29.6% at idle (von Kistowski 2016, **measured**, old hardware).
  - Turbo spreads run time by up to 16% between processors, driven by power limits rather than
    temperature (Acun 2016, **measured**, old hardware).
  - Schöne 2021 pre-heats for 15 min.
  - No paper defines a steady-state test; a slope rule would be our own design.
- **Energy variation.** Ournani 2020 report that controlling the protocol, C-states, turbo and
  pinning can tighten energy variation by up to 30× for some workloads. Only the abstract was read,
  so the per-factor numbers are **unverified**.
- **Regression detection.**
  - Change points:
    - E-Divisive (Matteson & James 2014) in MongoDB's CI found about 1 useful ticket per 100 under
      the old threshold scheme, and canary workloads flag suspect runs (Daly 2020, **measured**).
    - Hunter (Fleming 2023) replaced the permutation test with a t-test so results repeat, and
      needed about 30 points of history; "change point detection cannot fix noisy data"
      (**measured**, injected changes).
  - Run time: running both versions in random order on the same instances detects slowdowns of
    10% or less (Laaber 2019; abstract only).
  - Energy: Danglot 2024 (results not read) and EnergyTrackr (no false-positive rate, by its own
    account) report no detection limit.
- **Noise as a covariate.** No prior work was found that adjusts energy or performance results with
  OS-noise counters; tools only flag or drop runs. CUPED (Deng 2013) cuts variance by
  var·(1 − ρ²) using a covariate measured *before* the experiment, about 50% at Bing
  (**measured**, web A/B tests). Raffin & Trystram 2025 measured that frequent RAPL sampling raises
  idle energy (**measured**, EPYC and Xeon). On AMD Zen 2, RAPL energy is modeled, not measured
  (Schöne 2021, **measured** against an AC meter).
- **Reporting.** Only 2 of 95 HPC papers reported CIs and 15 any variance (Hoefler 2015). The SIGPLAN
  checklist asks for all parameters, versions and hardware. Guilloteau 2024 surveyed about 300
  papers on artifact longevity (abstract only).

## 4. Features worth adding (ranked)

Ranked by how much each helps the project's questions per unit of work. "In design" means it is
already in [MINION.md](MINION.md).

1. **A/A and injected-regression modes.** They give the false-positive rate and the detection rate
   against the number of runs, which is RQ3, and nobody reports them for energy. Sources: Laaber
   2019, Bulej 2020, Fleming 2023; the gap is stated by EnergyTrackr. *In design (RQ3).*
2. **Journaled save-before-write, restore when the client dies, and compare and restore.** This is
   the core of RQ2; GEOPM Service, Slurm and tuned each have part of it. *In design.*
3. **Randomized interleaved rounds, with seed, round and position saved.** Order bias is large and
   common on bare metal (Abedi 2017, Duplyakin 2023). *In design.*
4. **Order-independence check.** Run some rounds in a fixed order, then test fixed against random
   (Kruskal-Wallis) and run order against shuffled (Mann-Whitney). This shows whether a machine
   carries state between runs (Duplyakin 2023, Maricq 2018). *New: analysis on top of `plan.json`.*
5. **Adaptive stopping on a median CI, with a minimum and a maximum.** The runs needed differ more
   than 10× between machines (Hoefler 2015, Maricq 2018, Phoronix Test Suite). *New; noted in
   the design as later.*
6. **Canary workload that pauses the queue on drift.** It catches slow machine drift that
   statistics can't fix (Green Metrics Tool, Daly 2020). *In design (optional).*
7. **Covariate adjustment with pre-run covariates only.** For example the start temperature and
   other cgroups' CPU time before the run, judged by the A/A and injected results (Deng 2013).
   This is the test of whether the noise record helps. *New, RQ3.*
8. **A `verify` command and start/end checks.** Use the Green Metrics Tool's list: timers, cron,
   NTP, swap, RAPL limits, suspend and steal time. A cheap suspend check is a jump of
   `CLOCK_BOOTTIME` against `CLOCK_MONOTONIC`. Sources: tuned, Green Metrics Tool. *In design.*
9. **Thermal gate as a band around the idle baseline, with a timeout.** The Green Metrics Tool also
   warms a cold machine. Sources: Krun, Green Metrics Tool. *In design without warm-up.*
10. **Measure and publish the runner's own overhead.** Use a calibrate script, as the Green Metrics
    Tool did. *In design (RQ2).*
11. **Quality flags kept with the run, never silent drops.** Use a median/MAD outlier flag, a
    first-run warning (hyperfine) and new `dmesg` lines (Krun). *In design (flags); add the two
    rules.*
12. **Energy-source metadata.** Record the zone backend (MSR, TPMI) and whether energy is modeled
    (AMD) (Schöne 2021). *In design (`host.json`).*
13. **Topology-aware experiment CPUs.** Use whole physical cores on one socket with local memory
    (BenchExec, measured +22% and +6.8%). *New: guidance for host profiles.*
14. **Meter data off the host, matched by time window.** Treat BMC power as coarse (Kwollect).
    *In design (wall sampler).*
15. **Offline change-point detection across commits, deterministic, with a minimum history.**
    Sources: Daly 2020, Fleming 2023. *Later, for continuous use.*
16. **Event names from Slurm's mail types and a push sink such as Pushover.** *In design (events,
    `run` hooks).*

Avoid:
- resetting to fixed defaults (pyperf, LIKWID, EnergyTrackr);
- one-way machine preparation (the Green Metrics Tool's NOP Linux);
- periodic samplers that visit every CPU during runs (turbostat);
- boot options such as `idle=poll` that change idle energy;
- the osnoise workload during a measured run.

## 5. Unverified or not fully read

- Paper texts behind a paywall (only metadata, abstracts or code read): benchkit, EnergyTrackr
  (ASE), Ournani 2020, Laaber 2019 and 2020, Guilloteau 2024, Danglot 2024 (results), Khrouf 2026,
  Kwollect, perun, Kepler, SmartWatts, EnergAt (method details).
- Whether tuned's originals survive a daemon crash; what Slurm does if `slurmstepd` crashes.
- Overhead numbers for Scaphandre, Kepler, perun, PowerAPI and turbostat (none published).
- osnoise overhead in no-workload mode; the RAPL hardware update interval; the energy unit on the
  6248R and 6515P; Ubuntu backports of Granite Rapids RAPL; the NVML energy update rate and call
  cost; whether the NVML counter resets without persistence; the iDRAC averaging window.
- Whether sshd shows pam_nologin's text to clients that log in with a key.
- Whether NVML can read back the current locked clocks; the default `perf_event_paranoid` on the
  hosts' Ubuntu kernels; whether a root-made transient scope can be delegated to the user's
  running process; `KillUserProcesses` on the ETH hosts. The SSH facts come from reading OpenSSH
  source, not from a test.
- Prior work using OS-noise counters as covariates, energy-specific power analysis, and Cliff's
  delta: none found with the sources available (Crossref, Semantic Scholar, arXiv). This should be
  re-checked in the ACM DL, IEEE Xplore and Google Scholar.

## References

- Abedi, A., Brecht, T. 2017. Conducting repeatable experiments in highly variable cloud computing
  environments. ICPE. https://doi.org/10.1145/3030207.3030229
- Acun, B., Miller, P., Kale, L. V. 2016. Variation among processors under Turbo Boost in HPC
  systems. ICS. https://doi.org/10.1145/2925426.2926289
- Barrett, E., Bolz-Tereick, C. F., Killick, R., Mount, S., Tratt, L. 2017. Virtual machine warmup
  blows hot and cold. OOPSLA (PACMPL 1). https://doi.org/10.1145/3133876
- Bechet, F., Maquoi, J., Cruz, L., Vanderose, B., Devroey, X. 2026. EnergyTrackr: a modular energy
  regressions detection tool. ASE. https://doi.org/10.1145/3832783.3834640
- Beyer, D., Löwe, S., Wendler, P. 2019. Reliable benchmarking: requirements and solutions. STTT
  21(1). https://doi.org/10.1007/s10009-017-0469-y
- Bulej, L., Horký, V., Tůma, P., Farquet, F., Prokopec, A. 2020. Duet benchmarking: improving
  measurement accuracy in the cloud. ICPE. https://doi.org/10.1145/3358960.3379132
- Daly, D., Brown, W., Ingo, H., O'Leary, J., Bradford, D. 2020. The use of change point detection
  to identify software performance regressions in a continuous integration system. ICPE.
  https://doi.org/10.1145/3358960.3375791
- Danglot, B., Falleri, J.-R., Rouvoy, R. 2024. Can we spot energy regressions using developers
  tests? Empirical Software Engineering. https://doi.org/10.1007/s10664-023-10429-1
- de Oliveira, D. B., Casini, D., Cucinotta, T. 2023. Operating system noise in the Linux kernel.
  IEEE Transactions on Computers 72(1). https://doi.org/10.1109/TC.2022.3187351
- Deng, A., Xu, Y., Kohavi, R., Walker, T. 2013. Improving the sensitivity of online controlled
  experiments by utilizing pre-experiment data. WSDM. https://doi.org/10.1145/2433396.2433413
- Duplyakin, D. et al. 2023. Avoiding the ordering trap in systems performance measurement. USENIX
  ATC. https://www.usenix.org/system/files/atc23-duplyakin.pdf
- Fleming, M. et al. 2023. Hunter: using change point detection to hunt for performance
  regressions. ICPE. https://doi.org/10.1145/3578244.3583719
- Guilloteau, Q., Ciorba, F. M., Poquet, M., Goepp, D., Richard, O. 2024. Longevity of artifacts in
  leading parallel and distributed systems conferences: a review of the state of the practice in
  2023. ACM REP. https://doi.org/10.1145/3641525.3663631
- He, H., Friedman, M., Rekatsinas, T. 2023. EnergAt: fine-grained energy attribution for
  multi-tenancy. HotCarbon. https://doi.org/10.1145/3604930.3605716
- Hoefler, T., Belli, R. 2015. Scientific benchmarking of parallel computing systems: twelve ways to
  tell the masses when reporting performance results. SC. https://doi.org/10.1145/2807591.2807644
- Kalibera, T., Jones, R. 2013. Rigorous benchmarking in reasonable time. ISMM.
  https://doi.org/10.1145/2464157.2464160
- Laaber, C., Scheuner, J., Leitner, P. 2019. Software microbenchmarking in the cloud. How bad is it
  really? Empirical Software Engineering. https://doi.org/10.1007/s10664-019-09681-1
- Maquoi, J., Bechet, F., Cruz, L., Vanderose, B., Devroey, X. 2026. Systematic detection of energy
  regressions in software version histories. arXiv:2610.19895 [preprint].
  https://arxiv.org/abs/2610.19895
- Maricq, A., Duplyakin, D., Jimenez, I., Maltzahn, C., Stutsman, R., Ricci, R. 2018. Taming
  performance variability. OSDI. https://www.usenix.org/conference/osdi18/presentation/maricq
- Matteson, D. S., James, N. A. 2014. A nonparametric approach for multiple change point analysis
  of multivariate data. JASA. https://doi.org/10.1080/01621459.2013.849605
- Mytkowicz, T., Diwan, A., Hauswirth, M., Sweeney, P. F. 2009. Producing wrong data without doing
  anything obviously wrong! ASPLOS. https://doi.org/10.1145/1508244.1508275
- Ournani, Z., Belgaid, M. C., Rouvoy, R., Rust, P., Penhoat, J., Seinturier, L. 2020. Taming
  energy consumption variations in systems benchmarking. ICPE. https://doi.org/10.1145/3358960.3379142
- Paolillo, A., Van Molle, M., Hasselmann, K. 2026. benchkit: a declarative framework for
  composable performance evaluation of system software. ICPE. https://doi.org/10.1145/3777884.3796997
- Raffin, G., Trystram, D. 2025. Dissecting the software-based measurement of CPU energy
  consumption: a comparative analysis. IEEE TPDS 36(1). https://doi.org/10.1109/TPDS.2024.3492336
- Schöne, R. et al. 2021. Energy efficiency aspects of the AMD Zen 2 architecture. IEEE CLUSTER.
  https://doi.org/10.1109/CLUSTER48925.2021.00087
- von Kistowski, J. et al. 2016. Variations in CPU power consumption. ICPE.
  https://doi.org/10.1145/2851553.2851567
- Berger, E. D., Blackburn, S. M., Hauswirth, M., Hicks, M. 2018. SIGPLAN empirical evaluation
  checklist. https://www.sigplan.org/Resources/EmpiricalEvaluation/
