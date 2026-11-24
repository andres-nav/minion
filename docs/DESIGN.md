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

minion needs what gru must not: root for machine settings, later NVML (a C library), and control
over every wakeup on the host. Rust has no runtime threads or garbage collector, while an idle Go
program still wakes every minute or two ([RESEARCH.md](RESEARCH.md#2-linux-mechanisms)). Kept
apart, gru stays small. gru ships minion's static Linux builds and is the only way to a host:
nobody installs minion by hand.

## How they connect

minion knows gru only through environment variables; without gru it runs the same under `setsid`
or tmux, minus the hooks. The config snippet: [minion/README.md](../minion/README.md#with-gru).

- **gru carries minion.** gru uploads minion's build for the host, like its agent, and gives its
  path to tasks and hooks in `GRU_MINION` ([bootstrap](GRU.md#bootstrap-internalssh)).
- **Root, once per host and build.** `gru setup --host X` installs a root-owned copy of the build
  and a sudoers rule that allows only its `serve` ([GRU.md](GRU.md#root)). A queue's root side is
  one `minion serve`, which restores when the queue's pipe closes
  ([MINION.md](MINION.md#settings-and-root)). Without the setup a queue that changes settings
  exits 77 before touching anything, and an attached `gru run` offers the setup.
- **The job runs the queue.** A detached gru job runs `"$GRU_MINION" queue $GRU_ARGS` in its
  foreground, so `gru ps`, `attach`, `kill` and the log apply; `gru kill` sends SIGTERM and the
  queue restores before it exits.
- **Notify.** minion's `notify.cmd` calls gru's agent, which reads the 0600 notify file in the job
  dir, so secrets stay out of the queue and the environment.
- **Results.** minion writes each queue under its `out` dir in the job's directory, with the
  job's `GRU_*` variables in `queue.json`; a `fetch` glob brings it back.
- **Reap.** The template's `always` hook, `"$GRU_MINION" reap`, kills what a dead runner's
  workloads left running; the dead runner's serve has already restored.

## Principles

- **Nothing runs in a measured window.** Between the meter reads at a run's start and end, minion
  only starts the workload and waits; notifications and requests to serve run between runs
  ([the rule](MINION.md#stay-out-of-the-measured-window)).
- **Exact restore.** Every setting gets its original value back from an on-disk journal, also
  after `kill -9` or a crash, except values someone else changed, which are kept and reported
  ([journal](MINION.md#journal)). Reboots are covered once the boot unit exists.
- **Any experiment.** A workload is any command and meters are plugins.
- **Several machines and devices.** gru drives any number of hosts; experiments across hosts and
  GPU settings are [planned](MINION.md#planned).
- **All times UTC.** Experiments span machines and time zones, so local time is never used.
  Machine fields are epoch seconds or nanoseconds, and minion gives every wall-clock epoch an ISO
  8601 `*_utc` twin (`2026-11-23T14:25:01Z`); monotonic times have none. Ids, dirs and file names
  use `20261123T142501Z`. Displays and minion's log lines show UTC with the `Z`.

## Repository layout

```
gru/      gru, the Go CLI: go.mod, main.go, internal/
minion/   minion, the Rust runner: Cargo.toml, src/, README.md (usage)
test/     make e2e: gru and minion end to end, with no ssh and no root
docs/     this design, GRU.md, MINION.md, RESEARCH.md, and ai/SKILL.md (the AI skill for gru)
```

Build and test: [README.md](../README.md#develop).

## Status

gru works and was validated end to end against a real host. minion's built parts, what is
tested and the plan: [MINION.md](MINION.md#status). Open questions:
[MINION.md](MINION.md#open-questions).
