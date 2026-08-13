<p align="center">
  <img src="docs/logo.jpg" alt="litekvd" width="420">
</p>

# litekvd

**litekvd serves a key-value store over HTTP.** It is one binary with no dependencies
outside the Go standard library, and it runs without configuration. Storage is
[litekv](https://github.com/tillknuesting/litekv), a Bitcask-style log-structured engine:
writes append to a log, a read is one index lookup and one read, and every key is held in
memory while values stay on the disk.

It has replication with leader and replica roles, optional semi-synchronous writes, reads
that can refuse a replica that has not caught up, and Prometheus metrics.

[![Go](https://github.com/tillknuesting/litekvd/actions/workflows/go.yml/badge.svg)](https://github.com/tillknuesting/litekvd/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tillknuesting/litekvd.svg)](https://pkg.go.dev/github.com/tillknuesting/litekvd)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

```bash
go install github.com/tillknuesting/litekvd@latest
litekvd
```

```bash
curl -X PUT --data-binary 'ada' http://127.0.0.1:8080/v1/keys/user:1
curl http://127.0.0.1:8080/v1/keys/user:1
# ada
```

That is the whole of getting started. No config file, no `initdb`, no daemon user, no
port to choose: the store goes in `./litekv-data`, it listens on `127.0.0.1:8080`, and
every write is on the disk before it is acknowledged.

|                     |                                                                 |
| ------------------- | ---------------------------------------------------------------- |
| **Data model**      | one flat keyspace; keys and values are arbitrary bytes            |
| **Durability**      | every write synced to the disk before it is acknowledged, by default |
| **Memory**          | every key held in memory, about 59 bytes each; values on the disk |
| **Not provided**    | transactions across keys, secondary indexes, a query language, automatic failover |
| **Build and run**   | Go 1.26 to build; no runtime dependencies                        |
| **Stability**       | v1: the HTTP API and `litekvd/server` do not break without a `/v2` |
| **Measured**        | 779 ns per write through the handler at ten concurrent writers, `-sync always`; 215 µs when waiting for a follower |

**Contents** — [Install](#install) · [Sixty seconds](#sixty-seconds) ·
[What it is](#what-it-is) · [Configuration](#configuration) · [The API](#the-api) ·
[Replication](#replication) · [Operations](#operations) ·
[Performance](#performance) · [Limitations](#limitations)

## Install

**With Go** — the only thing you need, and it builds from source:

```bash
go install github.com/tillknuesting/litekvd@latest
```

The binary lands in `$(go env GOPATH)/bin`. Add that to your `PATH` if it is not there.

**From a checkout**, which is the same thing with a name you choose:

```bash
git clone https://github.com/tillknuesting/litekvd && cd litekvd
go build -o litekvd .
```

There is nothing to compile against, nothing to link, and no C. The module depends on the
storage engine and on the standard library, and on nothing else at all.

## Sixty seconds

Start it, put some records in, read them back, and look at what it is doing:

```bash
litekvd &

# one at a time
curl -X PUT --data-binary 'ada'   http://127.0.0.1:8080/v1/keys/user:1
curl -X PUT --data-binary 'grace' http://127.0.0.1:8080/v1/keys/user:2
curl http://127.0.0.1:8080/v1/keys/user:1          # ada

# several at once, all of them or none
curl -X POST --data-binary @- http://127.0.0.1:8080/v1/batch <<'EOF'
{"op":"write","key":"user:3","value":"katherine"}
{"op":"write","key":"session:x","value":"...","expires":"2030-01-01T00:00:00Z"}
{"op":"delete","key":"user:1"}
EOF

# a range, in key order, as one JSON object per line
curl 'http://127.0.0.1:8080/v1/keys?prefix=user:'
# {"key":"user:2","value":"grace"}
# {"key":"user:3","value":"katherine"}

curl http://127.0.0.1:8080/v1/status   # {"role":"leader","term":0,...}
curl http://127.0.0.1:8080/health      # ok
```

Stop it with Ctrl-C. Start it again in the same directory and everything is still there.

## What it is

A key is arbitrary bytes. A value is arbitrary bytes. Writes go to the end of a log and never
seek, reads are one index lookup and one read, and a crash costs at most the record that was
being written. Every key has to fit in memory; the values do not.

- **One key at a time** — `GET`, `HEAD`, `PUT`, `DELETE`, with per-record expiry.
- **Several at once** — a batch is atomic and durable: all of it or none of it, on the disk
  and on the wire.
- **Ranges and prefixes**, answered in key order as newline-delimited JSON.
- **Replication** — a second node follows the first over the same port, catches up from a
  snapshot when it has fallen too far behind, and reconnects on its own.
- **Semi-synchronous replication** — hold a write until a follower has it, so a failover
  cannot lose an acknowledged write.
- **Reads that are not stale** — a client can refuse a replica that has not caught up to a
  write it already knows about.
- **Operations** — `/health`, Prometheus `/metrics`, structured logs, a shared bearer token,
  and a shutdown that does not drop requests in flight.

What it is **not**: there are no transactions, no secondary indexes, no query language, and no
automatic failover. See [Limitations](#limitations), which is written to be read before you
choose this rather than after.

## Configuration

Every default is one you could leave in place. The two that change when the same binary runs
somewhere else also read an environment variable.

| flag                | what it is                                                     | default            |
| ------------------- | -------------------------------------------------------------- | ------------------ |
| `-dir`              | the directory holding the store, created if missing            | `./litekv-data` (`LITEKV_DIR`) |
| `-addr`             | the address to listen on                                       | `127.0.0.1:8080` (`LITEKV_ADDR`) |
| `-sync`             | `always`, `every` or `never`                                   | `always`           |
| `-sync-interval`    | how often to sync under `-sync every`                          | 1s                 |
| `-segment-size`     | bytes before a log is frozen                                   | 4 MiB              |
| `-merge-trigger`    | logs of a size before they are merged                          | 2                  |
| `-max-value`        | the largest value a write may carry                            | 16 MiB             |
| `-max-batch`        | the largest body `POST /v1/batch` will take                    | 32 MiB             |
| `-max-scan`         | most pairs a range answers with, and the most `?limit=` may ask | 1000               |
| `-queue`            | writes that may be waiting before a handler blocks             | 1024               |
| `-leader`           | base URL of a leader to follow                                 | follow nobody      |
| `-token-file`       | file holding a shared bearer token                             | no authentication  |
| `-spool-dir`        | where a snapshot is written on its way to a follower           | system temp        |
| `-wait-for`         | followers that must have a write before it is acknowledged     | 0 (asynchronous)   |
| `-wait-timeout`     | how long a write waits for them before answering 202           | 5s                 |
| `-heartbeat`        | how often an idle leader says it is still there                | 10s                |
| `-idle`             | how long a follower waits to hear it before reconnecting       | 30s                |
| `-read-timeout`     | how long a request has to arrive, headers and body             | 60s                |
| `-write-timeout`    | how long a response has to be written (streams exempt)         | 60s                |
| `-idle-timeout`     | how long an idle keep-alive connection is held                 | 120s               |
| `-shutdown-timeout` | how long requests in flight get once it is asked to stop       | 10s                |

`-sync` defaults to `always` because the engine does: a binary that quietly weakened durability
relative to the code it wraps would be the wrong kind of convenient. `-sync every` is the usual
trade and the one to reach for — it batches the disk waits and costs you a window of writes if
the machine dies rather than the process.

It listens on **loopback** unless told otherwise. There is no authentication and no TLS unless
you set `-token-file`, so put it behind a proxy or on a private network before giving it an
address a stranger can reach.

**One process owns a directory and the store enforces it.** A second `litekvd` on the same
`-dir` fails to start with `store directory is open in another process` rather than writing
over the first one's log. The claim is a lock held for as long as the process lives, so a
machine that lost power comes back and starts normally — there is nothing to clean up by hand.

### Running it as a service

Copy-paste, on the machine that will run it. It builds the binary, makes a user for it,
writes a unit and starts it. Read it before you run it — it does need root, and it writes
to `/usr/local/bin` and `/etc/systemd/system`.

```bash
# 1. Build and install the binary.
go install github.com/tillknuesting/litekvd@latest
sudo install -m 0755 "$(go env GOPATH)/bin/litekvd" /usr/local/bin/litekvd

#    No Go on the target? Build it somewhere that has one and copy it over.
#    Nothing is linked, so the binary is the whole of it:
#      GOOS=linux GOARCH=arm64 go build -o litekvd-arm64 github.com/tillknuesting/litekvd
#      scp litekvd-arm64 pi:/tmp/ && ssh pi 'sudo install -m 0755 /tmp/litekvd-arm64 /usr/local/bin/litekvd'

# 2. A user that owns the data and can do nothing else. --user-group so that
#    the Group= below is certain to exist; /usr/sbin/nologin is the Debian path.
sudo useradd --system --user-group --no-create-home \
    --shell /usr/sbin/nologin litekv || true

# 3. The unit. systemd creates and owns /var/lib/litekv through StateDirectory.
sudo tee /etc/systemd/system/litekvd.service >/dev/null <<'UNIT'
[Unit]
Description=litekvd
Documentation=https://github.com/tillknuesting/litekvd
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/litekvd \
    -dir /var/lib/litekv \
    -spool-dir /var/lib/litekv \
    -addr 127.0.0.1:8080 \
    -sync every
User=litekv
Group=litekv
StateDirectory=litekv
StateDirectoryMode=0700

Restart=always
RestartSec=1s

# It needs one directory and a socket. Everything else can be taken away.
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
LockPersonality=true

[Install]
WantedBy=multi-user.target
UNIT

# 4. Start it, and check.
sudo systemctl daemon-reload
sudo systemctl enable --now litekvd
systemctl status litekvd --no-pager
curl -sS http://127.0.0.1:8080/health && echo
```

Four choices in there are worth knowing about rather than inheriting.

**`-spool-dir /var/lib/litekv`, not the default.** A snapshot on its way to a follower is
spooled to a file first, and the default is the system temporary directory — which under
`PrivateTmp=true` is a tmpfs, and therefore memory. Pointing it at the data directory is
what keeps a snapshot of a large store off the heap. It costs nothing if you never
replicate.

**`-sync every`, not the default `always`.** `always` waits for the disk on every write,
which is what you get running the binary by hand; `every` batches those waits into one a
second, and is the usual trade for a service. What it costs is the writes of the last
second if the machine loses power — not if the process dies, which is a different and much
more common failure. Take the `-sync every` line out if that second matters.

**`127.0.0.1`.** There is no TLS and no authentication unless you add `-token-file`. Putting
it on `0.0.0.0` hands the whole database to anything that can reach the port.

**`AF_NETLINK` in that allow-list, which looks like it does not belong.** Go's resolver
reaches for netlink on Linux to work out whether the machine has usable IPv6, and a service
that follows a `-leader` by hostname has to resolve one. Taking it out is the kind of change
that works on the machine you test it on and fails on the one that has to resolve something.

**`StateDirectory` rather than a `mkdir`.** systemd creates `/var/lib/litekv` owned by the
service user, recreates it if it is removed, and `ProtectSystem=strict` makes it the only
writable path the process has. `StateDirectoryMode=0700` keeps the store readable only by
that user, which matters because the values are in those files in the clear.

To watch it, change it, or take it away:

```bash
journalctl -u litekvd -f                     # its log
sudo systemctl edit litekvd                  # override the flags without touching the unit
sudo systemctl disable --now litekvd         # stop it and leave the data
sudo rm /etc/systemd/system/litekvd.service  # and remove it; /var/lib/litekv stays
```

`systemctl edit` is the one worth remembering: it writes a drop-in that survives reinstalling
the unit, so the flags you chose are not lost the next time you paste the block above.

`SIGTERM` is a clean shutdown: it stops taking requests, lets the ones in flight finish, closes
the writer so that everything acknowledged is on the disk, and then closes the store. `SIGKILL`
costs you at most the record being written, and the lock goes with the process either way.

## The API

| method   | route                      | what it does                             | answers        |
| -------- | -------------------------- | ---------------------------------------- | -------------- |
| `GET`    | `/v1/keys/{key}`           | the value, as the body                   | 200, 404       |
| `HEAD`   | `/v1/keys/{key}`           | the value's length and nothing else      | 200, 404       |
| `PUT`    | `/v1/keys/{key}`           | stores the body under the key            | 204            |
| `DELETE` | `/v1/keys/{key}`           | writes a tombstone                       | 204            |
| `GET`    | `/v1/keys`                 | a range or a prefix, as NDJSON pairs     | 200, 400       |
| `POST`   | `/v1/batch`                | several records, all of them or none     | 204, 400, 413  |
| `GET`    | `/v1/status`               | which of the two this node is            | 200            |
| `POST`   | `/v1/promote`              | stop following and raise the term        | 200            |
| `GET`    | `/v1/replica/stream?from=` | the records after a position, streamed   | 200, 400, 409  |
| `POST`   | `/v1/replica/ack`          | a follower saying how far it has got     | 204, 400, 409  |
| `GET`    | `/health`                  | whether this node can serve              | 200, 503       |
| `GET`    | `/metrics`                 | Prometheus text                          | 200            |

A value is the body, raw. There is no JSON envelope around your bytes and nothing is base64 on
the way through, because a key-value store's whole job is to hand back what it was given; the
type is `application/octet-stream` and the server has no opinion about what is in it. An empty
value is a value, and a `Content-Length` of `0` is what tells it apart from a missing key.

`PUT` takes a `Litekv-Expires` header holding an RFC 3339 time, and writes a record that stops
answering once that instant has passed. It is an instant and not a duration for the same reason
the store's expiry is: a duration has to be resolved against somebody's clock, and the only
clock a client and a server agree on is the one they both write down. A client thinking in TTLs
subtracts.

`DELETE` of a key that was never there answers 204, not 404. The store cannot answer that
question anyway — a delete appends a tombstone without looking for what it hides — so a 404
would be a lie dressed as a check.

### Spelling a key in a URL

A key is arbitrary bytes and a URL is not. Percent-encoding a path segment carries all of them:
Go's `ServeMux` unescapes segment by segment and a `%2F` is deliberately *not* a separator, so a
key holding slashes, spaces, control bytes, or sequences that are not UTF-8 at all survives the
trip unchanged.

```bash
curl -X PUT --data-binary 'nested' http://127.0.0.1:8080/v1/keys/a%2Fb%2Fc
curl http://127.0.0.1:8080/v1/keys/a%2Fb%2Fc      # nested
```

`TestKeyOfAnyBytes` puts thirteen awkward keys through a real socket and a real client rather
than trusting the documentation for any of that, and checks the store holds the bytes the caller
meant rather than the ones the URL was spelled with — reading it back through the same encoding
would agree with itself however wrong both ends were.

The one key with no spelling *here* is the empty one, which the store holds happily. A path
wildcard does not match an empty segment, so `/v1/keys/` is not a route. It is reachable through
the routes that do not spell a key in a path — a batch line writes it and a range hands it back
— but not through this one.

### Several at once

Two routes carry more than one record, and both carry it as **newline-delimited JSON**: one
object to a line, no array around them, so a body can be produced and consumed a line at a time
and neither end has to hold a large answer as a single JSON value before it can look at any
of it.

`POST /v1/batch` stores every operation or none of them, and answers 204. `"op"` is `"write"`
or `"delete"`, `"expires"` is an RFC 3339 time meaning exactly what the `Litekv-Expires` header
means on a `PUT`, and a delete carrying a value or an expiry is refused rather than having them
quietly dropped. An absent key or value is the empty one, a blank line is skipped, and an empty
body stores nothing and answers 204.

All or nothing means two things and the route provides both. The engine provides the second:
`WriteBatch` puts the records down behind a marker and recovery discards from that marker on
unless every one of them is there. The route provides the first: the **whole body is parsed**
before any of it is handed to the store, so one line the server cannot read refuses the whole
request with a 400 naming that line. A parser that stored as it went would make the marker
pointless — atomic on the disk and torn on the wire.

### A key is bytes and a JSON string is not

Both routes use one encoding rule, in both directions:

- A key or a value is a plain string field — `"key"`, `"value"` — when it is **valid UTF-8**.
- It is a separate base64url field — `"key_b64"`, `"value_b64"` — when it is not. That is
  `base64.RawURLEncoding`: the alphabet of RFC 4648 §5 and **no padding**, since padding
  carries nothing and one spelling of a field is easier to be right about than two.
- Which one is decided by `utf8.Valid` and by nothing else. `encoding/json` replaces a byte
  that is not UTF-8 with U+FFFD rather than refusing it, in both directions, and a store that
  hands back a replacement character where its caller wrote `0xff` has lost that caller's data
  while answering 200.
- Sending both forms of one field is an error rather than something to resolve, and so is a raw
  byte that is not UTF-8 anywhere in a line — that is what the `_b64` fields are for.

The plain form is the ordinary one and is meant to be: keys people actually have are text, and a
body of them should be readable in a terminal without anything being decoded first.

### Ranges

`GET /v1/keys` takes `?prefix=` or `?from=`&`?to=`, with `from` included and `to` excluded, and
answers the matching pairs in key order. Both bounds and the prefix are percent-decoded exactly
as a key in a path is, so they carry any byte a key can hold.

| the request                      | what it means                                                   |
| -------------------------------- | ---------------------------------------------------------------- |
| no parameters at all             | every key, capped by `-max-scan`                                 |
| `?prefix=` with nothing after it | the same thing: an empty prefix is every key                     |
| `?from=` or `?to=` empty         | no bound on that side                                            |
| `prefix` with `from` or `to`     | 400. They are two ways of naming one range, not two to intersect |
| a `from` after its `to`          | an empty range, which is 200 and no lines                        |
| nothing matched                  | 200 and no lines. There is no key here to be missing, so no 404  |
| `?limit=` empty, zero, negative  | 400. A client that built the query wrongly should hear about it  |
| `?limit=` over `-max-scan`       | 400, naming the maximum                                          |

The limit is refused rather than quietly lowered because counting the lines against the limit it
asked for is the only way a client can tell that an answer was cut short. Paging is that plus one
byte: `from` is inclusive, so the next page starts at the last key with a `%00` after it.

**What the limit does not buy is a cheap range.** A range is gathered and not streamed — every
log has to be asked before the first key can be yielded in order, and the store's read lock is
held for all of it — so stopping at the limit does not stop the walk that found the keys. What it
stops is reading the records: the value copies, and for a frozen log the system calls that fetch
them, which is most of the cost of a large answer but not the search. A range that has to be
cheap has to be narrow. `-max-scan` is there because rotation and merging want the write lock and
would otherwise queue behind whoever is scanning; it is the cap a client cannot raise.

### What a failure says

A failure is a status and a JSON body of one field: `{"error":"key not found"}`. A client that
wants to branch on what went wrong branches on the status, and the sentence is for whoever is
reading a terminal.

| status | when                                                                          |
| ------ | ----------------------------------------------------------------------------- |
| 400    | an expiry, a batch line, a range query, or a `from` the server could not read  |
| 404    | no value under that key — never written, deleted, or expired                   |
| 405    | a method that route does not have, with an `Allow` header saying which it does |
| 409    | the store has been fenced, with `Litekv-Term` carrying the term it is on       |
| 409    | a write aimed at a replica, with `Litekv-Leader` saying where it should go     |
| 412    | a read carrying `Litekv-After` from a store that has not got there             |
| 413    | a value over `-max-value`, or a batch over `-max-batch`                        |
| 503    | the store is closed, which is what a server on its way down looks like         |
| 504    | a `Litekv-After` read whose `Litekv-Wait` ran out                              |
| 500    | anything else                                                                  |

The three ways there can be no value under a key are one status on purpose. The store tells them
apart because it knows whether the key was asked to go, told to go by itself, or was never there,
and none of those change what a caller does next.

A 500 says "internal error" and nothing more. An error from the store can name a path on the
server's disk or an offset in a log, and a stranger has no business with either; it goes to the
log instead.

## Replication

### What it is for

A replica is a second machine holding the same records, kept up to date as they are written.
It buys three things, and they are worth separating because you may want only one of them and
the cost of each is different.

**A live second copy, for when a machine goes away.** A disk that fails or a host that does
not come back takes the store with it. A replica means the data is also somewhere else,
seconds behind rather than however old your last copy is. This is the reason most people
want it and it is the one that needs no extra flags.

**Reads served from somewhere else.** Replicas answer reads, so read-heavy work can be spread
across them while writes go to one node. What this costs is that a replica can be behind: a
read there may not show a write that has already been acknowledged. The `Litekv-After` header
below is the fix — a client that just wrote can insist on a replica that has caught up, and
get 412 instead of a stale answer.

**A failover that takes seconds rather than an evening.** If the leader is gone and there is a
replica, `POST /v1/promote` makes it the leader and the application is pointed at it. Without
one, recovery means finding a copy of the data and restoring it. Note what this is not:
nothing here *decides* that a failover should happen. A person or an external lease does; see
[Limitations](#limitations).

### When it is not worth it

**On one machine it buys nothing.** A replica in another directory on the same host survives
neither the disk nor the host, which are the two failures it exists for. It is useful for
trying the mechanics out, and that is all.

**If restoring from a copy is fast enough, this is more moving parts than you need.** Two
processes, two directories, and a decision to make when one of them dies.

**It is not a backup, and this is the important one.** A replica applies whatever the leader
did, immediately — including a `DELETE` of the wrong key, or a batch that wiped something.
There is no delay and no undo, so a replica protects you from a machine and not from
yourself. A backup here is stopping a node and copying its directory (running a replica gives
you a node you can stop without downtime). There is no backup command; see
[Limitations](#limitations).

### What it costs

| what                        | cost                                                              |
| --------------------------- | ------------------------------------------------------------------ |
| a replica attached          | one copy of each batch out of the leader's memory as it is sent; ten replicas is ten copies |
| a replica catching up       | a snapshot of the live records, spooled to `-spool-dir` on the leader |
| `-wait-for 1`               | a write goes from 8.3 µs to 215 µs — a round trip to the follower and back |
| a stale read you cannot have | 412, and the client asks again or asks the leader                 |

Asynchronous replication is the default and costs a write nothing. `-wait-for` is what you add
when losing an acknowledged write in a failover is worse than the latency, and it is a real
trade rather than a setting to turn on for safety's sake.

### Setting one up

A follower is a second `litekvd` pointed at the first:

```bash
litekvd -dir /var/lib/litekv  -addr 127.0.0.1:8080                                   # leader
litekvd -dir /var/lib/replica -addr 127.0.0.1:8081 -leader http://127.0.0.1:8080     # replica
```

That is all of it. The replica catches up from wherever it was, takes a snapshot of the whole
store if it has fallen too far behind for the leader to still have the records, and reconnects on
its own when the connection ends.

**One listener and not two.** One port to open, one thing to shut down, one place for
authentication to go, and it goes through whatever proxy or load balancer a read replica is
already behind. A second raw TCP listener would have needed every one of those again.

**A connection ending is normal.** The follower reconnects with a backoff that doubles from
100 ms to 5 s, half of each wait jittered so that several followers that lost the same leader do
not all come back at the same instant. A connection that stayed up longer than the longest wait
was a working one, so the next attempt starts from the shortest wait again.

Stopping a follower does not cost it its place. The position is written down beside the
follower's own logs, so one that comes back reads it out of the store and resumes.

### Leader and replica

A node started with `-leader` is a **replica**. It refuses every write with 409 and a
`Litekv-Leader` header saying where to send it — `PUT`, `DELETE` and `POST /v1/batch` alike —
and it goes on answering reads, which is what it is for.

That refusal is not fencing and could not be. A store that is following holds its leader's term,
so the engine's own fencing never fires, and it will take a write perfectly happily: the record
goes into its own log, the leader's records keep arriving around it, and the two histories never
reconcile. No checksum is wrong and nothing errors. It is the quietest way to lose data this
design has, and the only thing that prevents it is this server knowing which of the two it is.

```bash
curl -X POST http://127.0.0.1:8081/v1/promote      # {"term":1}
curl http://127.0.0.1:8081/v1/status
# {"role":"leader","term":1,"position":"...","segments":3,"keys":812}
```

`POST /v1/promote` stops the following first and raises the term second, and the order is the
point: a term raised while records are still arriving is a store that has fenced its own leader
and then applies another of its batches.

**What promotion does not do is decide that this node should be the leader.** That is consensus
and it is not here. Raising the term in two places at once puts two stores on the same term and
gives the guarantee away, so whatever decides has to be the only thing deciding — an external
lease, or a person. See [Limitations](#limitations).

### Reads that are not stale

Every write answers with `Litekv-Position`, an opaque cookie for where the store had got to.
Send it back as `Litekv-After` on a later read and a node that has not got there refuses rather
than answering with what it has:

```bash
POS=$(curl -si -X PUT --data-binary 'ada' http://127.0.0.1:8080/v1/keys/user:1 \
      | grep -i '^litekv-position:' | cut -d' ' -f2 | tr -d '\r')

curl -H "Litekv-After: $POS" -H "Litekv-Wait: 2s" http://127.0.0.1:8081/v1/keys/user:1
```

Without `Litekv-Wait` a replica that is behind answers 412 at once, with its own position in
`Litekv-Position` so a client can decide whether to wait here or go elsewhere. With it, the read
waits and answers 504 if the time runs out — which says the wait was too short, not that the
records are never coming.

This is read-your-writes across a load balancer. It hides the replication lag from the client
that just wrote; it does not remove it.

### Semi-synchronous replication

Replication is asynchronous by default: a write returns as soon as the leader has it, so a leader
that dies loses whatever its followers had not received. `-wait-for` is the answer to exactly
that — a write is not acknowledged until that many followers have it.

```bash
litekvd -addr 0.0.0.0:8080 -wait-for 1 -wait-timeout 5s
```

**What it cannot do is take a write back.** The record is in the leader's log before anything
waits; it has to be, since there is nothing to replicate until it is written, and nothing here
can unwrite it. So a wait that runs out is *reported* and never undone:

| status | means                                                                          |
| ------ | ------------------------------------------------------------------------------ |
| 204    | stored, and `Litekv-Replicated` followers had it — a failover will not lose it  |
| 202    | stored, and fewer than `-wait-for` had it before the wait ran out               |

A client that reads 202 as a failure and retries will write the record twice. That is the honest
shape of semi-synchronous replication and not a shortcut in this one; MySQL degrades to
asynchronous after its timeout for the same reason.

An acknowledgement only counts from a follower this leader is streaming to. An ack is a claim,
and what makes it worth anything is that this leader is the one sending that follower records —
otherwise anything that could reach `/v1/replica/ack` could satisfy a semi-synchronous write by
asserting it had the data.

What it costs, with a real follower on a real listener:

| `-wait-for` | a 128-byte write |
| ----------- | ---------------- |
| unset       | 8.3 µs           |
| 1           | 215 µs           |

Twenty-six times, and all of it is the network: the leader writes the frame, the follower applies
it and posts an acknowledgement, and the leader wakes. On loopback that is 200 µs; between two
machines it is whatever two round trips cost there. It is the price of the guarantee and there is
no version of this that is cheaper.

### A quiet stream and a dead one

They look alike from the follower's end, and that is the problem. A TCP connection that has been
blackholed rather than closed — a cable pulled, a firewall dropping instead of refusing, a leader
that lost power — delivers nothing and reports nothing, and the OS keepalive notices in about
fifteen minutes.

So a leader with nothing to send says so: a heartbeat every `-heartbeat` (10s), carrying the
leader's own position so a follower can see how far behind it is while nothing is being written.
A follower that hears nothing for `-idle` (30s, three beats) drops the connection and dials
again, which costs it nothing.

**The deadline is on silence, not on a frame.** A snapshot of a large store is one frame that
takes as long as it takes, and a deadline that only moved when a frame completed would cancel the
transfer it was in the middle of — turning a slow first sync into a loop that never finishes one.

### Snapshots, and where they land

A snapshot is the whole live store. Neither end holds one in memory: the leader writes it to a
file and copies the file to the connection, and the follower hands the payload straight to the
store as a reader. Replicating a store used to cost that store twice over in RAM, in a database
whose whole premise is that only the keys have to fit.

**`-spool-dir` is worth setting.** The default is the system temporary directory, and on most
Linux systems `/tmp` is a tmpfs — which puts the whole live store back in memory and undoes the
point. The store's own directory is usually right: it is sized for the data and it is the same
filesystem. The file is unlinked the moment it is created, so nothing has to remember to remove
it — not a panic, not a killed process, not a follower that hangs up half way.

**What this cost.** A follower is emptied at the *start* of a snapshot rather than at the end,
because the store resets before it reads and it is now reading from the wire. A transfer that
breaks half way leaves that follower holding nothing until it reconnects and takes another.

## Operations

`GET /health` answers 200 while the store can serve and 503 once it is closing, and it asks the
store the cheapest question that touches its state — no disk. **It is the one route a token does
not cover**: a load balancer probing this node is not a client and has no business holding the
secret that opens the database.

### Prometheus

`GET /metrics` is Prometheus text exposition, version 0.0.4, served by the same listener as
everything else. There is no exporter to run beside it and nothing to configure:

```yaml
scrape_configs:
  - job_name: litekvd
    static_configs:
      - targets: ["127.0.0.1:8080"]
```

If `-token-file` is set, `/metrics` needs it like every other route except `/health`:

```yaml
    authorization:
      type: Bearer
      credentials_file: /etc/litekv/token
```

#### What it exposes

| metric                             | type      | labels                  | what it is                                          |
| ---------------------------------- | --------- | ----------------------- | ---------------------------------------------------- |
| `litekv_requests_total`            | counter   | `route`, `method`, `status` | requests **answered**                            |
| `litekv_request_duration_seconds`  | histogram | `route`                 | how long a route took; buckets 100 µs to 5 s         |
| `litekv_role`                      | gauge     | `role`, `leader`        | always `1`, on the one series for what this node is  |
| `litekv_term`                      | gauge     | —                       | the leader generation this store is on               |
| `litekv_fenced`                    | gauge     | —                       | `1` once this store has heard of a newer leader      |
| `litekv_replication_streams`       | gauge     | —                       | replication streams open right now                   |
| `litekv_replication_followers`     | gauge     | —                       | followers `-wait-for` can actually wait for          |
| `litekv_store_keys`                | gauge     | —                       | keys held, **tombstones included**                   |
| `litekv_store_segments`            | gauge     | —                       | logs the store is spread across                      |

Four of those have a wrinkle worth knowing before you build a graph on them.

**`litekv_store_keys` counts tombstones.** A key that was deleted is still an index entry until
a merge drops it, so this is the size of the index and not the number of live keys. It goes down
at a merge, not at a delete, and a sawtooth is the store working rather than data going missing.

**`litekv_role` is one series, not two.** The node exposes only the role it currently is, so a
promotion makes `role="replica"` go stale and `role="leader"` appear. `changes()` on it will not
work; alert on the label instead, or on `litekv_term`.

**`litekv_replication_streams` and `..._followers` are usually equal and are not the same
number.** A stream is a connection; a follower is a connection that identified itself and can
therefore be counted towards `-wait-for`. Streams above followers means something is reading the
replication route without being a countable follower, which is the difference between a
semi-synchronous leader that works and one that answers 202 to everything.

**`litekv_replication_streams` is the only gauge a request in flight contributes to.**
`litekv_requests_total` counts requests that *finished*, and a replication stream that has been
up for a week has never been counted once. Rate-of-requests panels will show a leader with a
busy follower as idle.

#### Queries to start from

```promql
# Requests a second, by route
sum by (route) (rate(litekv_requests_total[5m]))

# The share that failed, which should be flat and near zero
sum(rate(litekv_requests_total{status=~"5.."}[5m]))
  / sum(rate(litekv_requests_total[5m]))

# 99th percentile latency per route
histogram_quantile(0.99,
  sum by (route, le) (rate(litekv_request_duration_seconds_bucket[5m])))

# Writes that were stored but not replicated in time — semi-sync degrading
sum(rate(litekv_requests_total{status="202"}[5m]))

# Live keys, near enough, on a store that merges
litekv_store_keys
```

#### What to alert on

| alert                                                     | why it matters                                                       |
| --------------------------------------------------------- | --------------------------------------------------------------------- |
| `litekv_fenced == 1`                                       | this node has been replaced and every write it takes is refused. Nothing else about it looks wrong: it serves reads and reports a term |
| `litekv_replication_followers == 0` (with `-wait-for` set) | every write is answering 202. The data is on one machine and the guarantee you configured is not being met |
| `litekv_replication_streams == 0` on a leader with a replica | the follower is not connected, whatever the follower thinks           |
| `rate(litekv_requests_total{status="202"}[5m]) > 0`        | the same thing seen from the writes rather than from the followers    |
| `up == 0`, or `/health` failing                            | Prometheus's own metric; `/health` is the unauthenticated probe for a load balancer |

`litekv_fenced` is the one to wire up first. A fenced node is the quietest failure this design
has — it answers reads, reports a plausible term, and refuses every write with a 409 that only
the writing client sees.

#### What is not there

**No replication lag.** There is no metric saying how far behind a replica is. `/v1/status`
carries both positions and a client can compare them, but positions are opaque cookies and not
subtractable, so there is no seconds-behind number to graph. Comparing `litekv_store_keys`
between leader and replica is a rough proxy and no more than that.

**No Go runtime or process metrics.** No `go_goroutines`, no
`process_resident_memory_bytes` — those come from `client_golang`, and this module has no
dependencies outside the standard library. Use `node_exporter` or a container's own metrics for
the machine, and `up` for whether it is answering.

**Nothing about the disk.** No bytes written, no merge count, no sync latency. The store knows
those numbers; nothing exposes them yet.

The route label is the **pattern** and never the path. A label taken from the URL would be one
series per key, and `/metrics` would grow with the store until it was the largest thing this
server sends anybody. Every route is registered through one function so that it cannot be
forgotten, and `TestEveryRouteIsCounted` holds it there.

### Logs

Requests are logged at Debug, so turning request logging on is a level rather than a flag. A
server logging every request at Info is a server whose log nobody reads, and the failures that
matter log themselves already.

### Keeping strangers out

`-token-file` names a file holding a shared secret that every request must carry as
`Authorization: Bearer <token>`. A file and not a flag value, because an argument is visible in
`ps` to every process on the machine. The same token authenticates this node to its `-leader`.

It covers everything except `/health` — **replication included**, which is the route that matters
most, since it hands the whole database to whoever asks. The comparison is constant time: one
that stopped at the first wrong byte would tell a caller how much of the token it had right, and
a few thousand requests turn that into all of it.

This is a shared secret and nothing else. There are no users, no scopes, and no read-only
credential: anything that can read can also write. It is not a substitute for TLS, which is not
here — put it behind a proxy or on a private network.

### Timeouts, and the one route exempt from them

Without timeouts a client that opens a connection and sends a byte an hour holds a handler for as
long as it likes, and enough of them are the whole server. There is a 10s header timeout and the
three in the table above.

`-write-timeout` bounds how long a response may take to write, and there is exactly one response
here that is meant to still be being written next week: the replication stream. Rather than go
without the timeout because of that route, **the route takes its own deadline off**.

### Shutting down

`SIGINT` or `SIGTERM` stops the listener, gives requests in flight `-shutdown-timeout` to finish,
then closes the writer and then the store — in that order, which is the whole of it. Any other
order answers a request that was already accepted with a 503, or drops a write that was a moment
from being acknowledged.

A replication stream is a request that never finishes on its own, and Go's `Shutdown` waits for
every request rather than cancelling any of them, so a leader with a follower attached would
otherwise spend the whole timeout waiting for a handler that had no intention of returning:
**10.05 s against 0.03 s**, measured with two binaries on loopback. The streams are ended
explicitly. A follower whose stream ends that way reconnects, which is what it does about any
connection ending.

## Performance

A handler per request is a goroutine per request, and a write takes every shard of the store's
lock — so an HTTP server is the worst caller a store of this shape can have. There is one writer
goroutine in front of the store and every `PUT` and `DELETE` goes through it, which turns many
concurrent writers into one batch per disk wait.

Ten handler goroutines writing a 128-byte value:

| `-sync`  | nothing stored | straight to the store | through the queue |
| -------- | -------------- | --------------------- | ----------------- |
| `never`  | 971 ns         | 3,689 ns              | 1,248 ns          |
| `every`  | 996 ns         | 3,776 ns              | 1,276 ns          |
| `always` | 1,011 ns       | 3.82 ms               | 779 µs            |

The first column is a request that stores nothing, and it is there so the others can be read:
building the request is about a microsecond of every row. Take it off and the queue is worth
**9.8x** with no sync at all — pure lock contention going away — and **4.9x** under `always`,
where what is being amortized is one wait for the disk shared out among everybody waiting.

## Limitations

Read these before choosing this, not after.

- **There is no failover.** Which node is the leader is your decision and nobody else's.
  `POST /v1/promote` writes the decision down, it does not make it, and raising the term in two
  places at once puts two nodes on the same term and gives the guarantee away.
- **A fenced leader has to be told, and only replication tells it.** A leader that has been
  replaced finds out when something carrying a newer term asks it for records. Until then it goes
  on taking writes, and those writes are lost when it finds out.
- **A semi-synchronous write cannot be taken back.** A wait that runs out answers 202 and the
  record stays. A client that retries on 202 writes it twice.
- **No transactions.** A batch is atomic and durable and that is the whole of it: no reads in it,
  no isolation from a concurrent writer, and nothing to roll back once it is written.
- **Every key must fit in memory.** About 59 bytes per key, whatever the values are. Ten million
  keys is roughly 600 MB before any value is counted.
- **A range is gathered, not streamed**, and cannot be paged through cheaply — a client walking a
  large store restarts the gather on every page. `-max-scan` is the cap that keeps one client from
  parking a walk of the whole store in front of the writes.
- **No TLS and one shared secret.** No users, no scopes, no read-only credential. Put it behind a
  proxy or on a private network.
- **The empty key has no spelling in a path.** `/v1/keys/` is not a route. A batch line writes it
  and a range hands it back.
- **A follower is a whole copy.** No partial replication, no filtering by key. The unit is the log.
- **One process owns a directory**, and on Windows, solaris, aix, plan9 and wasm nothing enforces
  that — the lock needs `flock`, which those platforms do not have in Go's standard library.
- **Expiry is checked on read, not swept.** A store full of expired records is as large as a store
  full of live ones until the next merge.
- **There is no seconds-behind number for a replica.** `/v1/status` carries both positions and
  metrics carry neither lag nor a timestamp, so "how far behind is it" can be answered as
  caught-up-or-not and not as a duration. See [Prometheus](#prometheus).
- **There is no backup command, and a replica is not one.** A replica applies a mistaken delete
  as faithfully as anything else, with no delay and no undo. Backing up means stopping a node
  and copying its directory — which is what a replica is good for, since it gives you a node
  you can stop without taking the service down.

  Copying a directory from *under* a running node is the thing to be careful about, and not
  for the reason it looks like. The store survives being killed at any instant, so a
  point-in-time image of the directory is fine — a filesystem snapshot works. `cp -r` is not
  point-in-time: it reads the files over a period, and a merge landing in the middle of that
  can rename and remove logs between two of its reads, leaving a copy holding a set of logs
  that never existed together. `-merge-trigger 1` turns merging off if you want to copy a
  live directory anyway.

## The engine underneath

The storage is [github.com/tillknuesting/litekv](https://github.com/tillknuesting/litekv) — a
Bitcask-style log-structured store, standard library only, with no `net/*` package anywhere in
its dependency graph. If you want to build something of your own on the log rather than talk to a
server, that is the repository to take.

This one owns the protocol and nothing else: it is an `http.Handler` that reaches the store
through the same exported API any other caller would, so a change that breaks a caller breaks
these tests.

```go
import "github.com/tillknuesting/litekvd/server"

api := server.New(db, server.Options{MaxValue: 16 << 20})
http.ListenAndServe("127.0.0.1:8080", api)
```

## Working on it

`AGENTS.md` has the notes: what a handler test can and cannot say, the five mutations that
survive on purpose and why, and the traps this codebase has already sprung.

```bash
gofmt -l . && go vet ./... && go test -race ./...
go run ./tools/mutate          # break it on purpose, 109 ways, and check a test notices
```

## License

MIT. See [LICENSE](LICENSE).
