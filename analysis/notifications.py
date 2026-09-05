"""Is amac's notification rule any good, and can a model do better?

amac sends a Discord message when an agent finishes a turn or wants attention,
and suppresses on two hand-tuned rules: a dedup window, and whether you are
currently looking at that session. Nothing has ever checked whether the
notifications that survive those rules were worth sending.

This asks that, and the answer is mostly a warning about how easy it is to
measure the wrong thing.

Run it:  python -m notifications          (or --db path/to/events.db)
"""

from __future__ import annotations

import argparse
import sys

import numpy as np
import pandas as pd
from sklearn.dummy import DummyClassifier
from sklearn.ensemble import GradientBoostingClassifier
from sklearn.metrics import roc_auc_score

import amaclog

# A notification is "engaged" if the session it names changed state soon after.
# Ten minutes is a guess, and the conclusions below do not turn on it: the
# artifact this file exists to document is present at every window tried.
WINDOW = pd.Timedelta(minutes=10)


def label(notifs: pd.DataFrame, states: pd.DataFrame) -> pd.DataFrame:
    """Attach the outcome we can observe: did that session move soon after."""
    # Compared as integers, with the unit pinned rather than inherited.
    # Mixing tz-aware pandas timestamps with numpy datetime64 drops the zone and
    # then refuses to compare, so the arithmetic happens in one zone-free
    # representation. Going through datetime64[ns] explicitly is the load-
    # bearing part: astype("int64") on a tz-aware column yields MICROSECONDS in
    # pandas 3, while Timedelta.value is nanoseconds, and mixing the two made
    # the window a thousand times too wide so that everything looked engaged.
    # A test caught it; the units are now stated instead of assumed.
    def as_ns(col) -> np.ndarray:
        return col.to_numpy(dtype="datetime64[ns]").astype("int64")

    by_session = {s: np.sort(as_ns(g["at"])) for s, g in states.groupby("session")}
    window_ns = int(WINDOW.value)
    engaged = []
    for at_ns, sess in zip(as_ns(notifs["at"]), notifs["session"]):
        times = by_session.get(sess)
        if times is None or len(times) == 0:
            engaged.append(False)
            continue
        i = np.searchsorted(times, at_ns, side="right")
        engaged.append(bool(i < len(times) and times[i] < at_ns + window_ns))
    out = notifs.copy()
    out["engaged"] = engaged
    return out


def responded(notifs: pd.DataFrame, opens: pd.DataFrame, window: pd.Timedelta = WINDOW) -> pd.DataFrame:
    """The label amac can now actually observe: did a person open the board.

    Unlike "the session changed state", this cannot be produced by an agent. It
    takes a human with a token, so it is not a property of which CLI is running,
    which is what sank the previous attempt at a correlation of 0.64 with
    session chattiness.

    Two strengths, and the stronger one is narrower. A deep-linked open names
    the session it came from, so it answers that notification specifically. A
    bare open only says somebody looked at the board soon after, which is
    weaker evidence and still evidence.
    """
    out = notifs.copy()
    if opens.empty:
        out["responded"] = False
        out["responded_directly"] = False
        return out

    all_ns = np.sort(opens["at"].to_numpy(dtype="datetime64[ns]").astype("int64"))
    by_session = {
        s: np.sort(g["at"].to_numpy(dtype="datetime64[ns]").astype("int64"))
        for s, g in opens[opens["session"].astype(bool)].groupby("session")
    }
    w = int(window.value)

    def hit(times, at_ns):
        if times is None or len(times) == 0:
            return False
        i = np.searchsorted(times, at_ns, side="right")
        return bool(i < len(times) and times[i] < at_ns + w)

    at = notifs["at"].to_numpy(dtype="datetime64[ns]").astype("int64")
    out["responded"] = [hit(all_ns, t) for t in at]
    out["responded_directly"] = [
        hit(by_session.get(sess), t) for t, sess in zip(at, notifs["session"])
    ]
    return out


