//! The root side: one `minion serve` per queue, started through sudo. It reads
//! only its pipe (never user files) and never kills. Each request line, `apply
//! PROFILE` or `restore`, gets one reply line, `ok TEXT` or `error TEXT`, after
//! a first one that says it runs. It takes the journal lock at the first
//! request and holds it to its end; when the pipe closes (the queue ended or
//! died) it restores and exits. Blocked on the pipe between requests, it never
//! wakes while runs are measured.

use std::io::{self, BufRead, BufReader, PipeReader, PipeWriter, Write};
use std::os::unix::process::CommandExt;
use std::process::Command;

use crate::journal::Journal;
use crate::utc::log;
use crate::{NAME, Opts, VERSION, engine};

/// The exit code when settings must change and `sudo -n ... serve` isn't allowed.
pub const NEEDS_ROOT: u8 = 77;

/// Answers the requests on input until it ends, then restores.
pub fn serve(o: &Opts, input: impl BufRead, mut out: impl Write) -> Result<(), String> {
    let mut journal = None;
    if send(&mut out, Ok(format!("{NAME} {VERSION}"))) {
        for line in input.lines() {
            let Ok(line) = line else { break };
            if !send(&mut out, answer(o, &mut journal, &line)) {
                break;
            }
        }
    }
    let said = journal.map(|mut j| restore(o, &mut j)).transpose()?;
    if let Some(said) = said.filter(|s| s.starts_with("restored")) {
        log(format!("the queue is gone: {said}"));
    }
    Ok(())
}

/// What a request did, as one line: "" for an apply.
fn answer(o: &Opts, journal: &mut Option<Journal>, line: &str) -> Result<String, String> {
    let (verb, arg) = line.split_once(' ').unwrap_or((line, ""));
    if !matches!((verb, arg.is_empty()), ("restore", true) | ("apply", false)) {
        return Err(format!("unknown request {line:?}"));
    }
    let j = match journal {
        Some(j) => j,
        None => journal.insert(Journal::open(&o.state_dir)?),
    };
    if verb == "restore" {
        return restore(o, j);
    }
    let profile = serde_json::from_str(arg).map_err(|e| format!("profile: {e}"))?;
    engine::apply(j, &o.sysfs, &profile).map(|()| String::new())
}

/// Restores the open journal. Returns what it did.
fn restore(o: &Opts, j: &mut Journal) -> Result<String, String> {
    if j.lines.is_empty() {
        return Ok("no journal open".into());
    }
    let kept = engine::restore(j, &o.sysfs)?.into_iter();
    Ok(kept.fold("restored".into(), |s, c| {
        format!("{s}; kept {c}: changed by someone else")
    }))
}

/// Writes one reply line. Returns false once the queue is gone.
fn send(out: &mut impl Write, reply: Result<String, String>) -> bool {
    let (word, text) = reply.map_or_else(|e| ("error", e), |s| ("ok", s));
    writeln!(out, "{word} {}", text.replace('\n', " ")).is_ok() // stdout flushes each line
}

/// The user's end of a serve's pipe. Dropping it makes serve restore and exit.
pub struct Root(PipeWriter, BufReader<PipeReader>);

impl Root {
    /// Starts `sudo -n /usr/local/lib/gru/<this file's name> serve`, which
    /// `gru setup` installed under the name gru gives each build, by its hash,
    /// and reads its first line, so a missing setup shows before anything is
    /// touched; without it, logs how to set up and returns None. sudo gets its
    /// own process group, so a stop signal to the queue doesn't reach it. The
    /// unit tests run serve on a thread.
    pub fn start(o: &Opts) -> Option<Root> {
        let ((r1, w1), (r2, w2)) = (io::pipe().ok()?, io::pipe().ok()?);
        let mut root = Root(w1, BufReader::new(r2));
        #[cfg(test)]
        if !o.test.is_empty() {
            let o = o.clone();
            std::thread::spawn(move || serve(&o, BufReader::new(r1), w2));
            return root.read().ok().map(|_| root);
        }
        let path = o.lib.join(std::env::current_exe().ok()?.file_name()?);
        let mut sudo = Command::new("sudo");
        sudo.arg("-n").arg(path).arg("serve").args(&o.test);
        let spawned = sudo.stdin(r1).stdout(w2).process_group(0).spawn();
        drop(sudo); // closes our r1 and w2, so a failing sudo reads as EOF
        if spawned.is_err() || root.read().is_err() {
            let h = std::env::var("GRU_HOST_NAME").unwrap_or("<host>".into()); // gru's --host
            log(format!(
                "minion needs root on {h}: run `gru setup --host {h}` once in a terminal"
            ));
            return None;
        }
        Some(root)
    }

    /// Sends a request and returns its answer.
    pub fn ask(&mut self, req: &str) -> Result<String, String> {
        writeln!(self.0, "{req}").map_err(|e| format!("serve: {e}"))?;
        self.read()
    }

    fn read(&mut self) -> Result<String, String> {
        let mut line = String::new();
        let _ = self.1.read_line(&mut line); // an error reads as serve ended
        match line.trim_end_matches('\n').split_once(' ') {
            Some(("ok", s)) => Ok(s.into()),
            Some(("error", e)) => Err(e.into()),
            _ => Err("serve ended".into()),
        }
    }
}
