# analysis

Read-only Python over amac's event log, for the questions that want a dataframe
rather than a dashboard.

```bash
python -m venv .venv && .venv/bin/pip install -r requirements.txt
.venv/bin/python notifications.py            # or --db path/to/events.db
.venv/bin/python -m pytest                   # the tests use synthetic logs only
```

Nothing here writes to the log. Analysis that can mutate its own input is
analysis nobody can reproduce, and the daemon is usually holding the file, so
`amaclog` opens it read-only through a URI rather than taking a write lock on a
live system to ask about the past.

## What `notifications.py` found

amac sends a Discord message when an agent finishes a turn or wants attention,
and suppresses on two hand-tuned rules: a dedup window, and whether you are
looking at that session. Nothing had checked whether the notifications that
survive those rules were worth sending, and 2,006 recorded decisions looked
like enough data to learn from.

They are not, and the reason is the useful part.

**The obvious label is an artifact.** "Did that session change state soon
after" is only evidence when the session reports state at all. Claude's adapter
emits state through ACP; Codex's barely does. Codex accounts for 243
notifications and zero state events, so it can never be labelled engaged
whatever actually happened. A model handed this data learns which adapter
reports telemetry and is right for entirely the wrong reason.

**Filtering does not rescue it.** Dropping thinly instrumented sessions removes
that bias by selecting the other end of the same axis: a session emitting
constant state will have one inside any window and labels engaged regardless.
Engagement rises monotonically with chattiness, 0% to 15% to 60% to 82%, at a
correlation of 0.64. The label is telemetry volume.

**So the answer is a negative result**, and the script prints it rather than a
score. To do this properly amac has to record the thing itself: whether a
notification was opened, and whether the question it named was then answered.
Neither is in the log today.

Two smaller findings survive and are real:

- `reason` carries almost nothing. turn-complete and wants-attention engage at
  nearly the same rate, and it is the one categorical distinction the live rule
  branches on.
- Notification density does correlate, and the live rule does not look at it.

## Why the tests use synthetic logs

An analysis whose tests need one person's laptop is one nobody else can check.
Every test builds its own events.db with the schema the daemon writes, including
the two artifact detectors, so the thing that caught the bug can itself be
checked.

One of those tests earned its place immediately: it caught that
`astype("int64")` on a tz-aware column yields microseconds in pandas 3 while
`Timedelta.value` is nanoseconds, which had made the window a thousand times too
wide and every notification look engaged.
