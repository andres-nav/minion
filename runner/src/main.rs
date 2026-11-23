mod engine;
mod journal;
mod settings;
#[cfg(test)]
mod tests;

use std::io;
use std::path::PathBuf;
use std::process::ExitCode;

use serde_json::{Value, json};

const NAME: &str = env!("CARGO_PKG_NAME");
const USAGE: &str = concat!(
    "usage: ",
    env!("CARGO_PKG_NAME"),
    " <verb> [--sysfs DIR] [--state-dir DIR]

  snapshot   print the current settings as JSON
  apply      apply a profile (JSON on stdin), saving the original settings first
  restore    put the original settings back and close the journal
  status     show the open journal and what differs from the original settings

--sysfs and --state-dir are for tests and are refused as root."
);

unsafe extern "C" {
    safe fn geteuid() -> u32;
}

struct Opts {
    sysfs: PathBuf,
    state_dir: PathBuf,
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match run(&args) {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("{NAME}: {e}");
            ExitCode::FAILURE
        }
    }
}

fn run(args: &[String]) -> Result<(), String> {
    let Some((verb, rest)) = args.split_first() else {
        return Err(USAGE.into());
    };
    let o = parse_opts(rest, geteuid())?;
    match verb.as_str() {
        "snapshot" => print(json!(settings::snapshot(&o.sysfs)?)),
        "apply" => {
            let profile =
                serde_json::from_reader(io::stdin().lock()).map_err(|e| format!("profile: {e}"))?;
            engine::apply(&o.sysfs, &o.state_dir, &profile)
        }
        "restore" => {
            for c in engine::restore(&o.sysfs, &o.state_dir)? {
                eprintln!("{NAME}: kept {c}: changed by someone else");
            }
            Ok(())
        }
        "status" => print(engine::status(&o.sysfs, &o.state_dir)?),
        _ => Err(USAGE.into()),
    }
}

/// Root must not let a caller point its reads and writes at other files.
fn parse_opts(args: &[String], euid: u32) -> Result<Opts, String> {
    let mut o = Opts {
        sysfs: "/sys".into(),
        state_dir: ["/var/lib", NAME].iter().collect(),
    };
    let mut it = args.iter();
    while let Some(flag) = it.next() {
        let field = match flag.as_str() {
            "--sysfs" => &mut o.sysfs,
            "--state-dir" => &mut o.state_dir,
            _ => return Err(format!("unknown option {flag:?}\n\n{USAGE}")),
        };
        if euid == 0 {
            return Err(format!("{flag} is refused as root"));
        }
        *field = it.next().ok_or(format!("{flag} needs a value"))?.into();
    }
    Ok(o)
}

fn print(v: Value) -> Result<(), String> {
    println!("{v:#}");
    Ok(())
}
