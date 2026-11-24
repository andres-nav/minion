use std::path::Path;

use serde_json::{Value, json};

use crate::journal::Journal;
use crate::settings::{self, SETTINGS, State, Values};

/// Applies a profile. The first apply of a journal saves the full state first.
pub fn apply(j: &mut Journal, sys: &Path, profile: &State) -> Result<(), String> {
    settings::validate(sys, profile)?;
    let before = settings::snapshot(sys)?;
    if last(&j.lines, "saved").is_none() {
        j.append(json!({"t": "saved", "state": before}))?;
    }
    let ours = ours(&j.lines)?;
    j.append(json!({"t": "apply", "profile": profile}))?;
    for s in SETTINGS {
        if let Some(want) = profile.get(s.id()) {
            s.apply(sys, want)?;
        }
    }
    let mut applied = settings::snapshot(sys)?;
    // An object the profile leaves alone that changed since the last apply was
    // changed by someone else: keep expecting its earlier value, so restore keeps it.
    if let Some(ours) = &ours {
        for (id, values) in &mut applied {
            for (obj, v) in values.iter_mut() {
                let set = profile.get(id).is_some_and(|p| p.contains_key(obj));
                match ours.get(id).and_then(|o| o.get(obj)) {
                    Some(was) if !set && before[id].get(obj) != Some(was) => *v = was.clone(),
                    _ => {}
                }
            }
        }
    }
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

/// Puts back every saved value and closes the open journal. An object that
/// differs from both its saved and its last applied value was changed by someone
/// else: it is kept and returned as a conflict.
pub fn restore(j: &mut Journal, sys: &Path) -> Result<Vec<Value>, String> {
    let saved = state_of(&j.lines, "saved")?;
    let applied = ours(&j.lines)?;
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
        s.apply(sys, &back)?;
    }
    j.append(json!({"t": "restored", "conflicts": conflicts}))?;
    j.close()?;
    Ok(conflicts)
}

/// The state minion last left: saved if nothing was written, else the last
/// applied; None if an apply died before its read-back, so every object may be ours.
fn ours(lines: &[Value]) -> Result<Option<State>, String> {
    match (last(lines, "apply"), last(lines, "applied")) {
        (None, _) => state_of(lines, "saved").map(Some),
        (Some(a), Some(b)) if b > a => state_of(lines, "applied").map(Some),
        _ => Ok(None),
    }
}

fn last(lines: &[Value], t: &str) -> Option<usize> {
    lines.iter().rposition(|l| l["t"] == t)
}

fn state_of(lines: &[Value], t: &str) -> Result<State, String> {
    match last(lines, t) {
        Some(i) => serde_json::from_value(lines[i]["state"].clone())
            .map_err(|e| format!("journal {t}: {e}")),
        None => Ok(State::new()),
    }
}
