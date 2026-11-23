# minion

Two tools for running builds, tests and experiments on remote machines:

- **gru**, on the laptop: a thin remote dev-loop orchestrator. It dispatches a repo's
  build/test/experiment commands — locally or on a remote SSH server — reusing each repo's
  existing build tooling. It syncs only modified files, runs jobs in detached sessions that
  survive disconnects, notifies on failure from the remote side, and fetches results back. One
  uniform interface across every repo, driven from the terminal, Emacs, or an AI agent.
- **minion**, on the measured host: sets machine settings for a measurement, puts the originals
  back (also after a crash), and runs queues of measured runs (see [minion/README.md](minion/README.md)).

See [DESIGN.md](docs/DESIGN.md) for the full design.

## Status

gru v1 working, validated end-to-end against a real host. The full loop runs with
no tmux and nothing to install on the remote but a POSIX shell:

```
gru run <task>                    # build/test/lint/clean or a freeform task; local, or remote when a host is set
gru sync [--delete]               # git-aware, only modified files
gru fetch                         # pull result files back
gru attach <id> | ps | kill <id>  # manage detached remote jobs
gru ssh                           # interactive shell on the host, in the project dir
gru doctor [--notify]             # validate config + hosts; --notify fires a test notification (email and/or run hooks)
gru list [--json]                 # discover the resolved template + tasks
```

Config lives in `~/.config/gru/config.json` (+ optional per-project
`.gru.json`). See [DESIGN.md](docs/DESIGN.md).

## Layout

```
gru/      gru, the Go CLI: go.mod, main.go, internal/, Makefile
minion/   minion, the Rust runner (see minion/README.md)
docs/     design docs
```

## Develop

```sh
nix develop        # or: direnv allow  (uses flake.nix: Go and Rust)
make build         # -> gru/bin/gru and minion/target/release/minion
make install       # -> ~/.local/bin/gru
make test          # go test + vet in gru/, cargo test + clippy in minion/
```

Each folder also builds on its own: `make -C gru build` (or `make` targets inside `gru/`),
and `cargo build` / `cargo test` inside `minion/`.

## Testing

```sh
make test          # unit tests and lints of both
make e2e           # gru + minion end to end, on this machine (test/e2e.sh)
```

`make e2e` runs real queues through `gru run` with no ssh and no root: `test/ssh` stands in for
ssh and runs the "remote" commands here, and the runner works on a fake sysfs and powercap tree.
It checks the remote and `--host localhost` paths, `--detach`/`ps`/`attach`/`kill`, a crashed
runner, two queues on one machine, UTC names and times, exit codes and broken queue files, and
`minion snapshot` on the real `/sys`. Everything lives in one temp dir (`E2E_KEEP=1` keeps
it). CI runs it on every pull request. It can't cover real RAPL and cpufreq writes, sudo, real ssh
or several hosts: test those on a real machine.
