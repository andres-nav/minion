//! The queue lock: one queue at a time. Its holder writes its queue id into the
//! file, so a waiter can say whom it waits for, a privileged verb can tell a
//! live queue's child from a stray one, and reap can tell its workloads from
//! leftovers of a queue that died.

use std::fs::{self, File, OpenOptions, Permissions, TryLockError};
use std::io::{self, ErrorKind, Read};
use std::os::unix::fs::{FileExt, OpenOptionsExt, PermissionsExt};
use std::path::Path;
use std::time::Instant;

use crate::utc::log;

#[cfg(all(
    target_os = "linux",
    any(target_arch = "x86_64", target_arch = "aarch64")
))]
const O_NONBLOCK: i32 = 0o4000;
#[cfg(not(all(
    target_os = "linux",
    any(target_arch = "x86_64", target_arch = "aarch64")
)))]
compile_error!("O_NONBLOCK is only known here for Linux on x86_64 and aarch64");
const SIGKILL: i32 = 9;

/// Set in each workload's environment, which its children inherit.
pub const MARK: &str = "MINION_WORKLOAD";

unsafe extern "C" {
    safe fn kill(pid: i32, sig: i32) -> i32;
}

/// Takes the queue lock, sleeping in the kernel while another queue holds it.
/// Returns the lock and, if it waited, for whom.
pub fn take(path: &Path) -> Result<(File, Option<String>), String> {
    let err = |e: io::Error| format!("lock {}: {e}", path.display());
    let f = open(path, true).map_err(err)?;
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

/// The queue that holds the lock, or None if it is free. Root calls this, so it
/// never writes the file.
pub fn held_by(path: &Path) -> Result<Option<String>, String> {
    let err = |e: io::Error| format!("lock {}: {e}", path.display());
    let f = open(path, false).map_err(err)?;
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
    let me = std::process::id().to_string();
    let mut killed = 0;
    for e in fs::read_dir("/proc")
        .map_err(|e| format!("/proc: {e}"))?
        .flatten()
    {
        let name = e.file_name().into_string().unwrap_or_default();
        let Ok(pid) = name.parse::<i32>() else {
            continue;
        };
        let Ok(env) = fs::read(e.path().join("environ")) else {
            continue;
        }; // not ours
        let mark = env.split(|&b| b == 0).find_map(|v| {
            let v = String::from_utf8_lossy(v).into_owned();
            v.strip_prefix(&format!("{MARK}=")).map(str::to_string)
        });
        let Some((lock, queue)) = mark.as_deref().and_then(|m| m.split_once(' ')) else {
            continue;
        };
        let leftover = lock == path.to_string_lossy() && keep.as_deref() != Some(queue);
        if leftover && name != me && kill(pid, SIGKILL) == 0 {
            killed += 1;
        }
    }
    Ok(killed)
}

/// What MARK says for a workload of queue id under this lock.
pub fn mark(path: &Path, id: &str) -> String {
    format!("{} {id}", path.to_string_lossy())
}

/// At most the first 256 bytes, the holder's queue id.
fn holder(f: &File) -> String {
    let mut s = String::new();
    let _ = f.take(256).read_to_string(&mut s);
    s
}

/// Opens the lock file, creating it writable by all. An existing file is opened
/// without O_CREAT, which protected_regular refuses for another user's file in
/// a sticky directory such as /run/lock; and with O_NONBLOCK, so a FIFO put
/// there is refused instead of blocking.
fn open(path: &Path, write: bool) -> io::Result<File> {
    loop {
        let open = OpenOptions::new()
            .read(true)
            .write(write)
            .custom_flags(O_NONBLOCK)
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