def instrumentation_check(notifs: pd.DataFrame, states: pd.DataFrame) -> pd.DataFrame:
    """The check that decides whether any of this means anything.

    The label is "the session changed state", which is only evidence when the
    session reports state at all. Claude's adapter emits state through ACP;
    Codex's barely does. So a Codex session can never be labelled engaged, no
    matter what actually happened, and any model handed this data will discover
    that Codex sessions are worthless and be right for entirely the wrong
    reason.

    This is the same failure as amac's own routing benchmark, where the checker
    carried the answer it was grading. Both were found by asking what the
    number would look like if the instrument, rather than the world, produced
    it.
    """
    per_session = notifs.groupby("session").size().rename("notifications")
    per_state = states.groupby("session").size().rename("state_events")
    joined = pd.concat([per_session, per_state], axis=1).fillna(0).astype(int)
    joined = joined[joined["notifications"] > 0]
    joined["ratio"] = joined["state_events"] / joined["notifications"]
    # Fewer state events than notifications means absence of a transition is
    # missing instrumentation rather than evidence of anything.
    joined["labelable"] = joined["ratio"] >= 1.0
    return joined.sort_values("notifications", ascending=False)


def density_check(labelled: pd.DataFrame, check: pd.DataFrame) -> tuple[float, pd.DataFrame]:
    """The second check, and the one that settles it.

    Dropping thinly instrumented sessions removes one bias by selecting for the
    other end of the same axis: a session emitting many state events will have
    one inside any window, so it labels engaged whatever happened. If engagement
    tracks how chatty a session is, the label is measuring telemetry volume and
    no amount of filtering rescues it.

    Returns the correlation and the rate per density bucket, so the claim is a
    number rather than an impression.
    """
    joined = labelled.join(check["ratio"], on="session")
    buckets = pd.cut(joined["ratio"], [-0.01, 0.001, 1, 3, 10, 1e9],
                     labels=["0 (silent)", "<1", "1-3", "3-10", "10+"])
    by = joined.groupby(buckets, observed=True)["engaged"].agg(["size", "mean"])
    corr = joined["ratio"].corr(joined["engaged"].astype(int))
    return float(corr), by


def features(df: pd.DataFrame) -> pd.DataFrame:
    """Only features that could apply to a session nobody has seen before.

    Session identity is by far the strongest predictor here and is deliberately
    excluded. Sessions are ephemeral, so a model that learns
    "claude-lakshgoyal is worth notifying" has nothing to say about the next
    one, and its cross-validation score would be a memorisation score.
    """
    df = df.sort_values("at").copy()
    prior, last_seen = [], {}
    for at, sess in zip(df["at"], df["session"]):
        seen = last_seen.setdefault(sess, [])
        while seen and at - seen[0] > pd.Timedelta(hours=1):
            seen.pop(0)
        prior.append(len(seen))
        seen.append(at)
    df["prior_hour"] = prior
    df["hour"] = df["at"].dt.hour
    df["wants_attention"] = (df["reason"] == "wants-attention").astype(int)
    return df


