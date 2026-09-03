---
name: minion
description: >
  Use minion to build, test, and run tasks/experiments for a repo — locally or
  on a remote SSH host (dev desktop, GPU box). Invoke whenever you would run a
  build/test command in a repo, or need to sync code to and run something on a
  remote and get results back. minion is the uniform entry point: prefer
  `minion build`/`minion test`/`minion run <task>` over calling brazil-build,
  make, latexmk, pytest, etc. directly. Triggers: build, test, run, sync,
  deploy to remote/dev-dsk/GPU, run experiment, fetch results, attach to a job.
---

# minion

`minion` is a thin dispatcher: it syncs a repo's modified files to a host, runs
a task there (delegating to the repo's real build tool), streams output, and
fetches results. Local and remote use the same model. Zero-config repos work if
a global template matches them.

## Always start by discovering the setup

```
minion list --json
```

This prints the resolved `template`, `host`, `remote_dir`, and available
`tasks` (name → command) for the current directory. **Rules:**
- If it shows a `template` and tasks → use minion for build/test/run here.
- If `template` is empty ("no template matched") → this repo has no minion
  setup; fall back to the repo's own commands. Do NOT invent a minion config.
- Run it from the package/project dir (minion auto-detects the project root).

## Core commands

- `minion build` / `minion test` — run the template's build/test task.
- `minion run <task>` — run any task by name (e.g. `run lint`, `run sweep`).
- `minion build --host <name>` — override where it runs (a hosts-map name, an
  ssh alias, or `localhost`). Default is the template/repo's host.
- `minion build --host localhost` — run on this machine (in place, no ssh).
- `minion run <task> --detach` — launch without attaching; prints the job id.
- `minion sync [--delete]` — push modified files only (git-aware); no run.
- `minion fetch [<id>]` — pull result files back (into `fetch_into` if set).
- `minion attach [<id>]` — stream a job's output (default: latest); exits with
  the job's exit code.
- `minion ps [--all]` — list this package's jobs (name/task/started/status);
  `--all` for every job on the host.
- `minion kill <id>` — stop a running job.
- `minion ssh` — interactive shell on the host, in the project's remote dir.
- `minion doctor` — validate config + check hosts (reachable + agent).

Flags can follow the positional arg (`minion attach my_job --host gpu`).

## How runs work

- A `build`/`test`/`run` on a remote host: **sync → launch detached → attach
  (live output) → auto-fetch results**. It survives disconnects; hooks run on
  the remote.
- Jobs get friendly ids (`brave_otter`). Inspect state with `minion ps`, watch
  with `minion attach`, stop with `minion kill`.
- Exit codes propagate: `minion build` returns the task's exit code. Read a
  failed job's log with `minion attach <id>`.
- Results land where `fetch_into` says (e.g. `results/{ts}-{id}/`), so each run
  is preserved.

## Config model (only if asked to change it)

- Global: `~/.config/minion/config.json` — a library of `hosts` and `templates`
  (repo-types). Written once; repos need no files.
- Per-repo: an optional `.minion.json` that picks a template via `"use"` and
  overrides any field.
- A template maps standard verbs (`build`/`test`/`run`/`lint`/`clean`) and
  freeform `tasks` to real commands, plus environment:
  `detect` (markers), `host`, `remote_dir`, `needs` (sibling packages to sync
  first), `push` (local dirs to copy first, e.g. `~/.midway`), `exclude`,
  `env`, `before`/`on_success`/`on_failure`/`always` hooks, `fetch` (result
  globs), `fetch_into` (`{id}`/`{ts}`/`{repo}` placeholders → per-run dirs).
- To add a capability: add a task to the template (or a per-repo `.minion.json`
  override) — do not hardcode the underlying build command in your own calls.

## Guidance for AI use

- In a minion repo, run tasks **through minion**, not by invoking the build
  tool yourself — minion handles which host, syncing the edited files, and
  fetching outputs. Running `brazil-build`/`make` directly bypasses all that.
- To run on the dev desktop / a GPU box, just use the configured host (or
  `--host <name>`); minion bootstraps itself there automatically.
- After a run, check `minion ps` and `minion attach <id>` for status/logs;
  parse `minion list --json` for machine-readable task discovery.
- If a remote op says "no host configured", pass `--host` (use `--host
  localhost` to run locally as a managed job).
