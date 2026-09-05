"""Tests for the analysis layer.

Every one of these is built from synthetic rows rather than the real log. An
analysis whose tests need one person's laptop is an analysis nobody else can
check, and the artifacts these guard against are exactly the kind of thing that
looks fine until someone reproduces it.
"""

from __future__ import annotations

import json
import sqlite3

import pandas as pd
import pytest

import amaclog
from notifications import (density_check, instrumentation_check, label, readiness,
                           timewise_split)

T0 = pd.Timestamp("2026-09-01 12:00:00", tz="UTC")


def make_log(tmp_path, rows):
    """A minimal events.db with the same schema the daemon writes."""
    path = tmp_path / "events.db"
    con = sqlite3.connect(path)
    con.execute("""CREATE TABLE events (
        seq INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, kind TEXT NOT NULL,
        source TEXT NOT NULL, session TEXT NOT NULL DEFAULT '', payload BLOB)""")
    for at, kind, session, payload in rows:
        con.execute("INSERT INTO events (at, kind, source, session, payload) VALUES (?,?,?,?,?)",
                    (at.isoformat().replace("+00:00", "Z"), kind, "test", session, json.dumps(payload)))
    con.commit()
    con.close()
    return path


def notif(at, session, sent=True, reason="turn-complete"):
    return (at, "attention", session, {"outcome": {"sent": sent}, "reason": reason, "message": "x"})


def state(at, session, st="working"):
    return (at, "session.state", session, {"state": st})


# ----------------------------------------------------------------- loader ---

def test_runs_folds_the_rename_and_dedupes(tmp_path):
    """hacklist was hacklist-sf, and reporting is keyed on automation/id, so the
    rename made runs it had already seen look new. Both sides must fold and
    dedupe identically or the log and the board disagree about how many times
    something ran."""
    shared = {"automation": "hacklist-sf", "id": "33793730994", "status": "failed",
              "started": T0.isoformat(), "detail": "run failure"}
    renamed = dict(shared, automation="hacklist")
    fresh = {"automation": "hacklist", "id": "999", "status": "ok",
             "started": T0.isoformat(), "detail": "swept"}
    db = make_log(tmp_path, [
        (T0, "automation.run", "hacklist-sf", shared),
        (T0, "automation.run", "hacklist", renamed),
        (T0, "automation.run", "hacklist", fresh),
    ])
    df = amaclog.runs(db)
    assert set(df["automation"]) == {"hacklist"}
    assert len(df) == 2, "the same run under two names is one run"


def test_notifications_keep_the_suppressed_ones(tmp_path):
    """A log of only what was sent cannot answer whether suppression is any
    good, which is the entire question."""
    db = make_log(tmp_path, [
        notif(T0, "a", sent=True),
        (T0, "attention", "b", {"outcome": {"sent": False, "why": "already notified 3s ago"},
                                "reason": "wants-attention"}),
    ])
    df = amaclog.notifications(db)
    assert len(df) == 2
    assert df["sent"].tolist() == [True, False]
    assert "already notified" in df.iloc[1]["why"]


def test_missing_log_is_an_error_not_an_empty_frame(tmp_path):
    """An empty dataframe from a missing file reads as 'nothing happened',
    which is the same shape as a real answer and much worse."""
    with pytest.raises(FileNotFoundError):
        amaclog.events(db=tmp_path / "nope.db")


# ------------------------------------------------------------------ label ---

def test_label_only_counts_transitions_inside_the_window():
    notifs = pd.DataFrame({"at": [T0, T0], "session": ["a", "b"]})
    states = pd.DataFrame({
        "at": [T0 + pd.Timedelta(minutes=2), T0 + pd.Timedelta(minutes=45)],
        "session": ["a", "b"],
    })
    out = label(notifs, states)
    assert out["engaged"].tolist() == [True, False]


def test_label_ignores_transitions_before_the_notification():
    """A state change that happened first cannot have been caused by a message
    sent afterwards."""
    notifs = pd.DataFrame({"at": [T0], "session": ["a"]})
    states = pd.DataFrame({"at": [T0 - pd.Timedelta(minutes=1)], "session": ["a"]})
    assert label(notifs, states)["engaged"].tolist() == [False]


# --------------------------------------------------------------- artifacts ---

