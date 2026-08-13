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
| `ServiceMonitor`                  | optional, for prometheus-operator                    |

Each pod gets a PersistentVolumeClaim through `volumeClaimTemplates`, so a pod
that is rescheduled finds its own store where it left it.

## The one thing to understand first

**This chart does not fail over, and that is deliberate rather than unfinished.**

litekvd has no leader election. Which node leads is a decision something outside
the process makes, because two nodes raising the term at once puts both on the
same term and gives the guarantee away — the thing that makes a promoted replica
safe to write to is that exactly one promotion happened. A chart that elected a
leader by itself would be quietly undoing the engine's central rule.

So the leader is its own StatefulSet with one pod, the followers are another,
and promotion is a runbook a person runs. What the chart does give you is that
the runbook is three commands, does not involve reinstalling anything, and
cannot put two nodes in the write path while you do it.

If you want automatic failover, what you want is an operator holding a lease —
and that is a separate piece of software, not a values flag.

## Development, with k3d

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

## What testing found

Three bugs, all in this chart rather than in litekvd, and none of them visible
from `helm template`.

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
