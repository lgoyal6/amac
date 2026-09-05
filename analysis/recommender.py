"""The notification recommender: features, a label, a baseline, and a gate.

What this is for. amac delivers on the order of a hundred notifications a day
and a person acts on a handful. The question is which ones were worth sending,
and the honest answer so far has been arrived at by counting rather than by
learning: 81% of everything delivered was a turn-complete, most of those
announced a turn that took under a minute, and suppressing those took the
stream down by nine tenths without touching anything anybody acts on.

So this is deliberately built to be beaten by rules. The baseline it must
improve on is not "send everything", it is the rule set that actually ships. A
model that cannot beat a threshold on turn length has no business ranking
anything, and the gate below refuses to export one that does not.

Three things this file will not do.

It will not train on the old engagement label. "The session changed state soon
after" correlated 0.64 with how chatty an adapter is, so a model fitted to it
learns which CLI reports telemetry. See notifications.py, which proves it.

It will not train on a description of behaviour. Being told which notifications
matter is useful for choosing a window and deciding which acts count as
answers, and it is not a label. A model fitted to a description learns the
description.

It will not export a model it cannot show beats the baseline on data the model
never saw, split by time rather than at random. Notifications arrive in bursts
and a random split puts the same burst on both sides of it.
"""

from __future__ import annotations

import argparse
import json
import math
from dataclasses import dataclass, asdict
from pathlib import Path

import numpy as np
import pandas as pd
from sklearn.linear_model import LogisticRegression

import amaclog
from notifications import answered, responded, WINDOW

# The features, named once. Order is the contract with the Go side: the
# exported weights are a list, and a reordering here without a re-export would
# silently score every notification against the wrong coefficient.
FEATURES = [
    "wants_attention",   # blocked on a person, rather than an agent finishing
    "log_turn_s",        # how long the turn ran, which is what the shipped rule uses
    "log_since_s",       # since the last notification for this session
    "prior_hour",        # notifications for this session in the last hour
    "global_hour",       # notifications across all sessions in the last hour
    "hour_sin",          # time of day, as a circle rather than a number line
    "hour_cos",
    "is_weekend",
    "log_msg_len",       # how much the agent had to say
]

# Below this many positives in the holdout, nothing is exported. A model
# validated on a handful of positives is a model validated on noise, and the
# whole point of the gate is that it is checked before anyone relies on it.
MIN_HOLDOUT_POSITIVES = 30


@dataclass
class Verdict:
    """What the gate decided, and every number behind it."""
    trained: bool
    exported: bool
    why: str
    n_total: int = 0
    n_positive: int = 0
    holdout_positives: int = 0
    baseline_sent: int = 0
    baseline_kept: int = 0
    model_sent: int = 0
    model_kept: int = 0
    threshold: float = 0.0
    weights: dict | None = None


def featurise(notifs: pd.DataFrame, events: pd.DataFrame) -> pd.DataFrame:
    """Everything a decision could have known at the moment it was made.

    Session identity is excluded on purpose. It is the strongest predictor in
    the data and the least useful: sessions are ephemeral, so a model that
    learns one session's name has nothing to say about the next, and its score
    would be a memorisation score.
    """
    df = notifs.sort_values("at").reset_index(drop=True).copy()
    if df.empty:
        for c in FEATURES:
            df[c] = pd.Series(dtype="float64")
        return df

    df["wants_attention"] = (df["reason"] == "wants-attention").astype(float)

    hour = df["at"].dt.hour + df["at"].dt.minute / 60
    df["hour_sin"] = np.sin(2 * math.pi * hour / 24)
    df["hour_cos"] = np.cos(2 * math.pi * hour / 24)
    df["is_weekend"] = (df["at"].dt.dayofweek >= 5).astype(float)

    msg = df["message"] if "message" in df.columns else pd.Series([""] * len(df))
    df["log_msg_len"] = np.log1p(msg.fillna("").astype(str).str.len())

    # Per-session and global rate, as a decision would have seen them: only
    # what had already arrived. Anything computed over the whole frame would be
    # leakage dressed up as a feature.
    prior, glob, since = [], [], []
    seen: dict[str, list] = {}
    all_seen: list = []
    last: dict[str, pd.Timestamp] = {}
    hour_delta = pd.Timedelta(hours=1)
    for at, sess in zip(df["at"], df["session"]):
        s = seen.setdefault(sess, [])
        while s and at - s[0] > hour_delta:
            s.pop(0)
        while all_seen and at - all_seen[0] > hour_delta:
            all_seen.pop(0)
        prior.append(len(s))
        glob.append(len(all_seen))
        since.append((at - last[sess]).total_seconds() if sess in last else 86400.0)
        s.append(at)
        all_seen.append(at)
        last[sess] = at
    df["prior_hour"] = np.array(prior, dtype="float64")
    df["global_hour"] = np.array(glob, dtype="float64")
    df["log_since_s"] = np.log1p(np.clip(since, 0, None))

    df["log_turn_s"] = np.log1p(np.clip(turn_lengths(df, events), 0, None))
    return df


