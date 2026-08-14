# Running litekvd on Kubernetes

A Helm chart for one leader and two replicas, on three nodes.

```bash
helm install lk deploy/charts/litekvd -n litekv --create-namespace
```

Everything here was run against a four-node [k3d](https://k3d.io) cluster before
it was written down, including the failover drill — which is how three bugs in
this chart were found. They are in [What testing found](#what-testing-found), at
the bottom, because the last one put two nodes in the write path and none of
them was visible from `helm template`.

## What you get

| object                    | what it is                                                    |
| ------------------------- | -------------------------------------------------------------- |
| `StatefulSet/<r>-litekvd`         | the leader. One replica, and that is not a number to raise |
| `StatefulSet/<r>-litekvd-replica` | the followers. Two by default                        |
| `Service/<r>-litekvd-leader`      | **writes**. Selects exactly one pod, by name         |
| `Service/<r>-litekvd-read`        | **reads**, from every pod, some possibly behind      |
| `Service/<r>-litekvd-headless`    | stable per-pod DNS, and what the StatefulSets are governed by |
| `PodDisruptionBudget` ×2          | the leader is not evictable by default. See below    |
| `Secret/<r>-litekvd-token`        | a generated bearer token, when `auth.enabled`        |
| `Deployment/<r>-litekvd-controller` | optional: automatic failover, off by default       |
| `Role`, `RoleBinding`, `Lease`    | what the controller needs, and nothing more          |
| `ServiceMonitor`                  | optional, for prometheus-operator                    |

Each pod gets a PersistentVolumeClaim through `volumeClaimTemplates`, so a pod
that is rescheduled finds its own store where it left it.

## The one thing to understand first

**litekvd has no leader election, and is not going to grow one.** Two stores
raising the term at once puts both on the same term and gives the guarantee
away; what makes a promoted replica safe to write to is that exactly one
promotion happened. So exactly one thing may decide, and it is not the database.

You have two ways to be that one thing.

**By hand.** The leader is its own StatefulSet, promotion is three commands, and
[the runbook](#the-failover-runbook) is what they are. Nothing happens without
you, which for a small deployment is often the right answer: the failure modes
are yours to see and there is no policy to trust at three in the morning.

**With the controller**, `controller.enabled=true`. It holds a
`coordination.k8s.io` Lease and promotes for you. This is not a leader election
implemented here — the API server sits on a Raft cluster, so a Lease updated
with `resourceVersion` optimistic concurrency *is* a linearizable
compare-and-swap. The controller consumes agreement that already exists rather
than inventing any, which is why it is a few hundred lines and not a Raft
implementation. See [Automatic failover](#automatic-failover).

Either way the write Service selects **one pod by name**, so two nodes cannot be
in the write path while you change which one it is.

## Automatic failover

```bash
helm install lk deploy/charts/litekvd -n litekv --create-namespace \
  --set controller.enabled=true \
  --set config.waitFor=1
```

Two controller pods, one holding the Lease and one standing by. Once a second it
takes or renews the Lease, asks every litekvd pod for `/v1/status`, and if the
leader has been gone for longer than `controller.grace` it promotes the replica
that got furthest, points the write Service at it, and takes the old one out of
the read path.

Measured on k3d, with the leader scaled away and nothing else touched:

```
17:17:31 WARN the leader stopped being the leader pod=lk-litekvd-0 why=gone grace=10s
17:17:41 WARN failing over from=lk-litekvd-0 to=lk-litekvd-replica-0 candidate applied=3
17:17:41 WARN failed over leader=lk-litekvd-replica-0 term=1
```

Ten seconds, to the second, and the record written before the failover was still
there afterwards.

### What it refuses to do

The reluctance is the design. Each of these was written as a guard and then
tested by deleting the guard and watching a test fail.

**It will not fail over an asynchronous cluster.** With `config.waitFor: 0` an
acknowledged write may exist only on the node that just died, so promoting
anything at all loses it while answering 204 to everybody afterwards. The
controller refuses and says so. `controller.requireWaitFor: false` overrides it,
and that is a decision about your data rather than a tuning knob.

**It will not promote a node it cannot see**, or one the kubelet says is not
Ready. A controller that can reach nothing is far likelier to be the broken
thing than to be the last witness of everything else breaking.

**It does not compare clocks.** A Lease's `renewTime` was written by another
machine, and a controller whose clock runs fast would find every Lease expired
and seize it while the holder was renewing happily — two controllers acting at
once, produced by the very thing meant to prevent it. What is watched instead is
`resourceVersion`: while it keeps changing somebody is renewing, and when it
stops changing for a lease duration *by this controller's own clock*, they are
not. Every duration is measured on one clock and it does not matter which.

**It will not act on a stale Lease.** A round polls every pod, and each of those
can wait out `-probe-timeout`; a round that took longer than half the Lease
duration does nothing and comes back. That is the window where two controllers
could otherwise overlap.

**It will not treat a blip as a death.** `controller.grace` is a run of
consecutive failures, reset the moment the leader answers again — not an
aggregate. A leader restarting for an upgrade must not be replaced for it, which
is why the default is 15s and why it has to be comfortably above how long your
leader takes to come back.

The one thing it does *not* wait for is a fenced leader: a node that has been
told by a newer term that it is finished is not a judgement call, so that
promotes immediately.

### Ranking the candidates

`/v1/status` carries `seq` and `applied_seq`, and the controller ranks by
`(term, applied_seq)` — the same comparison a leader uses to decide whether a
follower has reached a write. Positions themselves stay opaque cookies; these
are separate integers added because something choosing between replicas has to
answer "which got furthest" and cannot do it with two base64 strings.

### What it cannot do

**It cannot stop the old leader taking writes from a client that reaches it
directly**, bypassing the Service. Nothing here can: fencing is something a
store is *told*, and the old leader is told only when something carrying the
newer term talks to it. Traffic is moved, not amputated.

This is not a theoretical edge. It was produced deliberately: the leader's node
was frozen with `docker pause` — alive, holding its data, answering nothing —
the controller failed over after 14s, and then the node was thawed. The old
leader woke reporting `role=leader term=0 fenced=false`, out of both Services and
entirely unaware. A write sent straight to its pod IP was **accepted, readable on
that node, and invisible to the real leader**. Those records are lost when its
volume is eventually wiped.

The thing that would close it is `DB.Demote` and a `/v1/demote` route — already
on the engine's roadmap — which would let the controller stand a stale leader
down rather than only starve it of traffic. Until then, a client that talks to
pod IPs instead of the Service is outside what any of this can promise.

What the controller does do is keep it out of both Services — by name for
writes, and by taking the `litekv.io/serving` label off it for reads, every
round rather than only at the moment of failover. That second part exists
because of a drill: the old leader's pod came back, its template stamped the
label on again, and it rejoined the read Service still serving the history that
diverged at the promotion.

### Watching it before trusting it

`controller.dryRun: true` decides everything and changes nothing, logging what
it would have done. Run it that way for a week and read the grace periods and
candidate choices it reports; a failover policy you have not watched is one you
are guessing about.

## Development, with k3d

### Development, with k3d

A cluster, the image, and the chart:

```bash
k3d cluster create litekv --agents 3 --wait

# Build and hand the image to the cluster's containerd. No registry involved,
# which also side-steps a Docker credential helper that is not installed.
docker build -t litekvd:dev .
k3d image import litekvd:dev -c litekv

helm install lk deploy/charts/litekvd -n litekv --create-namespace \
  --set image.repository=litekvd \
  --set image.tag=dev \
  --set image.pullPolicy=Never \
  --set config.waitFor=1 \
  --set auth.enabled=true \
  --wait
```

`pullPolicy=Never` matters: without it the kubelet tries to pull `litekvd:dev`
from a registry that has never heard of it, and the pod sits in
`ErrImagePull` next to an image that is already on the node.

Then talk to it:

```bash
TOKEN=$(kubectl -n litekv get secret lk-litekvd-token -o jsonpath='{.data.token}' | base64 -d)
kubectl -n litekv port-forward svc/lk-litekvd-leader 8080:8080 &

curl -H "Authorization: Bearer $TOKEN" -X PUT --data-binary 'ada' \
  http://127.0.0.1:8080/v1/keys/user:1
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/v1/keys/user:1
```

Who is who, and where:

```bash
kubectl -n litekv get pods -L litekv.io/role -o wide

# and who is actually taking writes, which is the Service's selector and not a
# pod label:
kubectl -n litekv get svc lk-litekvd-leader \
  -o jsonpath='{.spec.selector.statefulset\.kubernetes\.io/pod-name}{"\n"}'
```

Tear it down:

```bash
helm uninstall lk -n litekv
kubectl -n litekv delete pvc --all     # volumeClaimTemplates PVCs outlive the release, on purpose
k3d cluster delete litekv
```

`helm uninstall` deliberately leaves the volumes. A chart that deleted a
database's disks on uninstall would be one bad afternoon away from being the
worst tool in the room.

## Choosing the values

The whole set is in [`charts/litekvd/values.yaml`](charts/litekvd/values.yaml),
commented. The ones that change what you are running:

```yaml
replicas: 2          # followers, on top of the one leader

config:
  sync: every        # every | always | never
  waitFor: 1         # hold each write until this many followers have it

auth:
  enabled: true      # a bearer token on every route except /health

persistence:
  size: 8Gi
  storageClass: ""   # empty means the cluster default
```

**`config.waitFor` is the one worth thinking about.** At `0` a write is
acknowledged as soon as the leader has it, so losing the leader loses whatever
the replicas had not received. At `1` the write waits for a follower, which
costs a round trip — 8.3 µs becomes 215 µs on loopback — and buys a failover
that does not lose acknowledged writes. What it cannot do is take a write back:
a wait that runs out answers **202** and the record stays. A client that retries
on 202 writes it twice.

**`-spool-dir` is set to the data volume and should stay there.** A snapshot on
its way to a follower is spooled to a file first, and the default is the system
temporary directory — which in a container is either nothing at all or a
memory-backed emptyDir. Either way it is the wrong place for a copy of every
live record.

## The failover runbook

Run through this on a test cluster before you need it. Every line below was
executed against k3d and the outputs are what it actually printed.

**1. Establish that the leader is really gone.** A promotion while the old
leader is still taking writes is the failure this whole design is arranged to
prevent. Scaling it to zero is how you make sure:

```bash
kubectl -n litekv scale statefulset lk-litekvd --replicas=0
kubectl -n litekv wait --for=delete pod/lk-litekvd-0 --timeout=90s
```

Writes fail here, because `lk-litekvd-leader` has no endpoints. That is correct:
better a refused write than one that lands on a node about to be superseded.

**2. Promote a replica.** Pick one — with `waitFor` set they all had the write,
and `/v1/status` tells you where each got to:

```bash
kubectl -n litekv port-forward pod/lk-litekvd-replica-0 8081:8080 &
curl -H "Authorization: Bearer $TOKEN" -X POST http://127.0.0.1:8081/v1/promote
# {"term":1}
```

**3. Point the write Service at it.** This repoints every follower too, since a
follower's `-leader` is that Service and not a pod:

```bash
kubectl -n litekv patch svc lk-litekvd-leader \
  -p '{"spec":{"selector":{"statefulset.kubernetes.io/pod-name":"lk-litekvd-replica-0"}}}'
```

Set `leaderPodName` to the same value before the next `helm upgrade`, or the
upgrade will put the selector back on the old leader.

**4. Watch the followers come back.** Writes work again immediately, but for a
few seconds they answer **202 with `Litekv-Replicated: 0`** — the surviving
replica is still reconnecting to an endpoint that just changed under it, so
there is nobody to wait for yet. This is the semi-synchronous guarantee being
honest rather than broken:

```bash
curl -H "Authorization: Bearer $TOKEN" -i -X PUT --data-binary 'x' \
  http://127.0.0.1:8080/v1/keys/x
# HTTP/1.1 202 Accepted
# Litekv-Replicated: 0

# ... a few seconds later
# HTTP/1.1 204 No Content
# Litekv-Replicated: 1
```

**The failover is not complete until `litekv_replication_followers` has come
back up.** Until then you are running unreplicated, whatever the writes say.

**5. Leave the old leader at zero.** This is the step to get right, and the one
the drill was extended to cover.

The old leader has its own history and **does not know it was superseded** —
nothing has contacted it carrying the newer term, so it reports `role=leader`,
`term=0`, and `/health` 200. It is not fenced, because fencing is something a
store is told and nobody has told it.

The write Service cannot reach it: that selects one pod by name and the name is
now the promoted one. Verified by bringing the old leader back deliberately and
watching the endpoint list stay at one. **But the read Service selects every
healthy pod**, so a returning old leader will serve reads out of a history that
diverged at the moment of the promotion.

So: leave `lk-litekvd` scaled to `0` until you have dealt with its volume. Its
PVC still holds whatever it had that the replicas never received, and that is
the only copy — copy it out before you wipe anything.

The way back to a normal shape is either to stop everything, copy the promoted
node's data into the leader's PVC, and scale the leader back up; or to accept
the loss, wipe the old leader's volume, and let it come back as a follower and
take a snapshot. Which is right depends on what the old leader had that nobody
else did, and that is a judgement rather than a command.

## Operating it

**The leader will not be evicted.** `podDisruptionBudget.leaderMaxUnavailable`
is `0`, so `kubectl drain` on the leader's node refuses rather than proceeds.
This will surprise somebody during routine maintenance, and it is meant to: with
no automatic failover, evicting the leader is an outage until a person promotes
a replica. Do the promotion first, then drain. Set it to `1` if you would rather
have the drain and take the outage.

**Metrics.** Every pod serves `/metrics`; `serviceMonitor.enabled=true` wires up
prometheus-operator against the headless Service so each pod is scraped
separately — a leader's numbers and a replica's are different numbers and both
are wanted. `litekv_fenced == 1` is the first alert to wire up: a fenced node
serves reads and reports a plausible term while refusing every write. The full
list is in the [main README](../README.md#prometheus).

**Backups.** A replica is not a backup: it applies a mistaken delete as
faithfully as anything else, immediately. Backing up is stopping a node and
copying its volume, and a replica is exactly the node you can afford to stop.
There is no backup command.

**Upgrading litekvd.** Roll the replicas first and the leader last; a replica
running a newer build against an older leader is the direction that has been
tested. `helm upgrade` with a new `image.tag` does the leader in place, which is
a short write outage rather than a failover.

## Testing the controller

The policy tests run against structs. The rest run against stand-ins — a
Kubernetes API server that enforces `resourceVersion` the way etcd does, and
litekvd nodes that answer `/v1/status` and raise their term when promoted — so
everything the controller changes about a cluster goes through a real HTTP
round trip and a wrong patch shape is visible rather than theoretical.

Nineteen tests, and the one worth naming is **two controllers and only one
acts**: a hundred rounds of both trying, with the stand-in refusing any write
that does not carry the version it read. Both holding it at once means both
promoting, which means two stores on two terms with two histories. Plus the race
underneath it — both believing the Lease is free and both writing — where the
loser must come away with "somebody else has it" and not with an error.

Every guard was checked by breaking it and watching a test fail. Three of those
checks found the *test* wanting rather than the code:

- A mutation making a compare-and-swap conflict an error survived, because no
  test ever reached that path: a controller that can see the Lease is held
  stands by without writing. `TestARaceOnTheLeaseIsResolvedByTheAPIServer`
  produces a genuine race.
- A mutation promoting the wrong node survived twice. Every stand-in listens on
  127.0.0.1 and the controller uses one port for all of them, so promoting the
  corpse reached the same listener and the count was identical — and an empty
  pod IP does not separate them either, since Go reads `http://:8080` as
  localhost. The stand-in records the `Host` each promotion carried.
- One mutation survived and was left open for a while: swapping strategic merge
  patch for a plain JSON merge patch on the Service selector. Neither the unit
  tests nor the stand-in could tell them apart, because the stand-in merges
  either way. It was settled by patching a real Service both ways —

  ```
  before            {instance: lk, name: litekvd, pod-name: lk-litekvd-0}
  after json-merge  {instance: lk, name: litekvd, pod-name: promoted-A}
  after strategic   {instance: lk, name: litekvd, pod-name: promoted-B}
  ```

  — identical, because strategic merge patch only differs on lists with a patch
  strategy and `spec.selector` is a plain map. So it is an equivalent mutant and
  there is deliberately no entry for it. The controller speaks one patch type
  now instead of two, which is a simplification the measurement paid for; and
  `null` removing a label was checked the same way rather than assumed.

## What testing found

Five bugs. Four were in this chart, one was in litekvd itself, and none of them
was visible from `helm template` or from a unit test.

**A node that follows itself destroys the data, and everything following it.**
The worst thing found here by a distance, and it took a chaos run to reach.

A promoted replica whose pod restarts comes back with its `-leader` still
pointing at the write Service — which by then names itself. It asks itself what
comes after its position, concludes it has diverged from itself, and takes a
snapshot: and `ApplySnapshot` empties the store *before* it reads, so the node
empties itself and applies the nothing it has become. Every follower behind it
is emptied next. No error is reported anywhere, because at each step something
asked for records and something else gave them.

The cluster went from three nodes holding a key to two nodes holding zero, and
the only surviving copy was on the node that had been failed away from.

litekvd now **refuses to serve a replication stream from a node that is
following**. Nothing in the engine could refuse it — a follower asked and a
leader answered — so it is refused in the server, which is the layer that knows
which of the two it is. Replaying the exact sequence afterwards: `keys=3` where
it had been `keys=0`.

That left the write path down, because the node was still a replica refusing
writes while looking perfectly healthy. So the controller learned the other
half: a write target that reports `role=replica` is re-promoted rather than
failed away from — the Service naming it is the statement that it should lead,
and it holds the data. Measured after the fix: promoted at term 2, writes back
to 204, every key still there.

**A promotion plus a returning old leader put two nodes in the write Service.**
The worst of the three, and it took a second drill to find: promote a replica,
move the label, then let the old leader's node come back. Its StatefulSet
recreates the pod, the pod template stamps the role label on it, and the write
Service — which selected on that label — now has two endpoints. Two nodes taking
writes into two histories, which is the one failure the engine's whole term
mechanism exists to prevent, arranged by the chart on top of it.

The write Service selects a pod **by name** now, using the
`statefulset.kubernetes.io/pod-name` label Kubernetes maintains itself. A label
can end up on two pods; a name has room for one answer. Re-run with the fix, the
old leader comes back and the endpoint list stays at exactly one.

**The role label was in the StatefulSet selector, so promoting a pod orphaned
it.** Moving `litekv.io/role` to `leader` made the pod stop matching the
controller that owned it: the StatefulSet quietly built a replacement, and the
PodDisruptionBudget started reporting `UnmanagedPods`. The fix is that the
controller and the PDB select on `app.kubernetes.io/component`, which never
changes, and only the Service selects on the role, which is the thing that
moves. A promotion now leaves the pod exactly where it was.

Worth knowing if you hit this on an existing install: **a StatefulSet's selector
is immutable**, so `helm upgrade` cannot make that change. It needs
`kubectl delete statefulset --cascade=orphan` and a reinstall, or a full
uninstall.

**A promoted replica had dropped to asynchronous replication.** Only the leader
StatefulSet carried `-wait-for`, so the moment a replica was promoted the
semi-synchronous guarantee quietly went away — writes were acknowledged with no
follower having them, and nothing said so. Both StatefulSets carry the flag now.
It does nothing on a replica, which refuses writes anyway, and is exactly what
is wanted the second one stops being a replica.

The tell was `Litekv-Replicated` disappearing from the response headers after a
failover, which is a thing you only see if you look at the headers during a
drill rather than at the status code.
