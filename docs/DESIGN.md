# Design: gru and minion

This repository holds two tools: one runs builds, tests and experiments on other machines, the
other measures them there without disturbing them. Energy is the first use case; the goals follow
the project task description (M. Friedman, CORES Lab, ETH Zurich, 2026).

| Tool | Where | Language | Does | Design |
|---|---|---|---|---|
| **gru** | laptop | Go, stdlib only | syncs a repo, runs its tasks as detached jobs on a host, notifies, fetches results | [GRU.md](GRU.md) |
| **minion** | measured host | Rust | applies and restores machine settings, runs queues of measured runs, reads the meters | [MINION.md](MINION.md) |

Sources and related tools: [RESEARCH.md](RESEARCH.md).

## Why two tools

gru is a user-level CLI with no dependencies: a host needs only a POSIX shell, and gru uploads its
own static agent. It works for any repo and any host, measured or not.

minion needs what gru must not: root for machine settings (through one sudoers rule), NVML (a C
library), and control over every wakeup on the host. Rust has no runtime threads or garbage
collector, while an idle Go program still wakes every minute or two (`runtime/proc.go`). Kept
apart, gru stays small, and minion is installed only where measurements run (it can move to its
own repository later).

## How they connect

minion knows gru only through environment variables; without gru it runs the same under `setsid`
or tmux, minus the hooks.

- **The job runs the queue.** `gru run energy --arg q.json --detach` starts a detached gru job
  whose task is `minion queue $GRU_ARGS`. The queue runs in the job's foreground, so `gru ps`,
  `attach`, `kill` and the log apply to it; `gru kill` stops the queue, which restores.
- **Notify.** minion's `notify.cmd` calls gru's agent, which reads the 0600 notify file in the job
  dir, so secrets stay out of the queue and the environment: `"$GRU_AGENT" _agent notify --config
  "$GRU_NOTIFY" --subject "$MINION_EVENT $MINION_QUEUE" --body "$MINION_DETAIL"`.
- **Results and fetch.** minion writes each queue under `<remote_dir>/results/` and records the
  job's `GRU_*` variables (job id, task, host, commit, dirty flag) in `queue.json`. A `fetch` glob
  brings it back, with the job's `run.json` and log in `_gru/`.
- **Reap and restore hooks.** The template's `always` hooks, `minion reap` and `sudo -n minion
  restore`, clean up after a runner that died without restoring. They act only while no queue
  holds the queue lock. Setup: [minion/README.md](../minion/README.md#with-gru).

## Principles

- **Nothing runs in a measured window.** Between the meter reads at a run's start and end, minion
  doesn't log, write, send or fork; notifications, helpers and sudo calls run between runs
  ([the rule](MINION.md#the-rule-stay-out-of-the-measured-window)).
- **Exact restore.** Every setting gets its original value back, also after `kill -9` or a crash,
  from an on-disk journal ([restore](MINION.md#restore-crashes-and-reboots)); reboots once the
  boot unit exists.
- **Any experiment.** A workload is any command and meters are plugins.
- **Several machines.** gru drives any number of hosts, and one experiment can span hosts, a
  leader driving followers over SSH ([later](MINION.md#several-machines-later-phase)).
- **GPUs and devices.** Devices are setting objects like CPUs, saved, applied and restored the
  same way ([devices](MINION.md#devices)).
- **All times UTC.** Experiments span machines and time zones, so local time is never used.
  Machine fields are epoch seconds or nanoseconds, and every wall-clock epoch has an ISO 8601
  `*_utc` twin (`2026-11-23T14:25:01Z`); monotonic times have none. Ids, dirs and file names use
  `20261123T142501Z`. Displays and log lines show UTC with the `Z`.

## Repository layout

```
gru/      gru, the Go CLI: go.mod, main.go, internal/, Makefile
minion/   minion, the Rust runner: Cargo.toml, src/, README.md (usage)
test/     make e2e: gru and minion end to end, with no ssh and no root
docs/     this design, GRU.md, MINION.md, RESEARCH.md, and ai/SKILL.md (the AI skill for gru)
```

The root `Makefile` builds and tests both (`make build`, `make test`, `make e2e`); `flake.nix`
gives Go and Rust.

## Status and plan

- **gru**: v1 works, with the changes minion needed merged: a state dir per host, unique UTC job
  ids, an agent per build, quiet attach, provenance and notify for tasks
  ([list](MINION.md#gru-changes)).
- **minion**: the state engine (governor, turbo, idle states), `run` with the `time` and `rapl`
  meters, and `queue` with the queue lock, reap and notifications are merged. `make e2e` runs them
  through gru on a fake sysfs in CI; real RAPL and cpufreq writes, sudo, real ssh and several
  hosts still need a real machine.
- **Next** ([plan](MINION.md#plan)): meters, noise record and isolation; the opt-in machine lock;
  several machines; GPU and device state; a boot unit; the RQ3 analysis. Open questions are in
  [MINION.md](MINION.md#open-questions).
