mod engine;
mod journal;
mod queue;
mod run;
mod settings;
#[cfg(test)]
mod tests;
mod utc;

use std::io;
use std::path::{Path, PathBuf};
use std::process::ExitCode;

use serde_json::{Value, json};

const NAME: &str = env!("CARGO_PKG_NAME");
const USAGE: &str = concat!(
    "usage: ",
    env!("CARGO_PKG_NAME"),
    " <verb> [--sysfs DIR] [--state-dir DIR] [--lock FILE] [--out FILE] [--held] [Q.json] [-- CMD...]

  snapshot   print the current settings as JSON
  apply      apply a profile (JSON on stdin), saving the original settings first
  restore    put the original settings back and close the journal; does nothing while a queue
             holds the lock, as that queue restores at its start (--held: the caller is it)
  status     show the open journal and what differs from the original settings
  run        run CMD, reading the meters right before and after it, into --out (default run.json)
  queue      run every run of the queue in Q.json under its profile, then restore

--sysfs, --state-dir and --lock are for tests and are refused as root. With them, queue runs
apply and restore in-process instead of through sudo."
);

unsafe extern "C" {
    safe fn geteuid() -> u32;
}

struct Opts {
    sysfs: PathBuf,
    state_dir: PathBuf,
    lock: PathBuf,
    out: PathBuf,
    args: Vec<String>,
    /// The caller holds the machine lock.
    held: bool,
    /// Set by a test override: queue then calls the privileged verbs in-process.
    test: bool,
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match run(&args) {
        Ok(code) => ExitCode::from(code),
        Err(e) => {
            utc::log(e);
            ExitCode::FAILURE
        }
    }
}

fn run(args: &[String]) -> Result<u8, String> {
    let Some((verb, rest)) = args.split_first() else {
        return Err(USAGE.into());
    };
    let (opts, cmd) = match rest.iter().position(|a| a == "--") {
        Some(i) => (&rest[..i], &rest[i + 1..]),
        None => (rest, &[][..]),
    };
    let o = parse_opts(opts, geteuid())?;
    match (verb.as_str(), &o.args[..]) {
        ("run", []) => return run::run(&o.sysfs, &o.out, cmd, geteuid()),
        ("queue", [file]) => return queue::run(&o, Path::new(file), geteuid()),
        (_, []) => {}
        _ => return Err(USAGE.into()),
    }
    match verb.as_str() {
        "snapshot" => print(json!(settings::snapshot(&o.sysfs)?)),
        "apply" => {
            let profile =
                serde_json::from_reader(io::stdin().lock()).map_err(|e| format!("profile: {e}"))?;
            engine::apply(&o.sysfs, &o.state_dir, &profile)
        }
        "restore" => restore(&o),
        "status" => print(engine::status(&o.sysfs, &o.state_dir)?),
        _ => Err(USAGE.into()),
    }
    .map(|()| 0)
}

/// Root must not let a caller point its reads and writes at other files.
fn parse_opts(args: &[String], euid: u32) -> Result<Opts, String> {
    let mut o = Opts {
        sysfs: "/sys".into(),
        state_dir: ["/var/lib", NAME].iter().collect(),
        lock: format!("/run/lock/{NAME}.lock").into(),
        out: "run.json".into(),
        args: vec![],
        held: false,
        test: false,
    };
    let mut it = args.iter();
    while let Some(flag) = it.next() {
        let field = match flag.as_str() {
            "--sysfs" => &mut o.sysfs,
            "--state-dir" => &mut o.state_dir,
            "--lock" => &mut o.lock,
            "--out" => &mut o.out,
            "--held" => {
                o.held = true;
                continue;
            }
            a if !a.starts_with('-') => {
                o.args.push(a.into());
                continue;
            }
            _ => return Err(format!("unknown option {flag:?}\n\n{USAGE}")),
        };
        if euid == 0 {
            return Err(format!("{flag} is refused as root"));
        }
        *field = it.next().ok_or(format!("{flag} needs a value"))?.into();
        o.test |= flag != "--out";
    }
    Ok(o)
}

/// Restores, unless a queue holds the machine lock: a late `always` hook must not
/// undo the queue that started after a crash, which restores the leftover itself.
fn restore(o: &Opts) -> Result<(), String> {
    let _lock = if o.held {
        None
    } else {
        let lock = queue::try_lock(&o.lock)?;
        if lock.is_none() {
            let held = o.lock.display();
            utc::log(format!(
                "a queue holds {held}, so it restores: nothing done"
            ));
            return Ok(());
        }
        lock
    };
    for c in engine::restore(&o.sysfs, &o.state_dir)? {
        utc::log(format!("kept {c}: changed by someone else"));
    }
    Ok(())
}

fn print(v: Value) -> Result<(), String> {
    println!("{v:#}");
    Ok(())
}
