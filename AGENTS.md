# Notes for whoever works on this next

Written for the next person to touch this repository, which may well be you six
months from now. It is not a tour of the code; it is the things that cost a day
to learn and would cost another day to learn again.

## What this is, and what it is not

This is the protocol half of a database. The storage is
[litekv](https://github.com/tillknuesting/litekv), a separate module and a
separate repository, and this one reaches it through the same exported API any
other caller would.

**That boundary is the whole design and it is not a style rule.** Nothing here
may reach into the engine's internals, because there is no way to: it is a
dependency, and the compiler enforces what a directory boundary only suggested.
What it buys is that these tests are worth something — they exercise the exported
API and nothing else, so a change in the engine that breaks a caller breaks them.

What follows from it, practically:

- **A change that needs something the engine does not export is two commits in
  two repositories and a new tag.** That is a feature. It was a `git commit -a`
  away when they lived together, and "a quiet widening while building something
  else" is exactly the thing the separation is meant to make somebody stop and
  think about.
- **To test against an unreleased engine**, point this module at a working tree
  and remember to put it back:

  ```bash
  go mod edit -replace github.com/tillknuesting/litekv=../litekv
  go test -race ./...
  go mod edit -dropreplace github.com/tillknuesting/litekv
  ```

  A `replace` that reaches a commit is a `go install` that fails for everybody
  else, so it must never be committed.
- **There is no single command that runs both suites.** The engine has its own,
  including chaos runs that fail a disk one operation at a time, and none of that
  is reachable from here.
- **The one thing that does cross is the mutation runner**, which is a package in
  the engine module rather than a copy in each repository. Changing it means
  changing it there, and a sweep here will not see that change until the engine
  is tagged and this module's `require` is raised.

## Where things are

| file                 | owns                                                              |
| -------------------- | ----------------------------------------------------------------- |
| `main.go`            | flags, defaults, the listener, signals, and the order things shut down in |
| `server/server.go`   | the handler, its options, the routes, and the writer in front      |
| `server/keys.go`     | one key at a time: read, write, delete, and the expiry header      |
| `server/ndjson.go`   | how a key or a value goes in a JSON object, in both directions     |
| `server/batch.go`    | `POST /v1/batch`: a body of operations parsed whole, then stored   |
| `server/scan.go`     | `GET /v1/keys`: ranges and prefixes, the query, and the limits     |
| `server/errors.go`   | which store error is which status, and what a client is not told   |
| `server/role.go`     | leader or replica, promotion, status, and reads that are not stale |
| `server/ops.go`      | health, metrics, request logging, and the bearer token             |
| `server/acks.go`     | who is following, how far each has got, and what a write waits for |
| `server/replica.go`  | the frames, the position on the wire, and the leader's stream      |
| `server/follower.go` | the other end of it: dial, apply, reconnect                        |
| `tools/mutate/`      | this repository's own list of what to break, and its timeout       |

`package server` is an `http.Handler` and nothing else: it does not listen, it
does not open the store, and it does not close it. `main.go` does all three. That
is what lets every test in here run in-process with `httptest` and no port.

## Verifying a change

Everything below has to pass before anything is pushed.

```bash
gofmt -l . && go vet ./... && staticcheck ./...
go fix ./... && git diff --exit-code   # the modernisers, and CI runs it too
go test -race ./...
GOMAXPROCS=1 go test ./...
go test -run xxx -fuzz '^FuzzReadFrame$' -fuzztime 30s ./server/
go test -run xxx -fuzz '^FuzzBatchBody$' -fuzztime 30s ./server/
go run ./tools/mutate
```

The `^...$` matters: `-fuzz FuzzBatch` would match more than one target and the
go tool refuses to run rather than choosing.

`go fix` rewrites rather than reports, which is why the check is to run it and
see whether anything moved. Read what it did before committing it, and then run
the sweep: `mutations.go` matches source text exactly, so an automated rewrite
is exactly the thing that turns a mutation into a silent SKIP, and the sweep is
the only check that would notice.

`GOMAXPROCS=1` is not paranoia — the engine's lock shards on it, so a one-core
machine takes a different path through the store, and background merging stops
being in the background.

`tools/mutate` is a hundred and nine mutations across eight workers, each in its
own copy of the repository, printing verdicts as they land. The machinery is
`github.com/tillknuesting/litekv/mutate` — the engine module, which this one
depends on anyway, so that neither repository holds a copy of a runner. What is
here is the list and the ninety-second timeout this suite needs:

```bash
go run ./tools/mutate                 # all of them
go run ./tools/mutate replica batch   # only those whose name matches
```

**Every mutation must be caught except the five listed below.** A sixth survivor
is news, not noise: it means something the code promises has no test holding it
there. Adding a mutation when you add a behaviour is four lines and part of
writing the code.

The end-to-end run matters separately, because a handler test builds its own
request and the interesting question is often whether a request can be built at
all:

```bash
go build -o /tmp/litekvd . && /tmp/litekvd -dir "$(mktemp -d)" -addr 127.0.0.1:18080 &
curl -X PUT --data-binary 'v' http://127.0.0.1:18080/v1/keys/a%2Fb && curl http://127.0.0.1:18080/v1/keys/a%2Fb
```

Anything touching replication gets two of them, one following the other, with the
follower killed and restarted while the leader is being written to. Two traps
there: count what `ForEach` yields when comparing two stores and never `Len`,
which counts index entries and not live keys; and stop both with a signal rather
than a kill, because how long a leader takes to go down with a follower attached
is itself a thing that has been broken.

## What these tests can and cannot say

**Two ways of asking, and the difference matters.** `httptest.NewRecorder`
drives the handler directly and is enough for anything about statuses, headers
and bodies. `httptest.NewServer` puts a real client, a real parser and a real
socket in between, and is the only way to answer a question about whether a
request can be *built* — which is exactly what `TestKeyOfAnyBytes` asks. A
recorder is handed a request some other code already made.

**A closed `Server` is not a closed store, and that is what makes the wiring
testable.** There is no way from outside the package to see that a write went
through the queue rather than straight to the store: the record lands either
way. What can be seen is that closing the `Server` stops writes with a 503 while
reads carry on, which is only true if the queue is in the path.
`TestClosingTheServerStopsWritesAndNotReads` is doing that job, and three
mutations depend on it. If it is ever weakened, four things stop being tested.

**Five mutations survive on purpose, and no others.** Written down so that
nobody goes hunting for a test that was never written, and so that a sixth
survivor is read as news rather than as normal:

- **`Options.Queue` dropped on the way to `litekv.WriterOptions`.** The depth
  decides when a sender blocks and how large a group gets, and neither can be
  asserted without a timing test. A pass-through of an engine option the engine
  tests.
- **The snapshot's hold released before `Follow` takes its own.** `Follow` calls
  `db.Hold` first and only then releases the caller's, so the mutation opens a
  window between the two rather than removing a hold — a scheduling race, and
  the engine's `Follow` has the reason in its own comment. What the mechanism
  does is tested next door by `TestDBHoldKeepsALogFromMerging` and
  `TestDBFollowIsNotStrandedByAMerge`. A test here would be a race with a
  deadline, which is the kind this repo does not write.
- **Promote raising the term before it stops following, rather than after.**
  Both orders reach the same end state, and nothing observable from outside the
  package tells them apart — which is exactly why the code says why the order
  matters and this says no test enforces it. What the wrong order opens is a
  window in which the store's term is above its leader's and the follower is
  still running, so a batch that arrives in it is applied by a node that has
  just fenced the sender. Reproducing that needs the window held open, and
  there is no seam for it.
- **The drain after a snapshot.** `take` consumes whatever `ApplySnapshot` did
  not, and that cannot fire today: ApplySnapshot reads to the end of what it is
  given and reports an error if it stops early, and `take` returns that error,
  which ends the stream — so there is never a next frame for a desynchronised
  connection to ruin. It stays because "consume exactly what the header
  promised" is a property of the framing and not of the store, and an
  ApplySnapshot that one day returned nil having read less would leave the
  reader in the middle of a record.
- **The backoff not resetting after a long-lived connection.** With it gone,
  reconnects still happen and still converge; what is lost is that a leader
  restarting once a day is reconnected to at 5s instead of 100ms. That the
  backoff *grows* is tested — `TestTheBackoffGrows`, by counting attempts in a
  window rather than asserting a latency — and that it comes back down is
  tuning, not correctness.

**A catch that is not reproducible is not a catch.** The mutation removing the
`Flush` after each frame was reported as caught by the two big replication tests
in one sweep and as surviving the next. Both reports were true: those tests
write hundreds of four-kilobyte values, which fill net/http's buffer and push
the frames out whether the code flushes or not, so whether they notice is a fact
about how much they happened to write. `TestOneSmallRecordArrivesAtOnce` writes
one nine-byte record to an idle, caught-up follower — nothing can fill a buffer
and nothing else is coming — and fails in 15 seconds flat without the flush. If
a mutation's verdict changes between runs, the test is measuring the wrong thing.

**A role is not a term, and the engine cannot tell you which of the two a node
is.** Fencing is about two leaders: a store that has heard of a newer term stops
taking writes. A node that is *following* has heard of no such thing — it holds
its leader's term, so `ErrorFenced` never fires — and it will take a write, put
it in its own log, and go on applying the leader's records around it. Nothing
errors, no checksum is wrong, and the two stores disagree for ever. `role.go`
exists for that one failure. The check has to be in the server because the thing
that makes this node a replica is a goroutine in the server, and it has to be on
every route that stores something: `TestAReplicaRefusesEveryWrite` runs all
three, because a batch aimed at a replica is the same mistake as a `PUT` and a
longer one.

**Churn is not retention, and TotalAlloc measures the wrong one.** Two versions
of the snapshot test failed on this. Measuring a follower catching up reported
221 MB for a 32 MiB store — nearly all of it the follower *building* a store out
of the records, which is inherent. Measuring the leader's send path reported
40 MB, because the engine allocates every record as it reads it out of a frozen
log, whatever it writes them to. What the claim actually is — that the leader is
not *holding* the snapshot — is `HeapAlloc` after a collection, with the snapshot
produced and not yet sent. That reads 611 KB against 67 MB, which is the
difference stated plainly.

**A test that watches for a race will lose it.** The first version of
`TestAFollowerAppliesASnapshotAsItArrives` let the whole payload cross and polled
for the store to be emptied, racing "the last byte arrived" against "the store
was reset" — microseconds apart, and it passed against the buffering it was
written to catch. The sender stops half way and waits now. If a test's assertion
depends on which of two things happens first, arrange for one of them not to
happen yet.

**A measurement whose two arms agree is measuring the harness.** Semi-synchronous
replication was first timed with a curl loop: 1507ms waiting for a follower
against 1513ms not waiting, which reads as "it is free" and is really "a curl
process costs seven milliseconds and that is all this measured". The benchmark
says 8.3 µs against 215 µs. If two arms of a comparison land within half a
percent of each other, suspect the harness before believing the result.

**A wait that is never woken still returns the right answer.** The mutation that
stops anything waking a blocked write survived, because `await` times out and
counts again — so it answered correctly, five seconds late. Correctness and
promptness are two claims and a test that only makes the first one lets the
whole waking mechanism rot. `TestAWriteWaitsForAFollower` bounds how long it
took.

**A goroutine that outlives its handler is a race the tests cannot see.** The
heartbeat is a second goroutine holding the stream's `http.ResponseWriter`, and
a ResponseWriter may not be touched once its handler has returned. Left to stop
in its own time it writes into a response net/http is finishing. The fix is two
defers whose order is the whole of it — `close(served)` runs first, which ends
the watcher, which closes `until`, which the heartbeat selects on, and only then
does `beating.Wait()` let the handler return. **The race detector is the only
thing that found this**, which is why the sweep now runs `go test -race`: it
costs about a second a mutation and buys a class of bug no assertion covers.

**A fake leader has to flush.** A test server that writes frames and then holds
the connection open leaves all of them in net/http's buffer, so the follower
reads nothing and sits there. `TestAFollowerReadsPastAHeartbeat` failed against
correct code for exactly that, and the failure looks identical to the bug it was
written to catch. The real leader flushes every frame; a fake that does not is
testing the buffer.

**Slowing a writer is not the same as trickling bytes.** The first version of
`TestSilenceIsBytesAndNotFrames` slept once per `Write`, which is one long
silence — and silence is what the idle deadline is meant to end, so it made the
correct code fail and the bug pass. It sends the payload in flushed pieces now.
The version before that just made the store bigger, which on loopback crossed in
under the deadline and proved nothing at all.

**A test that hangs when it fails is worse than one that fails.** The token
test asks every route for a 401, and one of those routes is the replication
stream: with the check removed it does not answer, it starts streaming, and the
suite sat for the full timeout instead of naming the route that was open. That
request goes on a two-second context now. The same shape bit the write-deadline
test, where `defer wire.Close()` waited on a stream whose follower had not been
stopped yet — a defer runs before every `t.Cleanup`, so the ordering `serving()`
uses is the ordering to copy: store, listener, server, and the follower's own
cleanup last-registered so it runs first.

**"The record arrived" is not the same claim as "the stream stayed up."** A
stream cut by a write deadline is one the follower reconnects to, and everything
arrives either way, a little later. `TestAStreamTakesItsWriteDeadlineOff` counts
connections for that reason: one is the assertion, and two means the deadline
cut it and the reconnect hid it. Anything asserting that replication works has
to be asked which of the two it is actually testing.

**The shutdown order is four things now and only one of them is obvious.** Stop
taking requests, stop the follower, close the `Server`, close the store. The
`Server` step exists because a handler blocked on the queue is holding a request
open: close the store first and a write a moment from being acknowledged becomes
`ErrorClosed` for no reason but the order. The follower step exists because it
writes to the store without going through the handlers or the queue, so nothing
else orders it against the close, and its `Close` waits for the goroutine rather
than merely asking — a batch being applied when the stop arrives is a write, and
returning before it finished would leave the caller free to close the store
underneath it. `main.go` has no test of its own — the end-to-end curl run in
"Verifying a change" is what covers it.

**`Shutdown` waits for a stream forever, and the number is 10.05 seconds against
0.03.** `http.Server.Shutdown` closes the listeners and then waits for every
connection to go idle; it does not cancel a request's context. A replication
stream is a request that never finishes on its own, so a leader with one
follower attached spends the whole of `-shutdown-timeout` and then reports
`context deadline exceeded`. That is the measurement, taken with two binaries on
loopback and the hook removed. `Server.CloseStreams` ends the streams and
refuses new ones, and `litekvd` hands it to `RegisterOnShutdown`. Anything else
long-lived that gets added to `server` needs the same treatment; the
default is to hang.

**A stream answers everything it can before the first byte of the body.** After
a 200 there is no status left, so a failure can only end the stream and the
follower is left guessing. That is why the first snapshot is taken before
`WriteHeader` — a fenced leader is a 409 with the term on it, a closed store a
503 — and why the term check happens before that. Everything after the header
goes to the log at Debug and nowhere else.

**A leader learns it has been replaced from `Since` and not from `Follow`.**
`db.Since` writes the newer term down before reporting `ErrorFenced`; `db.Follow`
returns the same error and writes nothing. The endpoint stands in for that by
asking `Since` with the same position — which costs nothing, because a store
that refuses on the term refuses before it reads a record — and
`TestAFollowerWithANewerTermFencesTheLeader` holds it to it by writing to the
leader afterwards and expecting `ErrorFenced`. The real fix is one line in
`Follow`, in the engine, and belongs to whoever next has a reason to touch it.

**What a leader is fenced by is asked of it, never volunteered.** `Snapshot`
refuses when the store is fenced and `batch` does not, so a leader that has
heard of a newer term goes on streaming to a follower that has not. Nothing in
this piece can tell — there is no exported way to ask a store whether it is
fenced, only to try a write. Piece 5 needs one for `/v1/status` anyway.

**`utf8.Valid` is the encoding rule and nothing else may be.** `ndjson.go`
decides between a plain string field and a `_b64` one by asking whether the
bytes are valid UTF-8, because `encoding/json` does not refuse bytes that are
not — it substitutes U+FFFD, in both directions, and says nothing. A route that
let that happen would answer 200 having lost the caller's bytes, which is the
worst shape this kind of bug has. `TestTheReplacementCharacterIsNotAnEncoding`
demonstrates the loss with `encoding/json` first and then shows this rule not
making it, and skips itself if the standard library ever stops doing it. The
same check runs on the way in over the whole line, since a raw `0xff` inside a
JSON string is not JSON and the decoder would quietly repair it.

**A `bufio.Scanner` checks its maximum only when it grows.** `parseBatch` starts
its buffer at `min(64 KiB, max)` for that reason: a Scanner given a starting
buffer larger than its maximum token size never reaches the check and never
reports `ErrTooLong`, so the limit is not a limit. This was caught by a test that
asked for a 500-byte line under a 128-byte limit and got the line.

**A body cut short reads as a line that is not JSON.** `http.MaxBytesReader`
stops mid-line, the Scanner hands back the partial token, and blaming that line
is blaming the wrong thing — the answer a client can act on is 413 and not "line
12 is not JSON". `parseBatch` asks `lines.Err()` before reporting a parse
failure, which works because a Scanner sets its error on the same call that
returns the last partial token.

**The batch route's queue test is the same one-trick job as the PUT's.** There is
still no way from outside the package to see that a batch went through the
writer rather than straight to the store, so `TestBatchGoesThroughTheQueue`
closes the `Server` and asks for a 503 while the store is still open, exactly as
`TestClosingTheServerStopsWritesAndNotReads` does. Weakening either weakens the
only evidence that `writes` is in the path.

**`litekv.Batch` does not copy, and the parser is what has to know.** Every key
and value in a parsed batch is its own allocation — a string conversion or a
base64 decode — because the batch reads them when it is written rather than when
they are added. A decode buffer reused across the lines would store the last
line's bytes under every key in the batch and report nothing.
`TestEveryLineKeepsItsOwnBytes` writes two hundred lines whose values get shorter
as it goes, which is the shape that catches a shared buffer that is not cleared;
values of one length would not.

**A range holds the store's read lock for the whole gather, so nothing writes to
a socket inside the callback.** `scan.go` builds the whole answer in memory and
sends it afterwards. It is the same trade `keys.go` makes with `Read` instead of
`View` and for the same reason: a client that stopped reading would otherwise be
deciding when the store is allowed to rotate. It also means a failure part way
through is still a clean 503 or 500, since nothing has gone out —
`TestScanOfAClosedStore` checks the answer holds no part of a range.

**What `?limit=` bounds is smaller than it looks.** The engine gathers and sorts
every matching key before it yields the first one, so returning false from the
callback stops the record reads and not the walk. The limit bounds the memory
this handler holds and the values it copies; it does not bound the scan. Anybody
adding a cheaper range should read the k-way-merge note above rather than
tightening this.


## What was built, in six pieces

The server was built in six pieces, each verified and pushed on its own, back
when it lived inside the engine's repository. The table is kept because the
notes under it are about this code, and several are still the reason something
is shaped the way it is:

| # | piece                                    | state |
| - | ---------------------------------------- | ----- |
| 1 | the package, the binary, and one key     | done  |
| 2 | group commit under the handlers          | done  |
| 3 | several at once, and ranges              | done  |
| 4 | replication over the wire                | done  |
| 5 | two roles, and reads that are not stale  | done  |
| 6 | operations, and writing it down          | done  |

### What to build next

**A lease, and a way down** is the gap worth naming clearly, because it is the
only one where the current behaviour loses acknowledged writes. A replaced
leader finds out it was replaced when something carrying a newer term asks it
for records, and until then it goes on taking writes that are lost the moment it
finds out. There is also no way down at all: the route table has
`POST /v1/promote` and nothing opposite it, so a node that should hand over has
to be killed.

It is two commits in two repositories. The engine needs a `DB.Demote` — there is
no way to lower a store back to following, and inventing one up here by writing
a term into the state file would be exactly the reaching-past-the-API this
repository exists to prevent. This side needs a `POST /v1/demote` and an
optional lease loop, where a leader that cannot renew stops taking writes on its
own. That bounds the window at the lease TTL less the clock skew rather than
closing it, which is a much smaller number than "until a follower turns up" and
is still a number. It is the external-lease arrangement the engine's `AGENTS.md`
argues for under "Consensus, and why it is not on that list".

**Ranges that stream and page** is an engine change first: a k-way merge over
the per-log sorted keys instead of a gather, plus a cursor a client can resume
from. Nothing here can fix `-max-scan` on its own — the cap exists because the
gather holds a read lock, and that is below this layer.

Piece 4 was the one the rest of the engine was waiting on, and it is done: there
is now a connection for a leader to hang something off, which is what
semi-synchronous replication needs and what nothing here had before. What it
does not yet have is anything hanging off it — the handler keeps no list of
followers and no record of how far each has got, and adding one is the first
state a leader would have to keep. See "What piece 4 left for the pieces after
it" below.

### What piece 4 left for the pieces after it

**Roles are the missing half and the endpoint is written as if they existed.** A
node started with `-leader` still serves `PUT` and `DELETE`, and a write to it
goes into its own active log while `applied` stays where the leader put it. The
next batch is accepted — `Apply` compares `from` against `applied`, and
`applied` did not move — so the leader's records land on top of the local ones
and the two stores disagree with nothing reporting it. `litekvd` warned at
startup and that was all it did. Piece 5 closed it — `role.go`, and a check on
every route that stores anything — and this paragraph is kept because the shape
of the hole is worth knowing: it is what a store does when nothing tells it
which of the two it is.

**A leader keeps no list of followers, on purpose and only for now.** The
handler holds one connection and knows nothing about any other. Semi-synchronous
replication needs the opposite: the leader has to know who is connected and how
far each has got, and a write has to wait for some of them. The place for that
is the `send` closure in `streamReplica`, which already sees every position that
goes out and is the only code that knows a follower took it. What it does not
see is acknowledgement — nothing comes back up this stream, and there is no
frame kind for it. That is the first protocol change semi-sync needs, and it is
also what a heartbeat would need, which is the other thing missing: a stream
over a blackholed TCP connection is noticed by the OS keepalive in about fifteen
minutes and by nothing else.

**Two engine gaps this piece worked around were fixed in the engine, not around
here.** `Follow` writes down a newer term where before only `Since` did, and
`DB.Fenced` is exported.

The first was a real bug and not a tidiness complaint: a leader with a follower
attached — the ordinary arrangement, and the one this server creates — went on
taking writes after being superseded, because only the polling path recorded the
newer term. It was found from this side and fixed in the engine, where
`TestFollowFencesALeaderTheWaySinceDoes` now runs both calls through the same
assertions. That is the shape most bugs of this kind will have: the caller
notices, the invariant belongs to the store, and the fix is a commit and a tag
over there rather than a workaround in here.

The handler still asks `Since` before it starts a stream, and that is no longer
standing in for anything. Everything answerable with a status has to be answered
before the first byte of the body, and a follower told 409 knows it is pointed at
a store that has been replaced, where a stream that opened and then died says
only that a connection ended.

**The leader no longer holds a whole snapshot in memory, and neither does the
follower.** It spools to a file and copies the file to the connection; the
follower hands the payload to the store as a reader. The framing did not have to
change at all — what changed is where the length comes from.

**A file rather than the socket, and the reason is the engine's contract.**
`DB.Snapshot` holds `mergeMu` for the whole of its call, so whatever it writes to
decides how long merging is paused on that leader. A buffer is fast and costs the
store in memory; the socket costs nothing in memory and pauses merging for the
length of the transfer, which on a slow link is minutes of a leader that cannot
compact while it is still taking writes. Anyone tempted to "simplify" this by
handing the socket straight to `Snapshot` should read that sentence twice.

**It cost the follower something and the cost is in a test.** `ApplySnapshot`
resets the store before it reads, and it now reads from the wire, so a follower
is emptied at the *start* of a snapshot rather than at the end and a torn
transfer leaves it holding nothing. `TestAFollowerAppliesASnapshotAsItArrives`
asserts exactly that, which is the same fact as "it streams" seen from the other
side.

**`MaxFrame` stopped being a memory bound.** It was a gigabyte and it was the
largest store that could be replicated at all. It is a terabyte now and it is a
sanity bound on a number a stranger sent: `readPayload` grows into a payload as
the bytes arrive, so a header claiming more than it sends costs nothing.


## Semi-synchronous replication, in full

**Semi-synchronous replication** is done, and it needed all three of the things
that note said it did: a leader that knows its followers, a follower that says
how far it has got, and a write that waits. `acks.go` is the registry and the
wait; the follower's half is in `follower.go`.

The acknowledgement is `POST /v1/replica/ack` and not something coming back up
the stream, because a response body only goes one way and the alternative is
full-duplex HTTP/1.1 — which is the thing proxies break, and riding one listener
through whatever proxy a replica sits behind was the reason for choosing HTTP at
all.

**What it cannot do is take a write back**, and that is not a shortcut. The
record is in the log before anything waits — there is nothing to replicate until
it is written — so a wait that runs out is reported and never undone: 202 rather
than 204, with `Litekv-Replicated` saying how many followers had it. A client
that retries on 202 writes the record twice. Say so to anyone who asks for
"synchronous replication" here; what is on offer is a 204 that means a failover
will not lose the write, and nothing stronger.

**An ack only counts from a follower this leader is streaming to.** An ack is a
claim, and what makes it worth anything is that this leader is the one sending
that follower records. Taking one from anybody would let whatever can reach the
route satisfy a semi-synchronous write by asserting it had the data — the
guarantee, given away to a caller that guessed a URL.

It costs 8.3 µs against 215 µs for a 128-byte write on loopback, and all of the
difference is the network. See `BenchmarkSemiSynchronousWrite`, and read its
comment before writing another benchmark here: the first measurement of this was
a curl loop that reported 1507ms waiting against 1513ms not, because a curl
process costs about seven milliseconds to start and that was the entire number.