def test_instrumentation_check_flags_sessions_that_cannot_be_labelled():
    """Codex sessions emitted 243 notifications and no state events, so they
    could never be labelled engaged. A model handed that data learns which
    adapter reports telemetry and is right for the wrong reason."""
    notifs = pd.DataFrame({"at": [T0] * 5, "session": ["quiet"] * 3 + ["chatty"] * 2})
    states = pd.DataFrame({"at": [T0] * 6, "session": ["chatty"] * 6})
    got = instrumentation_check(notifs, states)
    assert not got.loc["quiet", "labelable"]
    assert got.loc["chatty", "labelable"]


def test_density_check_catches_the_mirror_artifact():
    """Dropping silent sessions removes one bias by selecting the other end of
    the same axis: a session emitting constant state will have one inside any
    window and labels engaged whatever happened. If engagement tracks density,
    the label is telemetry volume."""
    rows, states = [], []
    for i in range(30):  # chatty: many states, always "engaged"
        rows.append((T0 + pd.Timedelta(minutes=i), "chatty"))
        states += [(T0 + pd.Timedelta(minutes=i, seconds=s), "chatty") for s in (1, 2, 3)]
    for i in range(30):  # silent: none, never "engaged"
        rows.append((T0 + pd.Timedelta(minutes=i), "silent"))

    notifs = pd.DataFrame(rows, columns=["at", "session"])
    st = pd.DataFrame(states, columns=["at", "session"])
    labelled = label(notifs, st)
    corr, by = density_check(labelled, instrumentation_check(notifs, st))
    assert corr > 0.4, "a label that tracks chattiness must be detected"
    assert by["mean"].iloc[0] < by["mean"].iloc[-1]


# ------------------------------------------------------------------ split ---

def test_split_is_by_time_not_at_random():
    """Notifications arrive in bursts, so a random split puts two halves of one
    burst on both sides and leaks. The honest question is whether last month
    predicts this week."""
    df = pd.DataFrame({"at": pd.date_range(T0, periods=100, freq="min", tz="UTC")})
    train, test = timewise_split(df, holdout=0.3)
    assert len(train) == 70 and len(test) == 30
    assert train["at"].max() < test["at"].min(), "no training row may postdate a test row"


# --------------------------------------------------------------- policies ---

from policies import Decision, always, backoff, dedup, dedup_urgent, replay  # noqa: E402


def stream(*specs):
    """(minutes_offset, session, reason) tuples into decisions."""
    return [Decision(at=T0 + pd.Timedelta(minutes=m), session=s, reason=r) for m, s, r in specs]


def test_dedup_suppresses_only_inside_its_window():
    d = stream((0, "a", "turn-complete"), (2, "a", "turn-complete"), (20, "a", "turn-complete"))
    assert replay(d, "x", dedup(300)).sent == 2, "the 2-minute repeat is inside 5 minutes"


def test_dedup_is_per_session():
    """Two agents finishing at once are two things you need to know, not one."""
    d = stream((0, "a", "turn-complete"), (0, "b", "turn-complete"))
    assert replay(d, "x", dedup(300)).sent == 2


def test_urgent_is_never_suppressed():
    """turn-complete is an agent reporting it finished; wants-attention is one
    that is stuck. Treating them the same is what the live rule does."""
    d = stream((0, "a", "turn-complete"), (1, "a", "wants-attention"), (2, "a", "turn-complete"))
    assert replay(d, "x", dedup(300)).sent == 1
    assert replay(d, "x", dedup_urgent(300)).sent == 2


def test_backoff_widens_then_resets_when_quiet():
    """A fixed window treats the fifth message in ten minutes like the first."""
    burst = stream(*[(i, "a", "turn-complete") for i in (0, 1, 2, 4, 8, 16)])
    assert replay(burst, "x", backoff(60)).sent < replay(burst, "x", dedup(60)).sent
    # After a long silence the streak resets, so a later message is not
    # penalised for a burst an hour ago.
    later = burst + stream((200, "a", "turn-complete"))
    assert replay(later, "x", backoff(60)).sent == replay(burst, "x", backoff(60)).sent + 1


