//! Every time is UTC; local time and TZ are never used.

use std::fmt::Display;
use std::time::{SystemTime, UNIX_EPOCH};

pub fn now_ns() -> u128 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_nanos()
}

pub fn now_s() -> u64 {
    (now_ns() / 1_000_000_000) as u64
}

/// 2026-11-23T14:25:01Z, next to an epoch field and in the log.
pub fn iso(secs: u64) -> String {
    let (y, m, d, hh, mm, ss) = civil(secs);
    format!("{y:04}-{m:02}-{d:02}T{hh:02}:{mm:02}:{ss:02}Z")
}

/// 20261123T142501Z for names, or 20261123T142501.123456789Z with the nanoseconds.
pub fn stamp(ns: u128, with_ns: bool) -> String {
    let (y, m, d, hh, mm, ss) = civil((ns / 1_000_000_000) as u64);
    let frac = if with_ns {
        format!(".{:09}", ns % 1_000_000_000)
    } else {
        String::new()
    };
    format!("{y:04}{m:02}{d:02}T{hh:02}{mm:02}{ss:02}{frac}Z")
}

pub fn log(msg: impl Display) {
    eprintln!("{} {}: {msg}", iso(now_s()), crate::NAME);
}

/// Year, month, day, hour, minute and second of secs since the epoch
/// (Hinnant's civil_from_days).
fn civil(secs: u64) -> (u64, u64, u64, u64, u64, u64) {
    let (days, s) = (secs / 86_400, secs % 86_400);
    let z = days + 719_468;
    let (era, doe) = (z / 146_097, z % 146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = yoe + era * 400 + u64::from(m <= 2);
    (y, m, d, s / 3600, s / 60 % 60, s % 60)
}
