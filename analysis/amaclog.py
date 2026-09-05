"""Read amac's event log into dataframes.

The log is the only store amac has, and everything in the dashboard is a view
over it. This is the same idea for analysis: one loader, so a question asked in
Python and a question asked by the board are asked of the same rows.

Everything here is read-only. Nothing in this package writes to the log, which
is deliberate: analysis that can mutate its own input is analysis nobody can
reproduce.
"""

from __future__ import annotations

import json
import os
import sqlite3
from pathlib import Path

import pandas as pd


def default_path() -> Path:
    """Where the daemon keeps its log, overridable for a copy."""
    return Path(os.environ.get("AMAC_DB", Path.home() / ".amac" / "events.db"))


def events(kind: str | None = None, db: str | Path | None = None) -> pd.DataFrame:
    """Every event, or every event of one kind, with its payload expanded.

    Opened read-only through a URI. The daemon is usually running and writing
    to this file, and an analysis script has no business taking a write lock on
    a live system to answer a question about the past.
    """
    path = Path(db) if db else default_path()
    if not path.exists():
        raise FileNotFoundError(f"no event log at {path}")

    with sqlite3.connect(f"file:{path}?mode=ro", uri=True) as con:
        sql = "SELECT seq, at, kind, source, session, payload FROM events"
        params: tuple = ()
        if kind:
            sql += " WHERE kind = ?"
            params = (kind,)
        df = pd.read_sql_query(sql + " ORDER BY seq", con, params=params)

    df["at"] = pd.to_datetime(df["at"], format="mixed", utc=True)
    df["payload"] = df["payload"].map(_loads)
    return df


def _loads(raw) -> dict:
    if raw is None:
        return {}
    try:
        v = json.loads(raw)
    except (ValueError, TypeError):
        return {}
    return v if isinstance(v, dict) else {"value": v}


def field(df: pd.DataFrame, *names: str) -> pd.DataFrame:
    """Lift payload keys into columns, so the rest reads like a dataframe."""
    out = df.copy()
    for n in names:
        out[n] = out["payload"].map(lambda p, n=n: p.get(n))
    return out


def runs(db: str | Path | None = None) -> pd.DataFrame:
    """Automation runs, one row each, with the rename folded.

    hacklist was called hacklist-sf until the board stopped being SF-only, and
    reporting is suppressed by an automation/id key, so the rename made every
    run it had already seen look new. Folding the name and deduping on the run's
    own id is the same correction the Go side makes, and it belongs here too or
    the two disagree about how many times something ran.
    """
    df = field(events("automation.run", db), "automation", "id", "status", "started", "detail")
    df["automation"] = df["automation"].replace({"hacklist-sf": "hacklist"})
    df["started"] = pd.to_datetime(df["started"], format="mixed", utc=True, errors="coerce")
    return df.drop_duplicates(subset=["automation", "id"], keep="first")


def sessions(db: str | Path | None = None) -> pd.DataFrame:
    """Session state transitions."""
    df = field(events("session.state", db), "state")
    return df[df["state"].notna()]


def notifications(db: str | Path | None = None) -> pd.DataFrame:
    """Attention decisions, including the ones that were suppressed.

    The suppressed rows are why this is worth analysing at all. A log of only
    what was sent cannot answer whether the suppression rule is any good.
    """
    # id is lifted because it is the join key for anything that happened
    # afterwards. Without it the exact label silently answers False for every
    # row rather than failing, which is the worst way for a join to be wrong.
    df = field(events("attention", db), "outcome", "reason", "id")
    df["sent"] = df["outcome"].map(lambda o: bool(o.get("sent")) if isinstance(o, dict) else False)
    df["why"] = df["outcome"].map(lambda o: o.get("why", "") if isinstance(o, dict) else "")
    return df.drop(columns=["outcome"])


def opens(db: str | Path | None = None) -> pd.DataFrame:
    """Board opens by somebody holding a token.

    This is the half amac was missing. It knew what it sent and never what
    happened next, which is why the obvious outcome label turned out to measure
    which adapter reports telemetry rather than anything a person did. An open
    is a raw fact with no window attached; whether one answers a particular
    notification is decided here, where the window can be argued with.
    """
    df = field(events("board.opened", db), "session", "source")
    return df


def acts(db: str | Path | None = None) -> pd.DataFrame:
    """Deliberate acts by a person: writes through the API, and followed links.

    Board opens alone were far too rare to be a label. Counted over the four
    weeks before this was added, amac delivered a median of 111 notifications a
    day and could see roughly two human actions a day, so two hundred labelled
    examples were about a hundred days out. Not because a person did little,
    but because only a page load was recorded.

    The rule the daemon applies is whether an agent could have caused it. Reads
    are excluded because the board polls, and treating a poll as engagement is
    exactly how the previous label came to measure adapter chattiness.

    The `notice` column is the strong case. It is set only when the act arrived
    by a signed link out of one specific notification, which is the only signal
    here that says which alert was answered rather than that somebody showed up
    afterwards.
    """
    df = field(events("human.acted", db), "session", "action", "notice", "via")
    return df
