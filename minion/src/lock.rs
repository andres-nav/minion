//! The queue lock: one queue at a time. Its holder writes its queue id into the
//! file, so a waiter can say whom it waits for, and reap can tell its workloads
//! from leftovers of a queue that died.

use std::fs::{self, File, OpenOptions, Permissions, TryLockError};
use std::io::{self, ErrorKind, Read};
use std::os::unix::fs::{FileExt, OpenOptionsExt, PermissionsExt};
use std::path::Path;
use std::time::Instant;

use crate::ffi::{O_NOFOLLOW, O_NONBLOCK, SIGKILL, kill};
use crate::utc::log;

/// Set in each workload's environment, which its children inherit.
pub const MARK: &str = "MINION_WORKLOAD";

/// Takes the queue lock, sleeping in the kernel while another queue holds it.
/// Returns the lock and, if it waited, for whom.
pub fn take(path: &Path) -> Result<(File, Option<String>), String> {
    let err = |e| err(path, e);
    let f = open(path).map_err(err)?;
    match f.try_lock() {
        Ok(()) => Ok((f, None)),
        Err(TryLockError::WouldBlock) => {
            let holder = holder(&f);
            log(format!(
                "waiting for queue {holder:?}, which holds {}",
                path.display()
            ));
            let t = Instant::now();
            f.lock().map_err(err)?;
            let secs = t.elapsed().as_secs_f64();
            Ok((f, Some(format!("waited {secs:.0} s for queue {holder:?}"))))
        }
        Err(TryLockError::Error(e)) => Err(err(e)),
    }
}

/// Writes the holder's queue id into the lock.
pub fn name(f: &File, id: &str) -> Result<(), String> {
    f.set_len(0)
        .and_then(|()| f.write_all_at(id.as_bytes(), 0))
        .map_err(|e| format!("write the queue lock: {e}"))
}

/// The queue that holds the lock, or None if it is free.
pub fn held_by(path: &Path) -> Result<Option<String>, String> {
    let err = |e| err(path, e);
    let f = open(path).map_err(err)?;
    match f.try_lock() {
        Ok(()) => Ok(None),
        Err(TryLockError::WouldBlock) => Ok(Some(holder(&f))),
        Err(TryLockError::Error(e)) => Err(err(e)),
    }
}

/// Kills, as the caller, its processes whose MARK names this lock and a queue
/// other than the one holding it: the workloads, and their children, of a queue
/// that died. Returns how many it killed.
pub fn reap(path: &Path) -> Result<usize, String> {
    let keep = held_by(path)?;
    let prefix = format!("{MARK}={}", mark(path, "")).into_bytes();
    let me = std::process::id().to_string();
    let mut killed = 0;
    for e in fs::read_dir("/proc")
        .map_err(|e| format!("/proc: {e}"))?
        .flatten()
    {
        let name = e.file_name().into_string().unwrap_or_default();
        if let Ok(pid) = name.parse::<i32>()
            && let Ok(env) = fs::read(e.path().join("environ")) // unreadable: not ours
            && let Some(queue) = env.split(|&b| b == 0).find_map(|v| v.strip_prefix(&prefix[..]))
            && keep.as_deref().map(str::as_bytes) != Some(queue)
            && name != me
            && kill(pid, SIGKILL) == 0
        {
            killed += 1;
        }
    }
    Ok(killed)
}

/// What MARK says for a workload of queue id under this lock.
pub fn mark(path: &Path, id: &str) -> String {
    format!("{} {id}", path.to_string_lossy())
}

/// Where /run/lock is root's 0755 (Fedora, RHEL, Amazon Linux), says how to fix it.
fn err(path: &Path, e: io::Error) -> String {
    let p = path.display();
    match e.kind() {
        ErrorKind::PermissionDenied => format!(
            "lock {p}: {e}; root must create it writable by all, e.g. at boot with the tmpfiles.d line `f {p} 0666 root root -`"
        ),
        _ => format!("lock {p}: {e}"),
    }
}

fn holder(f: &File) -> String {
    let mut s = String::new();
    let _ = f.take(256).read_to_string(&mut s);
    s
}

/// Opens the lock file, creating it writable by all. An existing file is opened
/// without O_CREAT, which protected_regular refuses for another user's file in
/// a sticky directory such as /run/lock; with O_NONBLOCK, so a FIFO put there
/// is refused instead of blocking; and with O_NOFOLLOW, so a symlink is refused.
fn open(path: &Path) -> io::Result<File> {
    loop {
        let open = OpenOptions::new()
            .read(true)
            .write(true)
            .custom_flags(O_NONBLOCK | O_NOFOLLOW)
            .open(path);
        match open {
            Ok(f) if f.metadata()?.is_file() => return Ok(f),
            Ok(_) => return Err(io::Error::other("not a regular file")),
            Err(e) if e.kind() == ErrorKind::NotFound => {}
            Err(e) => return Err(e),
        }
        let new = OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .open(path);
        match new {
            Ok(f) => return f.set_permissions(Permissions::from_mode(0o666)).map(|()| f),
            Err(e) if e.kind() == ErrorKind::AlreadyExists => {}
            Err(e) => return Err(e),
        }
    }
}