def turn_lengths(notifs: pd.DataFrame, events: pd.DataFrame) -> np.ndarray:
    """Seconds since the session's previous event, per notification.

    The same statistic the shipped rule is built on, computed the same way, so
    the model and the rule it has to beat are looking at the same quantity.
    A notification with no prior event gets a day, which is how the rule treats
    it: unmeasurable means do not suppress.
    """
    if events.empty:
        return np.full(len(notifs), 86400.0)
    ev = events.sort_values("at")
    out = []
    by_session = {s: g["at"].to_numpy(dtype="datetime64[ns]") for s, g in ev.groupby("session")}
    for at, sess in zip(notifs["at"], notifs["session"]):
        times = by_session.get(sess)
        if times is None or len(times) == 0:
            out.append(86400.0)
            continue
        t = np.datetime64(at.tz_localize(None) if at.tzinfo else at)
        i = np.searchsorted(times, t, side="left")
        if i == 0:
            out.append(86400.0)
        else:
            out.append(float((t - times[i - 1]) / np.timedelta64(1, "s")))
    return np.array(out, dtype="float64")


def shipped_rules(df: pd.DataFrame, min_turn_s: float = 600.0) -> np.ndarray:
    """The baseline: what amac sends today, replayed over the same rows.

    Blocked on a person always goes. A turn shorter than the threshold does
    not. This is the thing to beat, and it is deliberately not a strawman: it
    already removes about nine tenths of the volume.
    """
    if df.empty:
        return np.array([], dtype=bool)
    wants = df["wants_attention"].to_numpy() > 0
    long_enough = np.expm1(df["log_turn_s"].to_numpy()) >= min_turn_s
    return wants | long_enough


def label(notifs: pd.DataFrame, acts: pd.DataFrame, opens: pd.DataFrame) -> pd.Series:
    """A yes or no per notification, produced by a person and nothing else.

    The exact label first: an act carrying the notification's own id, which has
    no window in it and cannot be a coincidence. Widened by proximity only
    where there is no exact answer, because the wide label is evidence and the
    narrow one is proof.
    """
    exact = answered(notifs, acts)["answered"]
    pooled = [d[["at", "session"]] for d in (opens, acts) if d is not None and not d.empty]
    if not pooled:
        return exact
    near = responded(notifs, pd.concat(pooled, ignore_index=True), WINDOW)["responded"]
    return (exact | near).astype(bool)


def split_by_time(df: pd.DataFrame, y: pd.Series, holdout: float = 0.3):
    """Train on the past, test on the future, as production experiences it.

    Notifications arrive in bursts, so a random split puts the same burst on
    both sides and the score measures memorising a burst.
    """
    cut = int(len(df) * (1 - holdout))
    return df.iloc[:cut], df.iloc[cut:], y.iloc[:cut], y.iloc[cut:]


def highest_affordable_threshold(scores: np.ndarray, budget: int) -> float:
    """The lowest cutoff that still sends no more than budget notifications.

    Ties are the whole difficulty. A model confident about a group of
    notifications gives them the same score, and a cutoff that admits one of
    them admits all of them, so the affordable cutoff has to be chosen over
    distinct scores rather than by counting rows.
    """
    if len(scores) == 0:
        return 1.0
    cutoff = float(np.max(scores)) + 1.0  # sends nothing, always affordable
    for t in np.unique(scores)[::-1]:
        if int((scores >= t).sum()) > budget:
            break
        cutoff = float(t)
    return cutoff


def compare(sent: np.ndarray, y: np.ndarray) -> tuple[int, int]:
    """Volume and retention: how many went out, how many wanted ones survived."""
    return int(sent.sum()), int((sent & y).sum())


def build(db: str | None = None, holdout: float = 0.3,
          min_turn_s: float = 600.0) -> Verdict:
    """Load, label, and hand the rest to evaluate."""
    notifs = amaclog.notifications(db)
    if notifs.empty:
        return Verdict(False, False, "no notifications in the log")
    notifs = notifs[notifs["sent"]] if "sent" in notifs.columns else notifs
    acts = amaclog.acts(db)
    opens = amaclog.opens(db)
    events = amaclog.events("session.state", db)

    df = featurise(notifs, events)
    y = label(df, acts, opens).to_numpy()
    return evaluate(df, y, holdout, min_turn_s)


