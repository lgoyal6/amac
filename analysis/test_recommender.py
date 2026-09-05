"""Tests for the recommender, and for the gate that decides it may not ship.

The gate currently refuses on the real log, which is the correct answer for
three positives. A gate nobody has watched pass is a gate nobody has tested, so
most of what follows drives it with data that has a known answer.
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pandas as pd
import pytest

from recommender import (FEATURES, MIN_HOLDOUT_POSITIVES, Verdict, evaluate,
                         export, featurise, highest_affordable_threshold,
                         shipped_rules, split_by_time, turn_lengths)


def frame(n: int, **cols) -> pd.DataFrame:
    """A featurised frame, built directly so a test can state its own signal."""
    base = pd.Timestamp("2026-08-01T09:00:00Z")
    df = pd.DataFrame({
        "at": [base + pd.Timedelta(minutes=7 * i) for i in range(n)],
        "session": [f"s{i % 5}" for i in range(n)],
    })
    for f in FEATURES:
        df[f] = cols.get(f, np.zeros(n))
    return df


# ----------------------------------------------------------------- the gate --

def test_the_gate_refuses_before_there_are_enough_positives():
    """Which is the answer on the real log today: three positives of 1,249."""
    n = 300
    y = np.zeros(n, dtype=bool)
    y[:5] = True  # all in the training window, none in the holdout
    v = evaluate(frame(n), y)
    assert not v.trained, "a model was fitted on almost no positives"
    assert not v.exported
    assert str(MIN_HOLDOUT_POSITIVES) in v.why
    assert "noise" in v.why


def test_the_gate_refuses_a_model_that_does_not_beat_the_rules():
    """The baseline is what ships, not a strawman.

    Here the only thing that predicts a response is turn length, which is
    exactly what the shipped rule already keys on. A model has nothing to add,
    and must not be exported for tying.
    """
    n = 600
    rng = np.random.default_rng(0)
    turn = rng.choice([60.0, 3600.0], size=n)
    y = turn > 600
    v = evaluate(frame(n, log_turn_s=np.log1p(turn)), y)
    assert v.trained, "there were enough positives to train"
    assert not v.exported, f"a model that only rediscovers the rule was exported: {v.why}"
    assert "does not beat the rules" in v.why


def test_the_gate_passes_when_the_model_sees_what_the_rules_cannot():
    """And the case it exists to allow.

    The rules can only see "blocked" and turn length. Here half the responses
    come from turn-completes that carried a long message, which no rule looks
    at, so the model should retain more of them at the same volume.
    """
    n = 800
    rng = np.random.default_rng(1)
    wants = (rng.random(n) < 0.25).astype(float)
    turn = rng.choice([60.0, 3600.0], size=n)
    msg = rng.choice([20.0, 4000.0], size=n)
    # Acted on when blocked, or when a finished turn actually said something.
    y = (wants > 0) | ((wants == 0) & (msg > 1000))

    v = evaluate(frame(n, wants_attention=wants, log_turn_s=np.log1p(turn),
                       log_msg_len=np.log1p(msg)), y)
    assert v.trained
    assert v.exported, f"a genuinely better model was refused: {v.why}"
    assert v.model_kept > v.baseline_kept
    assert v.weights and v.weights["features"] == FEATURES


def test_the_comparison_is_at_equal_volume():
    """A model cannot win by sending more, which is not an improvement at all.

    It is the same dial the rules already have, turned the other way, and a
    comparison that allowed it would call every looser threshold a better
    model.
    """
    n = 800
    rng = np.random.default_rng(2)
    wants = (rng.random(n) < 0.25).astype(float)
    msg = rng.choice([20.0, 4000.0], size=n)
    y = (wants > 0) | (msg > 1000)
    v = evaluate(frame(n, wants_attention=wants, log_msg_len=np.log1p(msg)), y)
    assert v.trained
    assert v.model_sent <= v.baseline_sent, (
        f"the model was allowed to send {v.model_sent} against the rules' {v.baseline_sent}")


def test_ties_at_the_cutoff_cannot_smuggle_extra_notifications_through():
    """The bug the equal-volume test caught.

    A confident model gives a group of notifications the same score, and a
    cutoff that admits one of them admits all of them. Taking the budget-th
    score and sending everything at or above it therefore overshoots: on one
    synthetic set it sent 116 against a budget of 61. The cutoff has to be
    chosen over distinct scores.
    """
    scores = np.array([0.9] * 50 + [0.4] * 50)

    # A budget that cannot be spent exactly. Ten is less than the fifty tied at
    # the top, so the only affordable threshold sends none of them.
    t = highest_affordable_threshold(scores, 10)
    assert int((scores >= t).sum()) <= 10

    # A budget that fits the tied group exactly still spends it.
    t = highest_affordable_threshold(scores, 50)
    assert int((scores >= t).sum()) == 50

    # And a budget larger than everything sends everything, not more.
    t = highest_affordable_threshold(scores, 500)
    assert int((scores >= t).sum()) == 100


# ----------------------------------------------------------------- exporting --

def test_nothing_is_written_when_the_gate_did_not_pass(tmp_path: Path):
    out = tmp_path / "recommender.json"
    assert not export(Verdict(trained=True, exported=False, why="no"), out)
    assert not out.exists(), "a refused model was written to disk anyway"


def test_the_export_is_atomic_and_carries_the_feature_order(tmp_path: Path):
    """The daemon reads this file while this may be rewriting it.

    Feature order is the contract with the Go side, which scores against a
    list of coefficients. A reordering that was not exported together with its
    weights would score every notification against the wrong ones, quietly.
    """
    out = tmp_path / "recommender.json"
    v = Verdict(trained=True, exported=True, why="ok", weights={
        "features": FEATURES, "coef": [0.1] * len(FEATURES),
        "intercept": -1.0, "threshold": 0.5,
    })
    assert export(v, out)
    got = json.loads(out.read_text())
    assert got["features"] == FEATURES
    assert len(got["coef"]) == len(FEATURES)
    assert not list(tmp_path.glob("*.tmp")), "a partial file was left behind"


# ------------------------------------------------------------------ features --

def test_features_only_use_what_was_already_known():
    """A rate computed over the whole frame is leakage wearing a feature's name.

    The first notification cannot have seen any, and the counts must climb with
    arrival order rather than describe the file as a whole.
    """
    base = pd.Timestamp("2026-08-01T09:00:00Z")
    notifs = pd.DataFrame({
        "at": [base + pd.Timedelta(minutes=i) for i in range(6)],
        "session": ["s1"] * 6,
        "reason": ["turn-complete"] * 6,
        "message": ["hello"] * 6,
    })
    df = featurise(notifs, pd.DataFrame(columns=["at", "session"]))
    assert df["prior_hour"].iloc[0] == 0, "the first notification saw earlier ones"
    assert list(df["prior_hour"]) == sorted(df["prior_hour"]), "counts are not in arrival order"
    assert df["prior_hour"].iloc[-1] == 5


def test_a_session_with_no_history_is_not_treated_as_an_instant_turn():
    """Unmeasurable must not read as zero.

    The shipped rule fails open for exactly this reason, and a feature that
    said "zero seconds" would teach the model the opposite.
    """
    notifs = pd.DataFrame({
        "at": [pd.Timestamp("2026-08-01T09:00:00Z")],
        "session": ["never-seen"],
        "reason": ["turn-complete"],
        "message": [""],
    })
    got = turn_lengths(notifs, pd.DataFrame(columns=["at", "session"]))
    assert got[0] == 86400.0
    assert shipped_rules(featurise(notifs, pd.DataFrame(columns=["at", "session"])))[0], (
        "a session with no history was suppressed")


def test_the_split_is_by_time_not_at_random():
    """Notifications arrive in bursts, so a random split puts one burst on both
    sides of it and the score measures memorising the burst."""
    n = 100
    df = frame(n)
    tr, te, _, _ = split_by_time(df, pd.Series(np.zeros(n, dtype=bool)), 0.3)
    assert len(tr) == 70 and len(te) == 30
    assert tr["at"].max() <= te["at"].min(), "the training window runs past the holdout"


def test_the_shipped_rules_always_send_a_blocked_session():
    """The fourteen a day that are actually acted on. Nothing about how long an
    agent worked changes whether it is now stuck waiting for a person."""
    df = frame(3, wants_attention=np.array([1.0, 0.0, 1.0]),
               log_turn_s=np.log1p(np.array([1.0, 1.0, 1.0])))
    assert list(shipped_rules(df)) == [True, False, True]


def test_the_golden_artifact_matches_what_this_exports():
    """The same file internal/attention checks, checked from this side too.

    A contract only one side verifies is a contract that breaks in the
    direction nobody is looking. If FEATURES changes here without the golden
    artifact being regenerated, the Go side would keep loading a model whose
    coefficients no longer line up with the names it scores against, and every
    notification would be ranked by the wrong weights without anything failing.
    """
    golden = Path(__file__).resolve().parent.parent / "internal" / "attention" / "testdata" / "recommender.json"
    if not golden.exists():
        pytest.skip("no golden artifact checked in")
    got = json.loads(golden.read_text())
    assert got["features"] == FEATURES, (
        "FEATURES changed without regenerating internal/attention/testdata/recommender.json")
    assert len(got["coef"]) == len(FEATURES)
    assert 0 < got["threshold"] < 1
