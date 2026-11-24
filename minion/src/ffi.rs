//! The C functions and constants std lacks, as Linux has them on x86_64 and aarch64.

#[cfg(not(all(
    target_os = "linux",
    any(target_arch = "x86_64", target_arch = "aarch64")
)))]
compile_error!("the C constants are only known here for Linux on x86_64 and aarch64");

pub const SIGHUP: i32 = 1;
pub const SIGINT: i32 = 2;
pub const SIGKILL: i32 = 9;
pub const SIGTERM: i32 = 15;
pub const EBUSY: i32 = 16;
pub const PR_SET_PDEATHSIG: i32 = 1;
pub const CLOCK_REALTIME: i32 = 0;
pub const CLOCK_MONOTONIC: i32 = 1;
pub const O_NONBLOCK: i32 = 0o4000;
#[cfg(target_arch = "x86_64")]
pub const O_NOFOLLOW: i32 = 0o400000;
#[cfg(target_arch = "aarch64")]
pub const O_NOFOLLOW: i32 = 0o100000;

#[repr(C)]
#[derive(Clone, Copy)]
pub struct Timespec {
    pub sec: i64,
    pub nsec: i64,
}

unsafe extern "C" {
    pub safe fn geteuid() -> u32;
    pub safe fn getppid() -> i32;
    pub safe fn kill(pid: i32, sig: i32) -> i32;
    pub safe fn prctl(option: i32, arg: u64) -> i32;
    pub fn signal(sig: i32, handler: usize) -> usize;
    pub fn clock_gettime(clock: i32, ts: *mut Timespec) -> i32;
    pub fn nanosleep(req: *const Timespec, rem: *mut Timespec) -> i32;
}