def timewise_split(df: pd.DataFrame, holdout: float = 0.3):
    """Split by time, not at random.

    Notifications arrive in bursts, so a random split puts two halves of the
    same burst on both sides and leaks. The honest question is whether last
    month predicts this week.
    """
    df = df.sort_values("at")
    cut = int(len(df) * (1 - holdout))
    return df.iloc[:cut], df.iloc[cut:]


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", default=None, help="event log to read (default: ~/.amac/events.db)")
    args = ap.parse_args(argv)

    notifs = amaclog.notifications(args.db)
    states = amaclog.sessions(args.db)
    opens = amaclog.opens(args.db)
    sent = notifs[notifs["sent"]].copy()

    print(f"attention decisions   : {len(notifs)}")
    print(f"  sent                : {len(sent)}")
    print(f"  suppressed          : {len(notifs) - len(sent)}")
    if sent.empty:
        print("\nnothing was ever sent; nothing to analyse")
        return 0

    # The observable label first, because it is the one worth having. It only
    # exists from the day board opens started being recorded, so this reports
    # how much of the history it covers rather than quietly scoring a week.
    print("\n--- the label amac can observe ---")
    if opens.empty:
        print("no board opens recorded yet.")
        print("This is the label the recommender needs and it starts empty by")
        print("construction: the daemon began recording opens on the day that")
        print("shipped. Come back after a few weeks of ordinary use.")
    else:
        first = opens["at"].min()
        covered = sent[sent["at"] >= first]
        if covered.empty:
            print(f"opens recorded from {first:%b %d}, which is after every notification here.")
        else:
            r = responded(covered, opens)
            print(f"opens recorded  : {len(opens)}, from {first:%b %d}")
            print(f"notifications since : {len(covered)}")
            print(f"  followed by any open within {WINDOW}: {r['responded'].mean():.1%}")
            print(f"  followed by an open naming that session: {r['responded_directly'].mean():.1%}")
            if len(covered) < 200:
                print(f"\n{len(covered)} notifications is not enough to model on.")
                print("Reported anyway, because a small honest number beats a")
                print("large invalid one, which is what the rest of this file is about.")

    labelled = label(sent, states)
    print(f"\n--- the label that does not work, kept as a warning ---")
    print(f"naive engagement rate : {labelled['engaged'].mean():.1%}")

    check = instrumentation_check(sent, states)
    bad = check[~check["labelable"]]
    print("\n--- instrumentation check ---")
    print(f"sessions notified               : {len(check)}")
    print(f"too thinly instrumented to label: {len(bad)}")
    if not bad.empty:
        lost = int(bad["notifications"].sum())
        print(f"notifications they account for  : {lost} ({lost/len(sent):.0%} of everything sent)")
        print("\nworst offenders (notifications vs state events):")
        for sess, row in bad.head(5).iterrows():
            print(f"  {str(sess)[:34]:36} {row['notifications']:>4} notified, {row['state_events']:>4} state events")
        print("\nThose sessions can never be labelled engaged. A model trained")
        print("on them learns which agent reports telemetry, not what you did.")

    keep = labelled[labelled["session"].isin(check[check["labelable"]].index)]
    print(f"\nafter dropping them   : {len(keep)} notifications, "
          f"{keep['engaged'].mean():.1%} engaged")

    corr, by_density = density_check(labelled, check)
    print("\n--- density check ---")
    print("engagement by state events per notification:")
    for bucket, row in by_density.iterrows():
        print(f"  {str(bucket):12} n={int(row['size']):>4}  engaged={row['mean']:.1%}")
    print(f"correlation(density, engaged) = {corr:.3f}")
    if corr > 0.4:
        print("\nThe label tracks how chatty a session is, not what you did.")
        print("Dropping the silent sessions did not fix that; it selected the")
        print("other end of the same axis. This label cannot support a model,")
        print("and the honest output is this paragraph rather than a score.")
        print("\nTo do this properly amac has to record the thing itself:")
        print("whether a notification was opened and whether the question it")
        print("named was then answered. Neither is in the log today.")
        return 0

    if len(keep) < 100:
        print("too few left to model honestly")
        return 0

    feat = features(keep)
    train, test = timewise_split(feat)
    cols = ["prior_hour", "hour", "wants_attention"]
    xtr, ytr = train[cols], train["engaged"].astype(int)
    xte, yte = test[cols], test["engaged"].astype(int)

    print("\n--- can session-independent features predict it? ---")
    print(f"train {len(xtr)} (to {train['at'].max():%b %d}), test {len(xte)} (from {test['at'].min():%b %d})")
    if yte.nunique() < 2:
        print("the holdout has one class only; no honest score exists")
        return 0

    base = DummyClassifier(strategy="prior").fit(xtr, ytr)
    model = GradientBoostingClassifier(random_state=0).fit(xtr, ytr)
    base_auc = roc_auc_score(yte, base.predict_proba(xte)[:, 1])
    auc = roc_auc_score(yte, model.predict_proba(xte)[:, 1])
    print(f"baseline (predict the base rate) AUC : {base_auc:.3f}")
    print(f"gradient boosting                AUC : {auc:.3f}")

    print("\nfeature importances:")
    for name, imp in sorted(zip(cols, model.feature_importances_), key=lambda kv: -kv[1]):
        print(f"  {name:16} {imp:.3f}")

    print("\n--- what the current rule uses ---")
    for reason, g in keep.groupby("reason"):
        print(f"  {reason:18} {g['engaged'].mean():.1%} engaged over {len(g)}")
    print("\nThe live rule branches on reason and suppresses on focus and a")
    print("dedup window. If the two rates above are close, reason is not")
    print("carrying information about whether the notification was wanted.")

    if auc < base_auc + 0.05:
        print("\nVERDICT: no useful signal in session-independent features.")
        print("Reporting that is the result. A model shipped here would score")
        print("well on session identity and fail on the next session opened.")
    else:
        print(f"\nVERDICT: {auc - base_auc:+.3f} AUC over the baseline, on a")
        print("time-based holdout. Worth pursuing with more data.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
