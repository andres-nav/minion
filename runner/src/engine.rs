use std::path::Path;

use serde_json::{Value, json};

use crate::journal::{self, Journal};
use crate::settings::{self, SETTINGS, State, Values};

/// Applies a profile. The first apply of a journal saves the full state first.
pub fn apply(sys: &Path, dir: &Path, profile: &State) -> Result<(), String> {
    let mut j = Journal::open(dir)?;
    settings::validate(sys, profile)?;
    if last(&j.lines, "saved").is_none() {
        if j.lines.is_empty() {
            j.append(json!({"t": "open"}))?;
        }
        j.append(json!({"t": "saved", "state": settings::snapshot(sys)?}))?;
    }
    j.append(json!({"t": "apply", "profile": profile}))?;
    for s in SETTINGS {
        if let Some(want) = profile.get(s.id()) {
            s.apply(sys, want)?;
        }
    }
    let applied = settings::snapshot(sys)?;
    j.append(json!({"t": "applied", "state": applied}))?;
    for (id, want) in profile {
        for (obj, value) in want {
            let got = applied.get(id).and_then(|v| v.get(obj));
            if got != Some(value) {
                return Err(format!("{id} {obj}: wrote {value:?}, kernel shows {got:?}"));
            }
        }
    }
    Ok(())
}

/// Puts back every saved value and closes the journal. An object that differs
/// from both its saved and its last applied value was changed by someone else:
/// it is kept and returned as a conflict.
pub fn restore(sys: &Path, dir: &Path) -> Result<Vec<Value>, String> {
    let mut j = Journal::open(dir)?;
    if !j.exists() {
        return Ok(vec![]);
    }
    let saved = state_of(&j.lines, "saved")?;
    let applied = match (last(&j.lines, "apply"), last(&j.lines, "applied")) {
        (None, _) => Some(saved.clone()), // nothing was written
        (Some(a), Some(b)) if b > a => Some(state_of(&j.lines, "applied")?),
        _ => None, // died between apply and applied: restore every object
    };
    let now = settings::snapshot(sys)?;
    let mut conflicts = vec![];
    for s in SETTINGS {
        let id = s.id();
        let mut back = Values::new();
        for (obj, old) in saved.get(id).into_iter().flatten() {
            let cur = now.get(id).and_then(|v| v.get(obj));
            let ours = applied
                .as_ref()
                .is_none_or(|a| a.get(id).and_then(|v| v.get(obj)) == cur);
            if cur == Some(old) {
                continue;
            }
            if cur.is_some() && ours {
                back.insert(obj.clone(), old.clone());
            } else {
                conflicts.push(json!({"setting": id, "object": obj, "saved": old, "now": cur}));
            }
        }
        s.restore(sys, &back)?;
    }
    j.append(json!({"t": "restored", "conflicts": conflicts}))?;
    j.append(json!({"t": "close"}))?;
    j.close()?;
    Ok(conflicts)
}

/// The open journal's last line and the objects that now differ from their saved value.
pub fn status(sys: &Path, dir: &Path) -> Result<Value, String> {
    let lines = journal::read(dir)?;
    let Some(end) = lines.last() else {
        return Ok(json!({"open": false}));
    };
    let saved = state_of(&lines, "saved")?;
    let changed: State = settings::snapshot(sys)?
        .into_iter()
        .map(|(id, now)| {
            let old = saved.get(&id);
            let diff = now
                .into_iter()
                .filter(|(o, v)| old.and_then(|s| s.get(o)).is_some_and(|s| s != v));
            (id, diff.collect())
        })
        .collect();
    Ok(json!({"open": true, "last": end["t"], "changed": changed}))
}

fn last(lines: &[Value], t: &str) -> Option<usize> {
    lines.iter().rposition(|l| l["t"] == t)
}

/// The state of the last line of type t, or an empty state if there is none.
fn state_of(lines: &[Value], t: &str) -> Result<State, String> {
    match last(lines, t) {
        Some(i) => serde_json::from_value(lines[i]["state"].clone())
            .map_err(|e| format!("journal {t}: {e}")),
        None => Ok(State::new()),
    }
}
