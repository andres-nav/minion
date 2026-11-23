---
name: gru
description: >
  Use gru to build, test, and run tasks/experiments for a repo — locally or
  on a remote SSH host (dev desktop, GPU box). Invoke whenever you would run a
  build/test command in a repo, or need to sync code to and run something on a
  remote and get results back. gru is the uniform entry point: prefer
  `gru run <task>` (e.g. `run build`, `run test`) over calling brazil-build,
  make, latexmk, pytest, etc. directly. Triggers: build, test, run, sync,
  deploy to remote/dev-dsk/GPU, run experiment, fetch results, attach to a job.
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
- If `tasks` is empty ("no tasks for …") → this repo has no gru setup; fall
  back to the repo's own commands. Do NOT invent a gru config.
- Run it from the package/project dir (gru auto-detects the project root).

## Core commands

- `gru run <task>` — run any task by name: `run build`, `run test`,
  `run sweep`. (There are no bare `build`/`test` commands; everything goes
  through `run`.)
- `gru run <task> --host <name>` — override where it runs (a hosts-map name,
  an ssh alias, or `localhost`). Default is the template/repo's host.
- `gru run <task> --host localhost` — run on this machine (in place, no ssh).
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
- `gru doctor [--notify]` — check config + hosts (reachable + agent);
  `--notify` also sends a test notification (email and/or run hooks).

Flags can follow the positional arg (`gru attach my_job --host gpu`).

## How runs work

- A `gru run <task>` on a remote host: **sync → launch detached → attach
  (live output) → auto-fetch results**. It survives disconnects; hooks run on
  the remote.
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
  `detect` (markers), `host`, `remote_dir`, `needs` (sibling packages to sync
  first), `push` (local dirs to copy first, e.g. `~/.midway`), `exclude`,
  `env`, `before`/`on_success`/`on_failure`/`always` hooks, `fetch` (result
  globs), `fetch_into` (`{id}`/`{ts}`/`{repo}` placeholders → per-run dirs).
- To add a capability: add a task to the template (or a per-repo `.gru.json`
  override) — do not hardcode the underlying build command in your own calls.

## Guidance for AI use

- In a gru repo, run tasks **through gru**, not by invoking the build
  tool yourself — gru handles which host, syncing the edited files, and
  fetching outputs. Running `brazil-build`/`make` directly bypasses all that.
- To run on the dev desktop / a GPU box, just use the configured host (or
  `--host <name>`); gru bootstraps itself there automatically.
- After a run, check `gru ps` and `gru attach <id>` for status/logs;
  parse `gru list --json` for machine-readable task discovery.
- If a remote op says "no host configured", pass `--host` (use `--host
  localhost` to run locally as a managed job).
