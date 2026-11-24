mod engine;
mod ffi;
mod journal;
mod lock;
mod noise;
mod perf;
mod queue;
mod run;
mod serve;
mod settings;
#[cfg(test)]
mod tests;
mod utc;

use std::io;
use std::path::{Path, PathBuf};
use std::process::ExitCode;

use serde_json::json;

use crate::ffi::geteuid;
use crate::serve::{NEEDS_ROOT, Root};

const NAME: &str = env!("CARGO_PKG_NAME");
/// gru's version, which its build passes in: minion ships with gru.
const VERSION: &str = match option_env!("MINION_VERSION") {
    Some(v) => v,
    None => "dev",
};
const USAGE: &str = concat!(
    "usage: ",
    env!("CARGO_PKG_NAME"),
    " <verb> [--sysfs DIR] [--state-dir DIR] [--lock FILE] [--lib DIR] [--out FILE] [Q.json] [-- CMD...]

  snapshot   print the current settings as JSON
  restore    put the original settings back and close the journal, unless a queue runs
  run        run CMD, reading the meters right before and after it, into --out (default run.json)
  queue      run every run of the queue in Q.json under its profile, then restore
  reap       kill the leftover workloads of a queue that died (as the user, never as root)
  serve      the root side: apply and restore requests on stdin, then restore

restore and a queue that changes settings start `sudo -n <root copy> serve`, which `gru setup`
allows; without it they exit 77.

--sysfs, --state-dir, --lock and --lib (where the root copies are) are for tests and are refused
as root."
);

#[derive(Clone)]
struct Opts {
    sysfs: PathBuf,
    state_dir: PathBuf,
    lock: PathBuf,
    lib: PathBuf,
    out: PathBuf,
    args: Vec<String>,
    /// The test overrides given, which serve gets too.
    test: Vec<String>,
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
        Some(i) if verb == "run" => (&rest[..i], &rest[i + 1..]),
        Some(_) => return Err(format!("-- CMD is only for run\n\n{USAGE}")),
        None => (rest, &[][..]),
    };
    let o = parse_opts(verb, opts, geteuid())?;
    match (verb.as_str(), &o.args[..]) {
        ("run", []) => return run::run(&o.sysfs, &o.out, cmd),
        ("queue", [file]) => return queue::run(&o, Path::new(file)),
        (_, []) => {}
        _ => return Err(USAGE.into()),
    }
    match verb.as_str() {
        "snapshot" => println!("{:#}", json!(settings::snapshot(&o.sysfs)?)),
        "serve" => serve::serve(&o, io::stdin().lock(), io::stdout().lock())?,
        "restore" => {
            if let Some(q) = lock::held_by(&o.lock)? {
                println!("queue {q:?} holds the queue lock and restores at its end: nothing done");
                return Ok(0);
            }
            let Some(mut root) = Root::start(&o) else {
                return Ok(NEEDS_ROOT);
            };
            println!("{}", root.ask("restore")?);
        }
        "reap" => {
            let n = lock::reap(&o.lock)?;
            utc::log(format!("killed {n} leftover workload processes"));
        }
        _ => return Err(USAGE.into()),
    }
    Ok(0)
}

/// Root must not let a caller point its reads and writes at other files.
fn parse_opts(verb: &str, args: &[String], euid: u32) -> Result<Opts, String> {
    if euid == 0 && ["run", "queue", "reap"].contains(&verb) {
        return Err(format!(
            "{verb} is refused as root: it runs or kills your processes"
        ));
    }
    let mut o = Opts {
        sysfs: "/sys".into(),
        state_dir: ["/var/lib", NAME].iter().collect(),
        lock: format!("/run/lock/{NAME}.lock").into(),
        lib: "/usr/local/lib/gru".into(),
        out: "run.json".into(),
        args: vec![],
        test: vec![],
    };
    let mut it = args.iter();
    while let Some(flag) = it.next() {
        let field = match flag.as_str() {
            "--sysfs" => &mut o.sysfs,
            "--state-dir" => &mut o.state_dir,
            "--lock" => &mut o.lock,
            "--lib" => &mut o.lib,
            "--out" if verb == "run" => &mut o.out,
            a if !a.starts_with('-') => {
                o.args.push(a.into());
                continue;
            }
            _ => return Err(format!("{verb} has no option {flag:?}\n\n{USAGE}")),
        };
        if euid == 0 {
            return Err(format!("{flag} is refused as root"));
        }
        let value = it.next().ok_or(format!("{flag} needs a value"))?;
        *field = value.into();
        if flag != "--out" {
            o.test.extend([flag.clone(), value.clone()]);
        }
    }
    Ok(o)
}
