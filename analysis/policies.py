"""Compare notification policies by replaying what actually happened.

A live A/B on notifications would take weeks and, worse, would be scored on an
outcome amac cannot currently observe: `notifications.py` shows that "did the
session react" measures how much telemetry an adapter emits, at a correlation of
0.64 with chattiness. Running an experiment against an invalid metric produces a
confident number and no knowledge.

So this is counterfactual replay, which is what you do when the outcome is
unavailable but the decisions are recorded. Every attention decision amac ever
made is in the log with its timestamp, session and reason. Each policy below is
re-run over that exact stream, and the comparison uses only quantities the
stream itself can settle:

  volume     how many messages the policy would have sent
  coverage   sessions it would have told you about at least once
  delay      how long after a session first wanted attention it first said so

None of those need to know whether you read the message. They are the honest
half of the question, and the half that is missing is stated rather than
estimated.

Run it:  python policies.py            (or --db path/to/events.db)
"""

from __future__ import annotations

import argparse
import sys
from dataclasses import dataclass, field

import pandas as pd

import amaclog


@dataclass
class Decision:
    at: pd.Timestamp
    session: str
    reason: str


@dataclass
class Result:
    name: str
    sent: int = 0
    sessions: set = field(default_factory=set)
    delays: list = field(default_factory=list)

    def summary(self, total: int, all_sessions: int) -> str:
        cov = len(self.sessions) / all_sessions if all_sessions else 0
        med = pd.Series(self.delays).median() if self.delays else pd.NaT
        med_s = f"{med.total_seconds():.0f}s" if self.delays else "n/a"
        return (f"  {self.name:<26} sent={self.sent:>5} ({self.sent/total:>5.1%})"
                f"  covered={len(self.sessions):>3}/{all_sessions} ({cov:>4.0%})"
                f"  median delay={med_s}")


# Each policy takes the decision and the state it is allowed to remember, and
# returns whether it would send. They are deliberately pure over the recorded
# stream: a policy that needed to know what the user did next could not be
# replayed, which is the constraint that keeps this honest.

def always(_d: Decision, _s: dict) -> bool:
    """No suppression at all. The ceiling on volume and the floor on delay."""
    return True


def dedup(window_s: float):
    """Suppress a repeat on the same session inside a window.

    This is half of what amac ships today.
    """
    def policy(d: Decision, state: dict) -> bool:
        last = state.get(d.session)
        if last is not None and (d.at - last).total_seconds() < window_s:
            return False
        state[d.session] = d.at
        return True
    return policy


def dedup_urgent(window_s: float):
    """Dedup, except never suppress a session that is asking for you.

    turn-complete is an agent reporting it finished; wants-attention is one
    that is stuck. Treating them the same is what the live rule does, and it is
    the first thing worth questioning.
    """
    def policy(d: Decision, state: dict) -> bool:
        if d.reason == "wants-attention":
            state[d.session] = d.at
            return True
        last = state.get(d.session)
        if last is not None and (d.at - last).total_seconds() < window_s:
            return False
        state[d.session] = d.at
        return True
    return policy


def backoff(base_s: float, cap_s: float = 3600):
    """Widen the window each time the same session repeats, and reset when it
    goes quiet for a while.

    The density finding in notifications.py is that a session notifying
    repeatedly within an hour is the shape most present in the data. A fixed
    window treats the fifth message in ten minutes exactly like the first.
    """
    def policy(d: Decision, state: dict) -> bool:
        last, streak = state.get(d.session, (None, 0))
        if last is not None:
            gap = (d.at - last).total_seconds()
            if gap > cap_s:
                streak = 0
            elif gap < min(base_s * (2 ** streak), cap_s):
                return False
        state[d.session] = (d.at, streak + 1)
        return True
    return policy


def replay(decisions: list[Decision], name: str, policy) -> Result:
    res = Result(name=name)
    state: dict = {}
    wanted_since: dict = {}
    for d in decisions:
        wanted_since.setdefault(d.session, d.at)
        if policy(d, state):
            res.sent += 1
            if d.session not in res.sessions:
                res.sessions.add(d.session)
                res.delays.append(d.at - wanted_since[d.session])
    return res


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", default=None)
    args = ap.parse_args(argv)

    df = amaclog.notifications(args.db).sort_values("at")
    if df.empty:
        print("no attention decisions recorded")
        return 0

    # Replay the requests, not the outcomes. What amac decided is the thing
    # being compared against, so the input is every time an agent asked for
    # attention, including the ones the live rule suppressed.
    decisions = [Decision(at=r.at, session=r.session, reason=r.reason or "")
                 for r in df.itertuples()]
    total = len(decisions)
    all_sessions = df["session"].nunique()

    print(f"replaying {total} recorded requests across {all_sessions} sessions\n")

    live_sent = int(df["sent"].sum())
    print(f"  {'amac as it runs today':<26} sent={live_sent:>5} ({live_sent/total:>5.1%})"
          f"  covered={df[df['sent']]['session'].nunique():>3}/{all_sessions}"
          f" ({df[df['sent']]['session'].nunique()/all_sessions:>4.0%})  median delay=n/a")
    print("  (delay is n/a for the live rule: it also suppresses on window focus,")
    print("   which is not in the log, so its decisions cannot be recomputed)\n")

    results = [
        replay(decisions, "send everything", always),
        replay(decisions, "dedup 60s", dedup(60)),
        replay(decisions, "dedup 300s", dedup(300)),
        replay(decisions, "dedup 300s, urgent free", dedup_urgent(300)),
        replay(decisions, "exponential backoff 60s", backoff(60)),
    ]
    for r in results:
        print(r.summary(total, all_sessions))

    best = min(results[1:], key=lambda r: r.sent)
    ceiling = results[0]
    print(f"\nquietest policy: {best.name}, {best.sent} messages against "
          f"{ceiling.sent} unsuppressed, a {1 - best.sent/ceiling.sent:.0%} reduction.")
    if len(best.sessions) < len(ceiling.sessions):
        missed = len(ceiling.sessions) - len(best.sessions)
        print(f"It costs {missed} session(s) that would never have been mentioned at all.")
    else:
        print("It still mentions every session at least once, so the reduction is"
              " in repeats rather than in coverage.")

    print("\nWhat this cannot tell you: whether the messages it removed were the")
    print("ones you did not want. That needs an outcome amac does not record.")
    print("See notifications.py for why the obvious substitute is invalid.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
