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

minion needs what gru must not: root for machine settings (through one sudoers rule), later NVML
(a C library), and control over every wakeup on the host. Rust has no runtime threads or garbage
collector, while an idle Go program still wakes every minute or two
([RESEARCH.md](RESEARCH.md#2-linux-mechanisms)). Kept apart, gru stays small. gru ships minion's
static Linux builds and is the only way to a host: nobody installs minion by hand.

## How they connect

minion knows gru only through environment variables; without gru it runs the same under `setsid`
or tmux, minus the hooks. Setup: [minion/README.md](../minion/README.md#with-gru).

- **gru carries minion.** Like its agent, gru uploads minion's build for the host to
  `<state_dir>/bin/minion-<ver>-<os>-<arch>-<sha12>` (each build its own file, checked by
  sha256) and gives its path to tasks and hooks in `GRU_MINION`; minion's version is gru's.
- **Root, once per host and build.** `gru setup --host X`, in a terminal, asks for the sudo
  password once and installs a root-owned copy of that build in `/usr/local/lib/gru` and a
  sudoers drop-in that allows only `minion-* serve`; `--remove` takes both away. A queue's root
  side is one `minion serve` that it talks to over a pipe and that restores when the pipe closes
  ([MINION.md](MINION.md#settings-and-root)). Without the setup, a queue that changes settings
  exits 77 before touching anything, and `gru run` in a terminal offers the setup and runs the
  task again. `gru minion <verb> --host X` runs any other minion verb there, such as `restore`.

- **The job runs the queue.** `gru run energy --arg q.json --detach` starts a detached gru job
  whose task is `"$GRU_MINION" queue $GRU_ARGS`. The queue runs in the job's foreground, so `gru ps`,
  `attach`, `kill` and the log apply to it; `gru kill` sends `SIGTERM`, and the queue restores
  before it exits.
- **Notify.** minion's `notify.cmd` calls gru's agent, which reads the 0600 notify file in the job
  dir, so secrets stay out of the queue and the environment: `"$GRU_AGENT" _agent notify --config
  "$GRU_NOTIFY" --subject "$MINION_EVENT $MINION_QUEUE" --body "$MINION_DETAIL"`.
- **Results and fetch.** minion writes each queue under its `out` dir, relative to the job's
  directory (`remote_dir`), and records the job's `GRU_*` variables (job id, task, host, commit,
  dirty flag) in `queue.json`. A `fetch` glob over `out` brings it back; an attached `gru run` and
  `gru fetch <id>` also bring the job's `run.json` and log into `_gru/`.
- **Reap hook.** The template's `always` hook, `"$GRU_MINION" reap`, kills what the workloads of
  a runner that died left running; it spares the workloads of the queue that holds the lock. The
  dead runner's serve has already restored.

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
gru/      gru, the Go CLI: go.mod, main.go, internal/, Makefile
minion/   minion, the Rust runner: Cargo.toml, src/, README.md (usage)
test/     make e2e: gru and minion end to end, with no ssh and no root
docs/     this design, GRU.md, MINION.md, RESEARCH.md, and ai/SKILL.md (the AI skill for gru)
```

The root `Makefile` builds and tests both (`make build`, `make test`, `make e2e`); `flake.nix`
gives Go and Rust.

## Status and plan

- **gru**: v1 works, with the changes minion needed merged (#3, #6, #7, #8, #10): a state dir
  per host, unique UTC job ids, an agent file per build, quiet attach, provenance, notify for tasks.
- **minion**: the state engine, `run` and `queue` with the queue lock, reap and notifications are
  merged ([what is built](MINION.md#what-is-built)), and so are `minion serve` and `gru setup`.
  `make e2e` runs them through gru on a fake sysfs in CI, with stand-ins for ssh and sudo; real
  RAPL and cpufreq writes, real sudo and visudo, real ssh and several hosts still need a real
  machine.
- **Next** ([plan](MINION.md#plan)): measurement quality (meters, noise record, isolation, more
  settings); the opt-in machine lock; several machines; GPU and device state; a boot unit; the
  RQ3 analysis. Open questions: [MINION.md](MINION.md#open-questions).
