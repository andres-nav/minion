---
name: gru
description: >
  Use gru to build, test, and run tasks/experiments for a repo — locally or
  on a remote SSH host (a build server, a GPU box). Invoke whenever you would
  run a build/test command in a repo, or need to sync code to and run something
  on a remote and get results back. gru is the uniform entry point: prefer
  `gru run <task>` (e.g. `run build`, `run test`) over calling make, cargo,
  latexmk, pytest, etc. directly. Triggers: build, test, run, sync, deploy to a
  remote or GPU host, run experiment, fetch results, attach to a job.
---

# gru

`gru` is a thin dispatcher: it syncs a repo's modified files to a host, runs
a task there (delegating to the repo's real build tool), streams output, and
fetches results. Local and remote use the same model. Zero-config repos work if
a global template matches them.

## Always start by discovering the setup

```
gru list --json
```

This prints the resolved `template`, `host`, `remote_dir`, and available
`tasks` (name → command) for the current directory. **Rules:**
- If it shows tasks → use gru for build/test/run here.
- If `tasks` is empty (plain `gru list` says "no tasks for …") → this repo has
  no gru setup; fall back to the repo's own commands. Do NOT invent a gru config.
- Run it from the package/project dir (gru auto-detects the project root).

## Core commands

- `gru run <task>` — run any task by name: `run build`, `run test`,
  `run sweep`. (There are no bare `build`/`test` commands; everything goes
  through `run`.)
- `gru run <task> --host <name>` — override where it runs: a hosts-map name,
  an ssh alias, or `localhost` (this machine, in place, no ssh). Default is
  the template/repo's host.
- `gru run <task> --detach` — launch without attaching; prints the job id.
- `gru run <task> --arg 'TEXT'` — pass TEXT to the task as `$GRU_ARGS`.
- `gru run <task> --no-sync` — run the code already on the host.
- `gru sync [--delete]` — push modified files only (git-aware); no run.
  Don't use `--delete` unless asked.
- `gru fetch [<id>] [--into DIR]` — copy the files matching `fetch` from the
  remote project dir as they are now (into `fetch_into` if set, else the
  project dir); the id names `{id}`/`{ts}` and adds that job's `run.json` and
  log under `_gru/`.
- `gru attach [<id>]` — stream a job's output (default: this project's latest);
  exits with the job's exit code.
- `gru ps [--all]` — list this project's jobs (name/task/started/status);
  `--all` for every job on the host.
- `gru kill <id>` — stop a running job (SIGTERM; its hooks still run).
- `gru ssh` — interactive shell on the host, in the project's remote dir.
- `gru doctor [--notify]` — check config + hosts (reachable, agent, minion,
  and whether minion has root there); `--notify` also runs a test
  notification through the notify `run` hooks.
- `gru minion <args> --host <name>` — run a minion command on the host, e.g.
  `gru minion snapshot` or `gru minion restore`.
- `gru setup --host <name>` — give minion root on a host, once per gru
  build. It asks for the user's sudo password, so it needs a terminal:
  never run it yourself; ask the user to.

- `gru upgrade` — install gru's latest release, which also updates this skill.
  Only when the user asks.

Flags can follow the positional arg (`gru attach my_job --host gpu`); a
second positional is a usage error.

## How runs work

- A `gru run <task>` on a remote host: **sync → launch detached → attach
  (live output) → auto-fetch results**. It survives disconnects; hooks run on
  the remote. gru uploads itself and minion there on first use.
- Jobs get friendly ids (`brave_otter-20261123T142501Z`, the start time in
  UTC). Inspect state with `gru ps`, watch with `gru attach`, stop with
  `gru kill`.
- Exit codes propagate: `gru run <task>` returns the task's exit code. Read a
  failed job's log with `gru attach <id>`.
- Results land where `fetch_into` says (e.g. `results/{ts}-{id}/`, with `{ts}`
  the start time in UTC: `20261123T142501Z`), so each run is preserved;
  without it they land in the project dir, replacing the last fetch's.

## Config model (only if asked to change it)

- Global: `~/.config/gru/config.json` — a library of `hosts` and `templates`
  (repo-types). Written once; repos need no files.
- Per-repo: an optional `.gru.json` that picks a template via `"use"` and
  overrides any field (unknown keys are refused).
- A template maps `tasks` (name → command) to real commands, plus environment:
  `detect` (markers), `host`, `remote_dir`, `needs` (sibling dirs to sync
  first), `push` (local dirs to copy first, e.g. a tool's token dir),
  `exclude`, `env` (`KEY=VALUE` entries), `before`/`on_success`/`on_failure`/
  `always` hooks, `fetch` (result globs), `fetch_into` (`{id}`/`{ts}`/`{repo}`
  placeholders → per-run dirs), `notify` (`on`, `run` commands, `env`).
- To add a capability: add a task to the template (or a per-repo `.gru.json`
  override) — do not hardcode the underlying build command in your own calls.

## Guidance for AI use

- In a gru repo, run tasks **through gru**, not by invoking the build tool
  yourself: running `make` or `cargo` directly skips the host choice, the sync
  and the fetch.
- If a remote op says "no host configured", pass `--host` (use `--host
  localhost` to run locally as a managed job).
- If a task exits 77 with "minion needs root on <host>: run `gru setup --host
  <host>` once in a terminal", stop and ask the user to run that command, then
  run the task again. Don't retry before that, and don't call sudo yourself.
