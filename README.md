# minion

Two tools for running builds, tests and experiments on other machines:

- **gru**, on the laptop: runs a repo's own build, test and experiment commands here or on an ssh
  host. It syncs only changed files, runs each task as a detached job that survives disconnects,
  notifies when it ends and fetches the results. [GRU.md](docs/GRU.md)
- **minion**, on the measured host: applies machine settings for a measurement, puts the
  originals back (also after a crash) and runs queues of measured runs. gru ships and uploads it;
  `gru setup` gives it root. [minion/README.md](minion/README.md), [MINION.md](docs/MINION.md)

How they fit together: [DESIGN.md](docs/DESIGN.md). Sources: [RESEARCH.md](docs/RESEARCH.md).

## Use

```sh
make install        # gru -> ~/.local/bin, its agents and minion -> ~/.local/libexec/gru
gru help            # the commands
gru run <task>      # a task of this repo's template, on its host
```

The config is `~/.config/gru/config.json`, plus an optional `.gru.json` per project
([GRU.md](docs/GRU.md#configuration)). A host needs only a POSIX shell.

## Develop

```sh
nix develop         # Go and Rust, from flake.nix
make build          # -> gru/bin: gru, its Linux agents and minion's static Linux builds
make dist           # the release archives, in dist/
make test           # go test + vet, cargo test + clippy
make e2e            # gru + minion end to end on this machine (Linux only)
```

Without nix, minion's builds need `rustup target add x86_64-unknown-linux-musl
aarch64-unknown-linux-musl`. `make install` also copies the AI skill (docs/ai/SKILL.md) to
`~/.claude/skills/gru` if that dir exists.

`make e2e` (test/e2e.sh) runs real queues and every gru command and option, with no ssh and no
root: `test/ssh` runs the "remote" commands here with their own home, `test/sudo` and
`test/visudo` stand in for sudo (as this user, with requiretty) and visudo (classic sudo's rules,
or sudo-rs's), and minion works on a
fake sysfs and powercap tree. Everything lives in one temp dir (`E2E_KEEP=1` keeps it). CI runs
`make test e2e` on every pull request. It can't cover real RAPL and cpufreq writes, real sudo, real
ssh or several hosts: test those on a real machine.
