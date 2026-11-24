# minion — research

What existing tools and Linux mechanisms offer for the design in [MINION.md](MINION.md), and why
energy measurements err; the features this asks for are ranked in
[MINION.md](MINION.md#improvements-ranked). Searched 2026-11-23; tool docs and source were read
from each project's default branch on that date and may change.

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
  [section 3.3](#33-methodology)).
- **Linux facts that changed the design**, such as restoring in dependency order rather than in
  reverse, and `flock` plus the EOF that sshd gives a non-pty command instead of a service, are in
  [section 2](#2-linux-mechanisms).
- **The largest documented energy errors come from how meters are read and runs are set up, more
  than from the meters** (our reading of [section 3](#3-measurement-accuracy-and-repeatability)):
  - Tools mishandle counter wraps, zones and units, and integrate sampled power instead of
    reading energy counters.
  - Within one machine, placement and idle states change the run-to-run spread by up to 30× and
    the energy itself by up to 50%; temperature (power +10-16%) and input data move the mean.
    Turbo, OS tuning and mitigations had no consistent effect on the energy spread of the Skylake
    and Broadwell Xeons where that was measured.
  - Rests between runs, common in guides, did not help where measured; reboots between runs and a
    single idle baseline added noise.
  - No energy regression tool reports a measured false-positive rate; where null changes were
    planted, detectors without an effect-size floor flagged most of them.
  - RAPL is good for comparing runs on one Intel server since Haswell. It follows the wall within
    one machine but is not wall energy, and on AMD and on Sandy Bridge it is a model.

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
- **GEOPM Service**: the closest model for `minion serve`. When a client starts writing, it saves
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
  (and NVML, BMC) samplers that attribute power to processes or containers. They control no state;
  their overhead and accuracy are in [section 3.4](#34-tools).
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
| NVML | `nvmlDeviceGetTotalEnergyConsumption` | mJ since the driver loaded, Volta and newer. `GetPowerUsage` is a 1 s average on Ampere and newer except GA100 (A100), which returns instantaneous power ([nvml.h](https://raw.githubusercontent.com/NVIDIA/go-nvml/main/gen/nvml/nvml.h)). Without persistence, the driver state is torn down at the last client and re-init costs seconds. Counter update rate and call cost **unverified**. | [NVML](https://docs.nvidia.com/deploy/nvml-api/latest/api/group__nvmlDeviceQueries.html), [persistence](https://docs.nvidia.com/deploy/driver-persistence/latest/background.html) |
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

## 3. Measurement accuracy and repeatability

Why energy numbers are wrong or don't repeat, with the size of each effect where it was measured.
Searched 2026-11-23 in arXiv, Crossref, Semantic Scholar, HAL, OSTI and the tools' trackers;
ACM DL and IEEE Xplore PDFs could not be fetched ([section 4](#4-unverified-or-not-fully-read)).
Everything listed was read in full unless marked. Old hardware is marked by its generation.

### 3.1 Meters

| Problem | Size of the effect | Evidence | Source |
|---|---|---|---|
| RAPL to wall power is a slope, not an offset | Pearson ≈ 0.95; wall vs tool slope 1.17 (CPU), 1.18 (GPU) | **measured**; 2× Xeon E5-2698 v4 + 8× V100, AC meter at 1 Hz; 2023 | Jay 2023 |
| What RAPL and NVML miss grows with load | up to 8.7% of node power (CPU only), 19-26% with 4-8 GPUs | **measured**; 2× Xeon + Quadro RTX, Yokogawa WT310E; 2025 | Koszczał 2025 |
| Sandy Bridge RAPL is modelled; Haswell-EP adds VR measurements | wall-power fit error 4.0-4.3% (Sandy Bridge) vs 1.7-1.8% (Haswell) | **trace**; ~860 cluster nodes at 0.5 Hz; 2018 | Khan 2018 |
| AMD RAPL is a model, has no DRAM zone, misses data-dependent power | system power +7.6% between operand values, RAPL within 0.08%; Zen 3 "significantly overestimates" | **measured**; 2× EPYC 7502, AC meter; Frontier (Trento); 2021, 2025 | Schöne 2021; Hernandez 2025 |
| DRAM RAPL has an offset, worst at idle | Haswell-EP: within 5% for one DIMM type, +9-20% for another, idle +76%; desktop Haswell idle up to +230% | **measured**, instrumented DIMMs; 2016 | Desrochers 2016 |
| The kernel has used wrong units | Sapphire Rapids DRAM scaled with 15.3 µJ instead of the MSR unit (about 4×) from 5.9 until 2022; SPR PSys steps in 1 J (6.8) | **source** | [commit 4c081324df56](https://github.com/torvalds/linux/commit/4c081324df56), `intel_rapl_common.c` v6.8 |
| Updates every ~1 ms, with jitter and no timestamp | mean 1.0023 ms, ±20 µs; fixed-rate sampling above 50 Hz gives ±20% spikes, below 50 Hz < 0.5%; PP0 every 50 µs | **measured**; Sandy Bridge to Cascade Lake; 2012-2021 | Hähnel 2012; Khan 2018; Lipp 2021 |
| Energy filtering (the Platypus mitigation) adds noise | < 2% std dev at 1 s, 5-15% at 100 ms; filters PKG and PP0 (DRAM not listed); MSR 0xBC bit 0; on with SGX or TDX, off only by reboot | **claimed** (vendor docs, 2026-11) | [Intel guidance](https://www.intel.com/content/www/us/en/developer/articles/technical/software-security-guidance/advisory-guidance/running-average-power-limit-energy-reporting.html) |
| Reading RAPL often raises idle power | 2× Xeon E5-2680 v4: +1.4-1.6 W at 10 Hz, +25-49 W at 1 kHz; 2× EPYC 7702: +2.7-3.3 W, only at 1 kHz | **measured**; kernel 4.18; 2024 | Raffin 2025 |
| NVML power samples part of the time | A100, H100: 25 ms of every 100 ms; V100, Pascal ~50%; integrating it errs up to 70% | **measured**; >70 GPUs, Fermi to Hopper; 2024 | Yang 2024 |
| GPU sensors lag | K20: time constant ≈ 0.84 s; a second kernel 1 s later read +29% | **measured**; 2014, old | Burtscher 2014 |
| BMC power is coarse and late | iDRAC: 14 W steps, fixed 60 s window, rounded down; a model calibrated on IPMI erred −15.8%, one calibrated on a wall meter −0.2%; plug meters lag RAPL by 10-24 s | **measured**; Dell R430 (Haswell-EP), 2019; cluster trace, 2018 | Kavanagh 2019; Khan 2018 |
| Above-idle ("dynamic") energy from on-chip sensors | mean error up to 73%, max 300% against an AC meter | **measured**; Haswell, Skylake, K40c, P100; 2019 | Fahad 2019 |

### 3.2 Sources of variation

"Spread" is the run-to-run standard deviation on one machine unless it says between parts.

| Problem | Size of the effect | Evidence | Source |
|---|---|---|---|
| Identical CPUs draw different power | 30 E5-2603 v2 in one board: full-load power spread > 30%, run time 0.3%; 12 identical Dell R630: 7.8% | **measured**; Ivy Bridge, Haswell; 2018-2019 | Wang 2019; Wang 2018 |
| Since Haswell, power stays at the limit and speed varies instead | LINPACK 355-400 GFlop/s (> 10%), RAPL σ 0.4 W; Sandy Bridge σ 0.6 GFlop/s, power ±5% | **measured**; 1,144 Haswell, 412 Sandy Bridge sockets; 2016 | Schuchart 2016 |
| Newer parts vary more | median-node spread 10% (Broadwell) vs 5.2% (Sandy Bridge); power 20% vs 10%; a 50 W cap: worst case 4× | **measured**; ~4,300 LLNL nodes; 2017 | Marathe 2017 |
| Identical GPUs differ | 8% mean (22% max) performance between GPUs, one GPU 0.12-6%; a 150 W limit doubles the spread (9% to 18%) | **measured**; V100, RTX 5000, MI60 clusters; 2022 | Sinha 2022 |
| Power rises with temperature | server +16% from CPU 37.7 to 74.5 °C (fans excluded); warmer rack neighbours +5.6%; Haswell package +10-12% from 37 to 74 °C | **measured**; Haswell and Skylake servers, i7-4770; 2018 | Wang 2018; Khan 2018 |
| Power follows temperature on one CPU; identical models differ at idle | run-to-run power correlates with temperature (> 84% at full load); identical CPU models differ by up to 29.6% at idle | **measured**, old hardware | von Kistowski 2016 |
| Turbo spreads run time between processors | up to 16%, driven by power limits rather than temperature | **measured**, old hardware | Acun 2016 |
| Power drifts for minutes at constant load | 625 to 629 W over the first 7 min; labs pre-heat 15 min | **measured**; 2× Xeon Gold 6154; 2019, 2021 | Schöne 2019; Schöne 2021 |
| Fans | a fixed good fan speed saved 8.3% of CPU + fan energy over the default control | **measured**; SPARC T3, old; 2015 | Zapater 2015 |
| One awake core keeps the package out of deep idle | Rome: one core in C1 instead of C2 +81 W at the wall (99 to 180 W); SMT siblings offline do the same; Skylake-SP first core in C1E +97 W | **measured**; 2× EPYC 7502, 2× Gold 6154; 2021 | Schöne 2021 |
| C-states off: less spread, more energy | 1 process: spread 5× lower, energy +50%; 2 processes 6×, +20%; no effect from 8 processes | **measured**; 2× Xeon Gold 6130, Linux 4.9, RAPL; 2020 | Ournani 2020 |
| Placement | at 50% load, one socket with its hyper-threads vs both sockets with theirs: spread 30× lower (116 vs 3,452 mJ); avoiding hyper-threads up to 5× | **measured**; same | Ournani 2020 |
| Placement on AMD, through turbo voltage | same thread count on different NUMA nodes: energy 8.1-12.8% apart | **measured**; EPYC 7551P (Zen 1), Linux 5.0; 2021 | von Elm 2021 |
| Turbo, stopped services, the KPTI patch | spread changed < ±10%, no consistent direction | **measured**; Xeon Gold 6130, E5-2630 v4; 2020 | Ournani 2020 |
| Rest and reboot between runs | 60 s sleep: no gain; reboot before each run: spread 1.5× worse at full load | **measured**; same | Ournani 2020 |
| Input data | same instructions, different operands: system power > 18% apart; under a power limit the frequency follows the data | **measured**; Skylake-SP; Coffee Lake to Tiger Lake; 2019, 2022 | Schöne 2019; Wang 2022 |
| Mitigations and kernels (time only) | Meltdown/Spectre patches 2-3% single node, 5-11% multi-node; NUMA balancing on: 28-30% slower | **measured**; 2018; 2× EPYC 7742, 2026 | Simakov 2018 [preprint]; Lober 2026 [preprint] |
| OS noise (time only) | ≤ 2.5% per process, but one daemon (snmpd) brought back most of the barrier-time spread | **measured**; 2003, 2016 | Petrini 2003; Leon 2016 |
| Co-runners on the package | a power virus on other cores pushed a service to 190% of its latency target at low load; attribution tools +46% / −93% | **measured**; Haswell, 2015; 2× E5-2630, 2023 | Lo 2015; He 2023 |
| Thermal gates | Krun waits until every sensor is within ±3 °C of its boot value (timeout 1 h), checks `dmesg` and records APERF/MPERF per run; no paper defines a thermal steady-state test, so a slope rule would be our own design | **measured** system | Barrett 2017 |
| Warm-up and steady state | 30-43.5% of (VM, benchmark) pairs reach steady state; 10.9% of JMH forks never do; a JVM's energy lead reversed after 150 s; for performance, steady state is detected with change points | **measured**; 2017, 2022, 2021 | Barrett 2017; Traini 2022; Ournani 2021 |

### 3.3 Methodology

| Problem | Size of the effect | Evidence | Source |
|---|---|---|---|
| Stopping on a CI of the median | recompute a nonparametric CI of the median after each run and stop when it is narrow enough, at least 6 runs for that CI; CONFIRM estimates the runs for a ±1% median CI from past data: 670 in one case, more than 10× apart between setups; the Phoronix Test Suite stops at 3.5% std dev with a 2× cap | **measured** (CONFIRM); **claimed** (Phoronix) | Hoefler 2015; Maricq 2018; Phoronix Test Suite |
| Repetitions per level | plan repetitions per level (iteration, process, build) from a pilot run, and report the effect size with a CI | **measured**, old hardware | Kalibera 2013 |
| Stopping when the test is significant ("peeking") | false positives "can easily increase fivefold" | **simulated**, online A/B tests | Johari 2022 |
| Stopping rules differ | relative CI width 3% (99% bootstrap) kept 87.6% of JMH benchmarks equal to full runs, CV 78.8%, and cut time 48-86% | **measured**; Xeon X5670, Linux 5.2; 2020 | Laaber 2020 |
| Best-of-n and means without CIs | up to 16% of comparisons too strong, > 3% in the wrong direction | **measured**; 2007, old | Georges 2007 |
| Runs are not independent | lag-1 correlation 0.668 dropped a 99% CI's coverage to 92% | **measured**, **simulated**; 2017 | Barrett 2017 |
| Pairing helps only if the pair shares the noise | duet runs narrowed CIs 5-37×; pairs shuffled: 1.02-1.03× | **measured**; clouds and bare metal; 2020 | Bulej 2020 |
| Fixed and blocked orders | they "falsely report" a 38% difference between identical systems, randomized multiple interleaved trials (RMIT) do not; a fixed A, B, A, B order aliases with periodic noise (odd trials slower than even) | **trace**; EC2 2010-2011 | Abedi 2017 |
| Order on bare metal | order biased results by 50% or more and could change the conclusion in 72% of cases; the method runs fixed and random orders side by side and compares them with Kruskal-Wallis | **measured**; 1,700+ bare-metal servers | Duplyakin 2023 |
| Environment and link order | environment size and link order alone swing a speedup from 0.92 to 1.10 | **measured**, old hardware | Mytkowicz 2009 |
| Fixed warm-up counts | developers' JMH warm-up right in 19% of forks; too short in 32%, and 57% of those ≥ 5% off | **measured**; 2× Xeon E5-2650 v3; 2022 | Traini 2022 |
| Rest times are folklore | 60 s sleep did not lower energy spread; the 180 s (Green Metrics Tool) and 60 s (Cruz) rests come with no data | **measured**; **claimed** | Ournani 2020; [GMT](https://raw.githubusercontent.com/green-coding-solutions/documentation/main/content/en/docs/measuring/best-practices.md); [Cruz 2021](https://luiscruz.github.io/2021/10/10/scientific-guide.html) (blog) |
| Idle-baseline subtraction | idle spread up to 140% above a one-process job; the first active core adds 22-81 W, so energy − idle × time is not the run's own energy; one lab pins fans at 100% to keep idle fixed | **measured**; 2020, 2024, 2019 | Ournani 2020; Cadorel 2024; Khaleghzadeh 2019 [preprint] |
| Null changes are flagged | planted null changes: 49/60 flagged without an effect-size floor, 0/60 with Cohen's d ≥ 0.8, which still caught 54/60 large ones (decided on CPU cycles, as energy CV was 0.09-0.28 vs 0.02-0.10) | **measured**; RAPL, Java tests; 2024 | Danglot 2024 |
| Many tests at α = 0.05 | 114 of 320 commits flagged while 318 of 319 changes were < 5%; no ground truth | **measured**; Xeon Gold 6326; 2026 | Maquoi 2026 [preprint] |
| Change points in CI | E-Divisive (Matteson & James 2014) in MongoDB's CI found about 1 useful ticket per 100 under the old threshold scheme, and canary workloads flag suspect runs; Hunter replaced the permutation test with a t-test so results repeat, and needed about 30 points of history: "change point detection cannot fix noisy data" | **measured**; injected changes (Hunter) | Daly 2020; Fleming 2023 |
| Detecting slowdowns | running both versions in random order on the same instances detects run-time slowdowns of 10% or less; for energy, Danglot 2024 and EnergyTrackr (no false-positive rate, by its own account) report no detection limit | abstract only (Laaber 2019); **source** | Laaber 2019; Danglot 2024; EnergyTrackr |
| Change-point defaults | default F1 against human labels: binary segmentation 0.87, E-Divisive 0.86, PELT 0.84; tuned mean 0.70 to 0.90; Mozilla's t > 7 and 2% rule: 12.5% false alert groups | **trace**; GraalVM, MongoDB, SAP HANA, Firefox; 2026 | Costa 2026 [preprint]; Besbes 2026 [preprint] |
| Reporting rules exist for power | SPECpower: meter ≤ 1% uncertainty, calibrated yearly, ≥ 1 Hz; inlet temperature ±0.5 °C within 50 mm; Green500's partial-window level overstated efficiency by up to 30% | **source** (2015); **measured** (2015) | [SPECpower rules](https://www.spec.org/power_ssj2008/docs/SPECpower_ssj2008-Run_Reporting_Rules.html); Rohr 2015 |
| Reporting and practice | only 2 of 95 HPC papers reported CIs and 15 any variance; the SIGPLAN checklist asks for all parameters, versions and hardware; about 300 papers surveyed on artifact longevity (abstract only); 79 green-software papers used 31 meters, mostly 10 or 30 runs | surveys; **source** | Hoefler 2015; SIGPLAN checklist (Berger 2018); Guilloteau 2024; Hort 2026 [preprint] |
| Covariates | regression adjustment with treatment × covariate terms can't lose asymptotic precision (abstract only); CUPED cuts variance by var·(1 − ρ²) using a covariate measured *before* the experiment, about 50% at Bing; no benchmarking work found that adjusts energy or performance with temperature, frequency, interrupts or other OS-noise counters: tools only flag or drop runs | **claimed** (proof); **measured**, web A/B tests | Lin 2013; Deng 2013 |

### 3.4 Tools

Accuracy only; features are in [section 1](#1-tools). Issue numbers are on each tool's GitHub
tracker, read 2026-11-23.

| Problem | Size of the effect | Evidence | Source |
|---|---|---|---|
| Counter wrap still mishandled | LIKWID: 248,923 J at 1,546 s, 30,811 J at 1,795 s (#646, open); EnergiBridge total −250,769 J (#17); Scaphandre drops the interval (#280); CarbonTracker sets it to 0; the language-ranking tool's published data have negative energies | **measured** by users, **source**; 2023-2026 | trackers; van Kempen 2025 |
| Zones summed twice | psys + package + core + uncore 10.88 W vs psys 6.66 W; per-die AMD zones counted twice, 137 W vs 125 W at the wall; idle Zen 1 package 134 W vs 102 W IPMI for the whole server | **measured** by users, **source** | CodeCarbon docs, #1274; LIKWID #241 |
| Units | EnergiBridge 15-61× off until 2024; LIKWID SPR DRAM 4× low until 2024; pyJoules 0.5.1 GPU energy 1000× too small | **source**, users | EnergiBridge #9, LIKWID #642, pyJoules #23 |
| One socket only | PowerJoular v1, EnergiBridge on Intel, EnergyTrackr's idle check read socket 0 | **source** | trackers |
| Wall-clock timestamps | 54.7 kW from a 0.5 ms clock step (Scaphandre) | **measured** by a user | Scaphandre #441 |
| Windows instead of edges | 30 s Prometheus scrapes vs reads at the edges: +30-36% on short workflows | **measured**; Xeon Silver 4314; 2025 | Thamm 2025 [preprint] |
| Attribution is a model | per-process vs RAPL: mean 3-16%, max 49%; MAPE Scaphandre 32%, EnergAt 69%, Kepler 17-23%; Kepler gives idle power to stopped containers | **measured** against RAPL; 2024-2026 | Cadorel 2024; Weigell 2026; Thamm 2026 [preprint]; Pijnacker 2025 |
| Constants and models | CodeCarbon: ~2× other RAPL tools, 80-102% of the wall, up to 40% off, its TDP fallback −60% to +190% of RAPL | **measured**; 2023-2026 | Jay 2023; Bouza 2023; Fischer 2026; CodeCarbon docs |
| Sampling cost | at 1 kHz: +0.25-47% run time (CodeCarbon worst, perf best); at default rates < 1% energy at the wall | **measured**; i7-8809G, 2026; DGX-1, 2023 | Diamond 2026 [preprint]; Jay 2023 |
| Integrating NVML power | perun, GMT, Kepler and variorum integrate the 25%-sampled power; Zeus, Alumet and CodeCarbon read the energy counter, which returns 0 J for windows shorter than its update | **source** | Yang 2024; tool sources |
| Repeatability left to the user | only GMT (warm-up, baseline, checks, refuses energy filtering) and EnergyTrackr (shuffle, idle gate) control any of it | **source** | GMT, EnergyTrackr sources |

Correct handling, from the same sources: perf's 64-bit RAPL count or each zone's own
`max_energy_range_uj`, subtraction on raw integers before converting (van Kempen 2025), one zone
per package, monotonic timestamps, and energy counters read at run edges.

## 4. Unverified or not fully read

- Paper texts behind a paywall (only metadata, abstracts or code read): benchkit, EnergyTrackr
  (ASE; its preprint was read), Laaber 2019, Guilloteau 2024, Kwollect, perun, Kepler (Amaral
  2023). Ournani 2020, Danglot 2024 and Khrouf 2026 were read on HAL, Laaber 2020 as the authors'
  version, EnergAt and SmartWatts on arXiv.
- Read only through other papers, because ACM DL and IEEE Xplore PDFs could not be fetched from
  here: von Kistowski 2016, Acun 2016,
  [Inadomi 2015](https://doi.org/10.1145/2807591.2807638),
  [Scogland 2015](https://doi.org/10.1145/2807591.2807653), Hackenberg
  [2013](https://doi.org/10.1109/ISPASS.2013.6557170) and
  [2015](https://doi.org/10.1109/IPDPSW.2015.70) (RAPL against instrumented nodes),
  [Ilsche 2015](https://doi.org/10.1109/IGCC.2015.7393710) (HDEEM); de Oliveira 2023 (osnoise) and
  [de Oliveira 2013](https://doi.org/10.1145/2451116.2451140) (quantile regression) abstract only.
- Whether tuned's originals survive a daemon crash; what Slurm does if `slurmstepd` crashes.
- Overhead numbers for perun, variorum, LIKWID and EnergyTrackr (none published; the others are in
  [section 3.4](#34-tools)).
- osnoise overhead in no-workload mode; the energy unit on the 6248R and 6515P; Ubuntu backports
  of Granite Rapids RAPL; the NVML energy counter's update rate, accuracy and call cost (Yang 2024
  studied only the power reading); whether the NVML counter resets without persistence; the
  averaging window of current iDRACs (one 2015-era iDRAC used 60 s, Kavanagh 2019).
- RAPL against a reference meter on Sapphire Rapids, Granite Rapids (TPMI) and Zen 3 to 5; BMC
  accuracy on current servers; the energy (not time) cost of timers, IRQs and SMIs on one node;
  the error a co-tenant adds to a shared package counter; the effect of EPP, microcode or BIOS
  profiles on energy variance: no measured study found.
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
- Berger, E. D., Blackburn, S. M., Hauswirth, M., Hicks, M. 2018. SIGPLAN empirical evaluation
  checklist. https://www.sigplan.org/Resources/EmpiricalEvaluation/
- Besbes, M. B. et al. 2026. Exploring statistical change point detection techniques for
  performance anomaly detection at Mozilla. arXiv:2606.18377 [preprint].
  https://arxiv.org/abs/2606.18377
- Beyer, D., Löwe, S., Wendler, P. 2019. Reliable benchmarking: requirements and solutions. STTT
  21(1). https://doi.org/10.1007/s10009-017-0469-y
- Bouza, L., Bugeau, A., Lannelongue, L. 2023. How to estimate carbon footprint when training deep
  learning models? A guide and review. Environmental Research Communications 5(11).
  https://doi.org/10.1088/2515-7620/acf81b
- Bulej, L., Horký, V., Tůma, P., Farquet, F., Prokopec, A. 2020. Duet benchmarking: improving
  measurement accuracy in the cloud. ICPE. https://doi.org/10.1145/3358960.3379132
- Burtscher, M., Zecena, I., Zong, Z. 2014. Measuring GPU power with the K20 built-in sensor.
  GPGPU-7. https://doi.org/10.1145/2576779.2576783
- Cadorel, E., Saingre, D. 2024. A protocol to assess the accuracy of process-level power models.
  IEEE CLUSTER. https://doi.org/10.1109/CLUSTER59578.2024.00014
- Costa, D. E. et al. 2026. Evaluating change point detection methods for software performance
  regression analysis. arXiv:2610.09023 [preprint]. https://arxiv.org/abs/2610.09023
- Daly, D., Brown, W., Ingo, H., O'Leary, J., Bradford, D. 2020. The use of change point detection
  to identify software performance regressions in a continuous integration system. ICPE.
  https://doi.org/10.1145/3358960.3375791
- Danglot, B., Falleri, J.-R., Rouvoy, R. 2024. Can we spot energy regressions using developers
  tests? Empirical Software Engineering. https://doi.org/10.1007/s10664-023-10429-1
- Deng, A., Xu, Y., Kohavi, R., Walker, T. 2013. Improving the sensitivity of online controlled
  experiments by utilizing pre-experiment data. WSDM. https://doi.org/10.1145/2433396.2433413
- Desrochers, S., Paradis, C., Weaver, V. M. 2016. A validation of DRAM RAPL power measurements.
  MEMSYS. https://doi.org/10.1145/2989081.2989088
- Diamond, J., Stoico, V. 2026. What is the cost of energy monitoring? An empirical study on the
  overhead of RAPL-based tools. arXiv:2604.26815 [preprint]. https://arxiv.org/abs/2604.26815
- Duplyakin, D. et al. 2023. Avoiding the ordering trap in systems performance measurement. USENIX
  ATC. https://www.usenix.org/system/files/atc23-duplyakin.pdf
- von Elm, C., Ilsche, T., Schöne, R., Bielert, M., Schmidl, M. 2021. Investigating the cause and
  effect of an AMD Zen energy management anomaly. ICPE Companion.
  https://doi.org/10.1145/3447545.3451193
- Fahad, M., Shahid, A., Manumachu, R. R., Lastovetsky, A. 2019. A comparative study of methods for
  measurement of energy of computing. Energies 12(11). https://doi.org/10.3390/en12112204
- Fischer, R. 2026. Ground-truthing AI energy consumption: validating CodeCarbon against external
  measurements. it – Information Technology 67(4-6). https://doi.org/10.1515/itit-2025-0031
- Fleming, M. et al. 2023. Hunter: using change point detection to hunt for performance
  regressions. ICPE. https://doi.org/10.1145/3578244.3583719
- Georges, A., Buytaert, D., Eeckhout, L. 2007. Statistically rigorous Java performance evaluation.
  OOPSLA. https://doi.org/10.1145/1297027.1297033
- Guilloteau, Q., Ciorba, F. M., Poquet, M., Goepp, D., Richard, O. 2024. Longevity of artifacts in
  leading parallel and distributed systems conferences: a review of the state of the practice in
  2023. ACM REP. https://doi.org/10.1145/3641525.3663631
- Hähnel, M., Döbel, B., Völp, M., Härtig, H. 2012. Measuring energy consumption for short code
  paths using RAPL. ACM SIGMETRICS Performance Evaluation Review 40(3).
  https://doi.org/10.1145/2425248.2425252
- He, H., Friedman, M., Rekatsinas, T. 2023. EnergAt: fine-grained energy attribution for
  multi-tenancy. HotCarbon. https://doi.org/10.1145/3604930.3605716
- Hernandez, O., Elwasif, W. 2025. Fine-grained application energy and power measurements on the
  Frontier exascale system. Cray User Group. https://doi.org/10.1145/3757348.3757363
- Hoefler, T., Belli, R. 2015. Scientific benchmarking of parallel computing systems: twelve ways to
  tell the masses when reporting performance results. SC. https://doi.org/10.1145/2807591.2807644
- Hort, M., Kechagia, M., Sarro, F. 2026. The green software landscape: a systematic mapping study
  on evolution, applications, software lifecycle, and best practices. arXiv:2608.26229
  [preprint]. https://arxiv.org/abs/2608.26229
- Jay, M. et al. 2023. An experimental comparison of software-based power meters: focus on CPU and
  GPU. IEEE/ACM CCGrid. https://doi.org/10.1109/CCGrid57682.2023.00020
- Johari, R., Koomen, P., Pekelis, L., Walsh, D. 2022. Always valid inference: continuous
  monitoring of A/B tests. Operations Research 70(3). https://doi.org/10.1287/opre.2021.2135
- Kalibera, T., Jones, R. 2013. Rigorous benchmarking in reasonable time. ISMM.
  https://doi.org/10.1145/2464157.2464160
- Kavanagh, R., Djemame, K. 2019. Rapid and accurate energy models through calibration with IPMI
  and RAPL. Concurrency and Computation: Practice and Experience 31(13).
  https://doi.org/10.1002/cpe.5124
- van Kempen, N., Kwon, H.-J., Nguyen, D. T., Berger, E. D. 2025. It's not easy being green: on the
  energy efficiency of programming languages. ASE. https://doi.org/10.1109/ase63991.2025.00131
- Khaleghzadeh, H., Fahad, M., Shahid, A., Manumachu, R. R., Lastovetsky, A. 2019. Bi-objective
  optimisation of data-parallel applications on heterogeneous platforms for performance and energy
  via workload distribution. arXiv:1907.04080 [preprint]. https://arxiv.org/abs/1907.04080
- Khan, K. N., Hirki, M., Niemi, T., Nurminen, J. K., Ou, Z. 2018. RAPL in action: experiences in
  using RAPL for power measurements. ACM TOMPECS 3(2). https://doi.org/10.1145/3177754
- Khrouf, L., Shatnawi, A., Rouvoy, R. 2026. Can we spot energy regressions using developers'
  tests? An industrial replication. ICPC. https://doi.org/10.1145/3794763.3798169
- von Kistowski, J. et al. 2016. Variations in CPU power consumption. ICPE.
  https://doi.org/10.1145/2851553.2851567
- Koszczał, G., Matuszek, M., Czarnul, P. 2025. Comparison and analysis of software and hardware
  energy measurement methods for a CPU+GPU system and selected parallel applications. Computer
  Science and Information Systems 22(2). https://doi.org/10.2298/CSIS240722023K
- Laaber, C., Scheuner, J., Leitner, P. 2019. Software microbenchmarking in the cloud. How bad is it
  really? Empirical Software Engineering. https://doi.org/10.1007/s10664-019-09681-1
- Laaber, C., Würsten, S., Gall, H. C., Leitner, P. 2020. Dynamically reconfiguring software
  microbenchmarks: reducing execution time without sacrificing result quality. ESEC/FSE.
  https://doi.org/10.1145/3368089.3409683
- Leon, E. A., Karlin, I., Moody, A. T. 2016. System noise revisited: enabling application
  scalability and reproducibility with SMT. IPDPS. https://doi.org/10.1109/IPDPS.2016.48
- Lin, W. 2013. Agnostic notes on regression adjustments to experimental data: reexamining
  Freedman's critique. Annals of Applied Statistics 7(1). https://doi.org/10.1214/12-AOAS583
- Lipp, M. et al. 2021. PLATYPUS: software-based power side-channel attacks on x86. IEEE S&P.
  https://doi.org/10.1109/SP40001.2021.00063
- Lo, D., Cheng, L., Govindaraju, R., Ranganathan, P., Kozyrakis, C. 2015. Heracles: improving
  resource efficiency at scale. ISCA. https://doi.org/10.1145/2749469.2749475
- Lober, M. et al. 2026. NUMA balancing hampering performance of spiking network simulations.
  arXiv:2607.22275 [preprint]. https://arxiv.org/abs/2607.22275
- Maquoi, J., Bechet, F., Cruz, L., Vanderose, B., Devroey, X. 2026. Systematic detection of energy
  regressions in software version histories. arXiv:2610.19895 [preprint].
  https://arxiv.org/abs/2610.19895
- Marathe, A. et al. 2017. An empirical survey of performance and energy efficiency variation on
  Intel processors. E2SC workshop at SC. https://doi.org/10.1145/3149412.3149421
- Maricq, A., Duplyakin, D., Jimenez, I., Maltzahn, C., Stutsman, R., Ricci, R. 2018. Taming
  performance variability. OSDI. https://www.usenix.org/conference/osdi18/presentation/maricq
- Matteson, D. S., James, N. A. 2014. A nonparametric approach for multiple change point analysis
  of multivariate data. JASA. https://doi.org/10.1080/01621459.2013.849605
- Mytkowicz, T., Diwan, A., Hauswirth, M., Sweeney, P. F. 2009. Producing wrong data without doing
  anything obviously wrong! ASPLOS. https://doi.org/10.1145/1508244.1508275
- de Oliveira, D. B., Casini, D., Cucinotta, T. 2023. Operating system noise in the Linux kernel.
  IEEE Transactions on Computers 72(1). https://doi.org/10.1109/TC.2022.3187351
- Ournani, Z., Belgaid, M. C., Rouvoy, R., Rust, P., Penhoat, J., Seinturier, L. 2020. Taming
  energy consumption variations in systems benchmarking. ICPE. https://doi.org/10.1145/3358960.3379142
- Ournani, Z., Belgaid, M. C., Rouvoy, R., Rust, P., Penhoat, J. 2021. Evaluating the impact of
  Java virtual machines on energy consumption. ESEM. https://doi.org/10.1145/3475716.3475774
- Paolillo, A., Van Molle, M., Hasselmann, K. 2026. benchkit: a declarative framework for
  composable performance evaluation of system software. ICPE. https://doi.org/10.1145/3777884.3796997
- Petrini, F., Kerbyson, D. J., Pakin, S. 2003. The case of the missing supercomputer performance:
  achieving optimal performance on the 8,192 processors of ASCI Q. SC.
  https://doi.org/10.1145/1048935.1050204
- Pijnacker, B., Setz, B., Andrikopoulos, V. 2025. Container-level energy observability in
  Kubernetes clusters. ICT4S. https://doi.org/10.1109/ict4s68164.2025.00016
- Raffin, G., Trystram, D. 2025. Dissecting the software-based measurement of CPU energy
  consumption: a comparative analysis. IEEE TPDS 36(1). https://doi.org/10.1109/TPDS.2024.3492336
- Rohr, D., Nešković, G., Lindenstruth, V. 2015. The L-CSC cluster: optimizing power efficiency to
  become the greenest supercomputer in the world in the Green500 list of November 2014.
  Supercomputing Frontiers and Innovations 2(3). https://doi.org/10.14529/jsfi150304
- Schöne, R., Ilsche, T., Bielert, M., Gocht, A., Hackenberg, D. 2019. Energy efficiency features of
  the Intel Skylake-SP processor and their impact on performance. HPCS.
  https://doi.org/10.1109/HPCS48598.2019.9188239
- Schöne, R. et al. 2021. Energy efficiency aspects of the AMD Zen 2 architecture. IEEE CLUSTER.
  https://doi.org/10.1109/CLUSTER48925.2021.00087
- Schuchart, J. et al. 2016. The shift from processor power consumption to performance variations:
  fundamental implications at scale. Computer Science – Research and Development 31(4).
  https://doi.org/10.1007/s00450-016-0327-2
- Simakov, N. A. et al. 2018. Effect of Meltdown and Spectre patches on the performance of HPC
  applications. arXiv:1801.04329 [preprint]. https://arxiv.org/abs/1801.04329
- Sinha, P. et al. 2022. Not all GPUs are created equal: characterizing variability in large-scale,
  accelerator-rich systems. SC. https://doi.org/10.1109/SC41404.2022.00070
- Thamm, P., Leser, U. 2025. Strategies to measure energy consumption using RAPL during workflow
  execution on commodity clusters. arXiv:2505.09375 [preprint]. https://arxiv.org/abs/2505.09375
- Thamm, P. et al. 2026. Nf-PEAK: process-based energy attribution for Nextflow workflows on
  Kubernetes clusters. arXiv:2605.22393 [preprint]. https://arxiv.org/abs/2605.22393
- Traini, L., Cortellessa, V., Di Pompeo, D., Tucci, M. 2022. Towards effective assessment of steady
  state performance in Java software: are we there yet? Empirical Software Engineering 28(1).
  https://doi.org/10.1007/s10664-022-10247-x
- Wang, Y., Nörtershäuser, D., Le Masson, S., Menaud, J.-M. 2018. Potential effects on server power
  metering and modeling. Wireless Networks. https://doi.org/10.1007/s11276-018-1882-1
- Wang, Y., Nörtershäuser, D., Le Masson, S., Menaud, J.-M. 2019. Experimental characterization of
  variation in power consumption for processors of different generations. IEEE GreenCom.
  https://doi.org/10.1109/iThings/GreenCom/CPSCom/SmartData.2019.00133
- Wang, Y. et al. 2022. Hertzbleed: turning power side-channel attacks into remote timing attacks
  on x86. USENIX Security. https://www.usenix.org/system/files/sec22-wang-yingchen.pdf
- Weigell, B., Hornung, S., Bauer, B. 2026. METRION: a framework for accurate software energy
  measurement. GREENS workshop at ICSE. https://doi.org/10.1145/3786148.3788635
- Yang, Z., Adamek, K., Armour, W. 2024. Accurate and convenient energy measurements for GPUs: a
  detailed study of NVIDIA GPU's built-in power sensor. SC. https://doi.org/10.1109/SC41406.2024.00028
- Zapater, M. et al. 2015. Leakage-aware cooling management for improving server energy efficiency.
  IEEE TPDS 26(10). https://doi.org/10.1109/TPDS.2014.2361519
