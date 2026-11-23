use std::fs::{self, File, OpenOptions};
use std::io::{ErrorKind, Read, Write};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use serde_json::{Value, json};

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
            .open(dir.join("lock"))
            .map_err(|e| err("open lock", dir, e))?;
        lock.lock().map_err(|e| err("lock", dir, e))?;
        let path = dir.join("journal.jsonl");
        let (file, lines) = match OpenOptions::new().read(true).append(true).open(&path) {
            Ok(mut f) => {
                let mut s = String::new();
                f.read_to_string(&mut s)
                    .map_err(|e| err("read", &path, e))?;
                let end = s.rfind('\n').map_or(0, |i| i + 1);
                f.set_len(end as u64)
                    .map_err(|e| err("truncate", &path, e))?;
                (Some(f), parse(&s[..end])?)
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
            let f = OpenOptions::new().append(true).create_new(true).open(&path);
            self.file = Some(f.map_err(|e| err("create", &path, e))?);
            sync_dir(&self.dir)?;
        }
        line["time"] = json!(now_ns() / 1_000_000_000);
        let mut bytes = line.to_string().into_bytes();
        bytes.push(b'\n');
        let f = self.file.as_mut().unwrap();
        f.write_all(&bytes)
            .and_then(|_| f.sync_all())
            .map_err(|e| err("write", &path, e))?;
        self.lines.push(line);
        Ok(())
    }

    /// Moves the journal to done/<ns>.jsonl.
    pub fn close(self) -> Result<(), String> {
        let done = self.dir.join("done");
        create_dir(&done)?;
        let to = done.join(format!("{}.jsonl", now_ns()));
        fs::rename(self.dir.join("journal.jsonl"), &to).map_err(|e| err("rename to", &to, e))?;
        sync_dir(&done)?;
        sync_dir(&self.dir)
    }

    pub fn exists(&self) -> bool {
        self.file.is_some()
    }
}

/// Reads the journal without the lock, for status. Missing means none is open.
pub fn read(dir: &Path) -> Result<Vec<Value>, String> {
    let path = dir.join("journal.jsonl");
    match fs::read_to_string(&path) {
        Ok(s) => parse(&s[..s.rfind('\n').map_or(0, |i| i + 1)]),
        Err(e) if e.kind() == ErrorKind::NotFound => Ok(vec![]),
        Err(e) => Err(err("read", &path, e)),
    }
}

fn parse(s: &str) -> Result<Vec<Value>, String> {
    s.lines()
        .map(|l| serde_json::from_str(l).map_err(|e| format!("journal line {l:?}: {e}")))
        .collect()
}

/// Creates dir if missing, fsyncing its parent so the entry survives a crash.
fn create_dir(dir: &Path) -> Result<(), String> {
    match fs::create_dir(dir) {
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

fn now_ns() -> u128 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_nanos()
}

fn err(what: &str, path: &Path, e: std::io::Error) -> String {
    format!("{what} {}: {e}", path.display())
}