def test_every_policy_mentions_a_session_at_least_once():
    """A policy that silences a session entirely is not quieter, it is broken.
    Coverage is the constraint that makes a volume reduction meaningful."""
    d = stream(*[(i, f"s{i%7}", "turn-complete") for i in range(60)])
    ceiling = replay(d, "all", always)
    for name, pol in [("dedup", dedup(300)), ("backoff", backoff(60))]:
        got = replay(d, name, pol)
        assert got.sessions == ceiling.sessions, f"{name} lost a session entirely"
        assert got.sent <= ceiling.sent


def test_replay_is_deterministic():
    """A counterfactual that changes between runs cannot be compared against."""
    d = stream(*[(i, f"s{i%5}", "turn-complete") for i in range(80)])
    assert replay(d, "a", backoff(60)).sent == replay(d, "b", backoff(60)).sent


# ------------------------------------------------------- observable label ---

from notifications import responded  # noqa: E402


def test_a_board_open_soon_after_counts_as_a_response():
    """The label amac can now observe. Unlike a session state change it cannot
    be produced by an agent: it takes a person with a token, which is exactly
    what the previous label was missing."""
    notifs = pd.DataFrame({"at": [T0, T0], "session": ["a", "b"]})
    opens = pd.DataFrame({
        "at": [T0 + pd.Timedelta(minutes=2)],
        "session": ["a"],
    })
    got = responded(notifs, opens)
    assert got["responded"].tolist() == [True, True], "any open is weak evidence for both"
    assert got["responded_directly"].tolist() == [True, False], \
        "only the deep-linked session is answered specifically"


def test_an_open_before_the_notification_is_not_a_response():
    notifs = pd.DataFrame({"at": [T0], "session": ["a"]})
    opens = pd.DataFrame({"at": [T0 - pd.Timedelta(minutes=5)], "session": ["a"]})
    assert responded(notifs, opens)["responded"].tolist() == [False]


def test_an_open_long_after_is_not_a_response():
    notifs = pd.DataFrame({"at": [T0], "session": ["a"]})
    opens = pd.DataFrame({"at": [T0 + pd.Timedelta(hours=3)], "session": ["a"]})
    assert responded(notifs, opens)["responded"].tolist() == [False]


def test_no_opens_recorded_yet_is_false_rather_than_an_error():
    """The label starts empty by construction, on the day recording shipped. An
    empty frame must read as 'nothing responded yet', not crash the report."""
    notifs = pd.DataFrame({"at": [T0], "session": ["a"]})
    got = responded(notifs, pd.DataFrame(columns=["at", "session"]))
    assert got["responded"].tolist() == [False]
    assert got["responded_directly"].tolist() == [False]


def test_readiness_reports_no_window_before_any_open():
    notifs = pd.DataFrame({"at": pd.to_datetime(["2026-09-05T10:00:00Z"]), "session": ["s1"]})
    got = readiness(notifs, pd.DataFrame(columns=["at", "session"]))
    assert got["positives"] == 0
    assert got["ready"] is False
    assert got["days_left"] is None
    assert "has not started" in got["note"]


def test_readiness_does_not_extrapolate_a_rate_from_one_event():
    """One open is a data point, not a rate.

    Without a floor on the observation window, a single open makes days ~= 0
    and the implied opens-per-day runs off to infinity, which would report the
    label as days away from ready on the strength of one click.
    """
    at = pd.to_datetime(["2026-09-05T10:00:00Z"])
    notifs = pd.DataFrame({"at": at, "session": ["s1"]})
    opens = pd.DataFrame({"at": at + pd.Timedelta(minutes=1), "session": ["s1"]})
    got = readiness(notifs, opens, want=200)
    assert got["positives"] == 1
    assert got["days"] >= 0.5
    assert got["per_day"] <= 2.0, "a single open implied an impossible rate"
    assert got["ready"] is False


def test_readiness_says_ready_only_with_enough_positives():
    """The threshold exists so a model is not trained on a holdout of twelve."""
    base = pd.Timestamp("2026-09-01T00:00:00Z")
    at = pd.to_datetime([base + pd.Timedelta(hours=i) for i in range(300)])
    notifs = pd.DataFrame({"at": at, "session": ["s1"] * 300})
    opens = pd.DataFrame({"at": at + pd.Timedelta(minutes=1), "session": ["s1"] * 300})
    got = readiness(notifs, opens, want=200)
    assert got["positives"] == 300
    assert got["ready"] is True
    assert got["days_left"] == 0.0
