# minion

Two tools for running builds, tests and experiments on remote machines:

- **gru**, on the laptop: a thin remote dev-loop orchestrator. It dispatches a repo's
  build/test/experiment commands — locally or on a remote SSH server — reusing each repo's
  existing build tooling. It syncs only modified files, runs jobs in detached sessions that
  survive disconnects, notifies on failure from the remote side, and fetches results back. One
  uniform interface across every repo, driven from the terminal, Emacs, or an AI agent.
- **minion**, on the measured host: sets machine settings for a measurement, puts the originals
  back (also after a crash), and runs queues of measured runs (see [minion/README.md](minion/README.md)).

Design: [DESIGN.md](docs/DESIGN.md) for both tools, how they connect and the repo layout, then
[GRU.md](docs/GRU.md), [MINION.md](docs/MINION.md) and [RESEARCH.md](docs/RESEARCH.md).

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
`.gru.json`). See [GRU.md](docs/GRU.md).

## Develop

```sh
nix develop        # Go and Rust, from flake.nix
make build         # -> gru/bin/gru and minion/target/release/minion
make install       # gru -> ~/.local/bin, its agents -> ~/.local/libexec/gru, the skill -> ~/.claude/skills/gru if present
make test          # go test + vet in gru/, cargo test + clippy in minion/
make e2e           # gru + minion end to end on this machine (test/e2e.sh, Linux only)
```

Each folder also builds on its own: `make -C gru build` (or `make` targets inside `gru/`), and
`cargo build` / `cargo test` inside `minion/`. `make install` doesn't install minion: copy it to
each measured host, into a root-owned dir on sudo's `secure_path`.

`make e2e` runs real queues and every gru command and option through `gru run` with no ssh and no
root: `test/ssh` stands in for ssh and runs the "remote" commands here, with their own home, and
the runner works on a fake sysfs and powercap tree. Everything lives in one temp dir (`E2E_KEEP=1`
keeps it). CI runs `make test e2e` on every pull request. It can't cover real RAPL and cpufreq
writes, sudo, real ssh, email or several hosts: test those on a real machine.
