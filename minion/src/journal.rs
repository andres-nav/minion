use std::fs::{self, DirBuilder, File, OpenOptions};
use std::io::{ErrorKind, Read, Write};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::path::{Path, PathBuf};

use serde_json::{Value, json};

use crate::utc;

/// The open journal of the state dir, held under an exclusive lock. The lock is
/// a separate file because the journal itself is renamed on close.
pub struct Journal {
    dir: PathBuf,
    file: Option<File>,
    pub lines: Vec<Value>,
    _lock: File,
}

impl Journal {
    /// Locks the state dir and loads its journal, dropping an incomplete last line.
    pub fn open(dir: &Path) -> Result<Journal, String> {
        create_dir(dir)?;
        let lock = OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .open(dir.join("lock"))
            .map_err(|e| err("open lock", dir, e))?;
        lock.lock().map_err(|e| err("lock", dir, e))?;
        let path = dir.join("journal.jsonl");
        let (file, lines) = match OpenOptions::new().read(true).append(true).open(&path) {
            Ok(mut f) => {
                let mut s = String::new();
                f.read_to_string(&mut s)
                    .map_err(|e| err("read", &path, e))?;
                let s = whole(&s);
                f.set_len(s.len() as u64)
                    .map_err(|e| err("truncate", &path, e))?;
                (Some(f), parse(s)?)
            }
            Err(e) if e.kind() == ErrorKind::NotFound => (None, vec![]),
            Err(e) => return Err(err("open", &path, e)),
        };
        Ok(Journal {
            dir: dir.into(),
            file,
            lines,
            _lock: lock,
        })
    }

    /// Appends one line (with the time) and fsyncs it before returning.
    pub fn append(&mut self, mut line: Value) -> Result<(), String> {
        let path = self.dir.join("journal.jsonl");
        if self.file.is_none() {
            let f = OpenOptions::new()
                .append(true)
                .create_new(true)
                .mode(0o600)
                .open(&path);
            self.file = Some(f.map_err(|e| err("create", &path, e))?);
            sync_dir(&self.dir)?;
        }
        let now = utc::now_s();
        line["time"] = json!(now);
        line["time_utc"] = json!(utc::iso(now));
        let mut bytes = line.to_string().into_bytes();
        bytes.push(b'\n');
        let f = self.file.as_mut().unwrap();
        f.write_all(&bytes)
            .and_then(|_| f.sync_all())
            .map_err(|e| err("write", &path, e))?;
        self.lines.push(line);
        Ok(())
    }

    /// Moves the journal to done/<UTC time to the ns>.jsonl.
    pub fn close(self) -> Result<(), String> {
        let done = self.dir.join("done");
        create_dir(&done)?;
        let to = done.join(format!("{}.jsonl", utc::stamp(utc::now_ns(), true)));
        fs::rename(self.dir.join("journal.jsonl"), &to).map_err(|e| err("rename to", &to, e))?;
        sync_dir(&done)?;
        sync_dir(&self.dir)
    }
}

/// Reads the journal without the lock, for status. Missing means none is open.
pub fn read(dir: &Path) -> Result<Vec<Value>, String> {
    let path = dir.join("journal.jsonl");
    match fs::read_to_string(&path) {
        Ok(s) => parse(whole(&s)),
        Err(e) if e.kind() == ErrorKind::NotFound => Ok(vec![]),
        Err(e) => Err(err("read", &path, e)),
    }
}

/// s without a torn last line.
fn whole(s: &str) -> &str {
    &s[..s.rfind('\n').map_or(0, |i| i + 1)]
}

fn parse(s: &str) -> Result<Vec<Value>, String> {
    s.lines()
        .map(|l| serde_json::from_str(l).map_err(|e| format!("journal line {l:?}: {e}")))
        .collect()
}

/// Creates dir if missing, private to its owner, fsyncing its parent so the
/// entry survives a crash.
fn create_dir(dir: &Path) -> Result<(), String> {
    match DirBuilder::new().mode(0o700).create(dir) {
        Ok(()) => sync_dir(&dir.join("..")),
        Err(e) if e.kind() == ErrorKind::AlreadyExists => Ok(()),
        Err(e) => Err(err("create", dir, e)),
    }
}

fn sync_dir(dir: &Path) -> Result<(), String> {
    File::open(dir)
        .and_then(|d| d.sync_all())
        .map_err(|e| err("fsync", dir, e))
}

fn err(what: &str, path: &Path, e: std::io::Error) -> String {
    format!("{what} {}: {e}", path.display())
}