def evaluate(df: pd.DataFrame, y: np.ndarray, holdout: float = 0.3,
             min_turn_s: float = 600.0) -> Verdict:
    """Train, compare against the shipped rules, and decide whether to export.

    Separate from build so the decision can be exercised on data with a known
    answer. A gate that has only ever been run on a log where it refuses is a
    gate nobody has watched pass.
    """
    n_pos = int(y.sum())
    tr, te, ytr, yte = split_by_time(df, pd.Series(y), holdout)
    holdout_pos = int(yte.sum())

    v = Verdict(
        trained=False, exported=False, why="",
        n_total=len(df), n_positive=n_pos, holdout_positives=holdout_pos,
    )

    # The gate, before anything is fitted. Reporting a score computed on four
    # positives would invite exactly the reliance this exists to prevent.
    if holdout_pos < MIN_HOLDOUT_POSITIVES:
        v.why = (f"{holdout_pos} positives in the holdout, need {MIN_HOLDOUT_POSITIVES}. "
                 f"{n_pos} of {len(df)} notifications have a label at all. "
                 "Nothing trained: a model validated on this many positives is "
                 "validated on noise.")
        return v
    if len(set(ytr)) < 2:
        v.why = "the training window contains only one class; nothing to learn"
        return v

    model = LogisticRegression(max_iter=2000, class_weight="balanced")
    model.fit(tr[FEATURES], ytr)
    v.trained = True

    base_sent = shipped_rules(te, min_turn_s)
    v.baseline_sent, v.baseline_kept = compare(base_sent, yte.to_numpy())

    # Compared at equal volume, which is the only comparison that means
    # anything. A model that keeps more by sending more has not improved on the
    # rules, it has just turned the dial the other way.
    #
    # The comparison is of a threshold, not of a top-k, because a threshold is
    # what actually ships: the daemon scores one notification at a time and has
    # no idea what the rest of the day will look like. Evaluating top-k and
    # exporting a threshold would measure a policy nobody runs.
    #
    # Which is also the bug this replaced. Taking the budget-th score and
    # sending everything at or above it looks like top-k and is not: a
    # confident model puts many notifications on the same score, and the ties
    # went out too. Measured on one synthetic set it sent 116 against a budget
    # of 61 and reported them as the same volume.
    scores = model.predict_proba(te[FEATURES])[:, 1]
    budget = int(base_sent.sum())
    if budget == 0:
        v.why = "the baseline sent nothing on the holdout; nothing to compare against"
        return v
    cutoff = highest_affordable_threshold(scores, budget)
    model_sent = scores >= cutoff
    v.model_sent, v.model_kept = compare(model_sent, yte.to_numpy())
    v.threshold = cutoff
    v.weights = {
        "features": FEATURES,
        "coef": [float(c) for c in model.coef_[0]],
        "intercept": float(model.intercept_[0]),
        "threshold": cutoff,
    }

    if v.model_kept <= v.baseline_kept:
        v.why = (f"at the same volume ({v.model_sent} sent) the model kept "
                 f"{v.model_kept} of the wanted notifications and the shipped rules kept "
                 f"{v.baseline_kept}. Not exported: it does not beat the rules.")
        return v

    v.exported = True
    v.why = (f"at the same volume ({v.model_sent} sent) the model kept "
             f"{v.model_kept} against the rules' {v.baseline_kept}.")
    return v


def export(v: Verdict, path: Path) -> bool:
    """Write the model only when the gate passed. Never a partial artifact."""
    if not v.exported or not v.weights:
        return False
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(v.weights, indent=2) + "\n")
    tmp.replace(path)  # atomic, so the daemon never reads half a model
    return True


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", default=None)
    ap.add_argument("--holdout", type=float, default=0.3)
    ap.add_argument("--min-turn", type=float, default=600.0)
    ap.add_argument("--out", default=str(Path.home() / ".amac" / "recommender.json"))
    ap.add_argument("--write", action="store_true",
                    help="export the model if it beats the shipped rules")
    a = ap.parse_args(argv)

    v = build(a.db, a.holdout, a.min_turn)
    print("notification recommender")
    print(f"  labelled       {v.n_positive} of {v.n_total} notifications")
    print(f"  holdout        {v.holdout_positives} positives (need {MIN_HOLDOUT_POSITIVES})")
    if v.trained:
        print(f"  shipped rules  sent {v.baseline_sent}, kept {v.baseline_kept}")
        print(f"  model          sent {v.model_sent}, kept {v.model_kept}")
    print(f"  verdict        {v.why}")
    if a.write and export(v, Path(a.out)):
        print(f"  wrote          {a.out}")
    elif a.write:
        print("  wrote          nothing, the gate did not pass")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
