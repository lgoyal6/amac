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

## The recommender

`recommender.py` trains the notification ranker, and refuses to ship one it
cannot show beats the rules that already ship.

```
.venv/bin/python recommender.py            # report only
.venv/bin/python recommender.py --write    # export if the gate passes
```

The baseline is not "send everything". It is amac's shipped rule set: always
send a session blocked on a person, suppress a finished turn that took under
ten minutes. That already removes about nine tenths of the volume, so a model
has to beat a genuinely good rule rather than a strawman.

Three things it will not do:

- **Train on the old engagement label.** "The session changed state soon after"
  correlates 0.64 with how chatty an adapter is, so a model fitted to it learns
  which CLI reports telemetry. `notifications.py` proves this.
- **Train on a description of behaviour.** Being told which notifications matter
  sets the window and decides which acts count as answers. It is not a label.
- **Export a model it cannot show beats the baseline** on data it never saw,
  split by time rather than at random, at the same volume. A model that keeps
  more by sending more has not improved on the rules, it has turned the same
  dial the other way.

Serving is in Go (`internal/attention/model.go`), so the daemon stays one
binary that launchd starts and nothing about deciding whether to send a
notification depends on a virtualenv being intact. The artifact between them is
a few logistic-regression weights, small enough to read: you can open the file
and see what the model believes. The model can only ever suppress, because it
runs after the rules have said send.

`internal/attention/testdata/recommender.json` is a real exported artifact and
is the contract between the two sides. Both check it. Regenerate it if
`FEATURES` changes, or the Go side will score notifications against
coefficients that no longer line up with the names.

On the log as it stands, the gate refuses: 3 labelled notifications out of
1,249.
