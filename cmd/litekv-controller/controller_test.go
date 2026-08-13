package main

import (
	"fmt"
	"testing"
	"time"
)

// The policy is the dangerous part of this program. The plumbing either works
// or fails loudly; a promotion made for the wrong reason is quiet and permanent,
// so these are about when it decides rather than about how it acts.

// at is a replica, that far through its leader's records.
func at(term, applied uint64) *status {
	return &status{Term: term, AppliedSeq: applied, Role: "replica", WaitFor: 1}
}

// leading is a node that says it is the leader, which is not the same thing and
// must not be spelled with at(): a write target whose status says "replica" is
// a node the controller re-promotes, so using at() for a healthy leader makes a
// test reach the network and pass or fail on whether something answers port
// 8080 on the machine running it. CI found that; this laptop had something
// listening.
func leading(term, seq uint64) *status {
	return &status{Term: term, Seq: seq, Role: "leader", WaitFor: 1}
}

// TestFurtherRanksByTermThenSequence. The same comparison a leader uses to
// decide whether a follower has reached a write — see reaches in server/acks.go.
// If the two ever disagree, that one is right and this is wrong.
func TestFurtherRanksByTermThenSequence(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b *status
		want bool
	}{
		{"a higher term wins outright", at(2, 1), at(1, 9_000), true},
		{"even against a much longer history", at(2, 0), at(1, 1<<40), true},
		{"and a lower term loses outright", at(1, 9_000), at(2, 1), false},
		{"within a term, the further sequence", at(1, 100), at(1, 99), true},
		{"one behind is behind", at(1, 99), at(1, 100), false},
		{"level is not further", at(1, 100), at(1, 100), false},
		{"nothing applied is the floor", at(1, 0), at(1, 1), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := further(c.a, c.b); got != c.want {
				t.Errorf("further(%+v, %+v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// TestASilentLeaderIsNotImmediatelyGone. The grace period is the difference
// between a failover and a hair trigger: a leader restarting for an upgrade is
// unreachable for a few seconds and must not be replaced for it.
func TestASilentLeaderIsNotImmediatelyGone(t *testing.T) {
	c := &controller{log: quiet(), grace: 15e9, leaseDuration: 15e9}

	nodes := []node{
		{pod: named("lk-0"), status: nil}, // the leader, silent
		{pod: named("lk-replica-0"), status: at(0, 5)},
	}

	// The first round only starts the clock. It must not act, and it must
	// remember when the silence began.
	if err := c.decide(t.Context(), nodes, "lk-0"); err != nil {
		t.Fatal(err)
	}
	if c.unreachableSince.IsZero() {
		t.Fatal("a silent leader did not start the grace period")
	}

	// And a leader that comes back inside the grace period clears it, so that
	// the next silence is measured from its own beginning rather than from an
	// hour ago.
	back := []node{
		{pod: named("lk-0"), status: leading(0, 6)},
		{pod: named("lk-replica-0"), status: at(0, 5)},
	}
	if err := c.decide(t.Context(), back, "lk-0"); err != nil {
		t.Fatal(err)
	}
	if !c.unreachableSince.IsZero() {
		t.Error("the leader answered again and the grace period was not reset")
	}
}

// TestItWillNotFailOverAnAsynchronousCluster. Without -wait-for, an
// acknowledged write may exist only on the node that has gone, and promoting
// anything at all loses it while answering 204 to everybody afterwards. That is
// not a trade a program should make on somebody's behalf without being told.
func TestItWillNotFailOverAnAsynchronousCluster(t *testing.T) {
	async := []node{
		{pod: named("lk-0"), status: nil},
		{pod: ready("lk-replica-0"), status: &status{Term: 0, AppliedSeq: 5, WaitFor: 0}},
	}
	old := &async[0]

	c := &controller{log: quiet(), requireWaitFor: true, leaseDuration: 15e9}
	c.held = now()

	best, refused := c.choose(async, old)
	if best != nil {
		t.Errorf("it would promote %s from an asynchronous cluster", best.pod.Metadata.Name)
	}
	if refused == "" {
		t.Error("it refused without saying why")
	}

	// Told explicitly that the loss is acceptable, it goes ahead. The same
	// nodes, one flag different, which is what makes this a test of the flag.
	c.requireWaitFor = false
	if best, refused := c.choose(async, old); best == nil {
		t.Errorf("told the loss was acceptable, it still refused: %s", refused)
	}
}

// TestItWillNotPromoteWhatItCannotSee. A controller that can reach nothing is
// far likelier to be the broken thing than to be the last witness of everything
// else breaking.
func TestItWillNotPromoteWhatItCannotSee(t *testing.T) {
	c := &controller{log: quiet(), requireWaitFor: false, leaseDuration: 15e9}
	c.held = now()

	blind := []node{
		{pod: named("lk-0"), status: nil},
		{pod: named("lk-replica-0"), status: nil},
		{pod: named("lk-replica-1"), status: nil},
	}
	if best, refused := c.choose(blind, &blind[0]); best != nil {
		t.Errorf("it picked %s, which never answered", best.pod.Metadata.Name)
	} else if refused == "" {
		t.Error("it refused without saying why")
	}

	// A pod that answers but is not Ready is not a candidate either: the
	// kubelet is the thing that decides what a Service may route to, and
	// promoting something Kubernetes will not send traffic to is a failover
	// that changes nothing.
	notReady := []node{
		{pod: named("lk-0"), status: nil},
		{pod: named("lk-replica-0"), status: at(0, 5)},
	}
	if best, _ := c.choose(notReady, &notReady[0]); best != nil {
		t.Errorf("it picked %s, which is not Ready", best.pod.Metadata.Name)
	}
}

// TestAStaleLeaseStopsItActing. A round polls every pod, and each of those may
// wait out the probe timeout. A round that took longer than half the lease has
// no business still believing it holds it.
func TestAStaleLeaseStopsItActing(t *testing.T) {
	nodes := []node{
		{pod: named("lk-0"), status: nil},
		{pod: ready("lk-replica-0"), status: at(0, 5)},
	}

	c := &controller{log: quiet(), requireWaitFor: false, leaseDuration: 15e9}
	c.held = longAgo() // renewed far too long ago

	if best, refused := c.choose(nodes, &nodes[0]); best != nil {
		t.Errorf("it would promote %s on a lease it renewed an hour ago", best.pod.Metadata.Name)
	} else if refused == "" {
		t.Error("it refused without saying why")
	}

	// The same situation with a fresh lease is a failover, which is what makes
	// the line above about the lease and not about anything else.
	c.held = now()
	if best, refused := c.choose(nodes, &nodes[0]); best == nil {
		t.Errorf("with a fresh lease it still refused: %s", refused)
	}
}

// TestALeaderPodThatIsGoneIsAFailover. This test had the policy backwards
// first, and a drill on a real cluster is what said so: the controller refused
// to act because the write Service pointed at a pod that was not in the list,
// which is exactly what a leader whose node died looks like. A pod that is gone
// is at least as unreachable as one that will not answer.
func TestALeaderPodThatIsGoneIsAFailover(t *testing.T) {
	c := &controller{log: quiet(), grace: 15e9, leaseDuration: 15e9}

	// The leader is not in the list at all — only the replicas are.
	nodes := []node{
		{pod: ready("lk-replica-0"), status: at(0, 5)},
		{pod: ready("lk-replica-1"), status: at(0, 4)},
	}

	if err := c.decide(t.Context(), nodes, "lk-0"); err != nil {
		t.Fatalf("a missing leader was an error rather than a failover: %v", err)
	}
	if c.unreachableSince.IsZero() {
		t.Fatal("a leader pod that has gone did not start the grace period")
	}

	// And once the grace period is out it picks the one that got furthest.
	c.unreachableSince = longAgo()
	c.held = now()

	best, refused := c.choose(nodes, &node{})
	if best == nil {
		t.Fatalf("it would not promote anything with the leader gone: %s", refused)
	}
	if best.pod.Metadata.Name != "lk-replica-0" {
		t.Errorf("it picked %s, but lk-replica-0 had applied more", best.pod.Metadata.Name)
	}
}

// TestAWriteTargetThatIsFollowingIsPromoted.
//
// A promoted node whose pod restarts comes back with its -leader still pointing
// at the write Service, which by then names itself. It follows itself and
// refuses every write with 409 while looking perfectly healthy — a write outage
// with nothing unhealthy in it.
//
// litekvd refuses to serve replication to itself, so the store survives that;
// this is the other half, which puts the role back. Found on a cluster, after
// the version without it left a healthy pod refusing writes indefinitely.
func TestAWriteTargetThatIsFollowingIsPromoted(t *testing.T) {
	c := &controller{log: quiet(), grace: 15e9, leaseDuration: 15e9, dryRun: true}

	following := &status{Role: "replica", Term: 1, AppliedSeq: 9, WaitFor: 1,
		Leader: "http://lk-litekvd-leader:8080"}
	nodes := []node{
		{pod: ready("lk-litekvd-replica-0"), status: following},
		{pod: ready("lk-litekvd-replica-1"), status: at(1, 9)},
	}

	// It must not be read as an unreachable leader: the node is answering, and
	// waiting out a grace period would be waiting for something that is never
	// going to change on its own.
	if err := c.decide(t.Context(), nodes, "lk-litekvd-replica-0"); err != nil {
		t.Fatal(err)
	}
	if !c.unreachableSince.IsZero() {
		t.Error("a write target that is following was treated as unreachable")
	}
}

// TestALeaseIsJudgedWithoutComparingClocks.
//
// The obvious expiry check subtracts the Lease's renewTime from time.Now(), and
// it is wrong in a way that only appears when it matters: renewTime was written
// by another machine's clock. This is that scenario — a peer renewing steadily
// while this controller's clock runs an hour fast — and the answer must be that
// the Lease is held, not that it is free.
func TestALeaseIsJudgedWithoutComparingClocks(t *testing.T) {
	c := &controller{log: quiet(), leaseDuration: 15 * time.Second}

	held := &lease{Spec: leaseSpec{
		HolderIdentity:       "the-other-controller",
		LeaseDurationSeconds: 15,
		// Written by a clock an hour behind this one, which is what skew looks
		// like from here. A check that subtracted this from now would call it
		// expired by a factor of two hundred.
		RenewTime: time.Now().Add(-time.Hour).UTC().Format(microTime),
	}}
	held.Metadata.ResourceVersion = "100"

	now := time.Now()
	if c.stale(held, now) {
		t.Fatal("the first sight of a Lease was called stale; that is a clock comparison")
	}

	// It keeps being renewed: a new resourceVersion every round, and its
	// timestamps stay an hour behind. Never stale, however long this runs.
	for i := range 20 {
		held.Metadata.ResourceVersion = fmt.Sprintf("%d", 101+i)
		now = now.Add(5 * time.Second)
		if c.stale(held, now) {
			t.Fatalf("a Lease being renewed was called stale on round %d", i)
		}
	}

	// And when the holder stops — the resourceVersion stops moving — it goes
	// stale on this controller's own clock, and only then.
	now = now.Add(10 * time.Second)
	if c.stale(held, now) {
		t.Error("called stale before a full lease duration of silence")
	}
	now = now.Add(6 * time.Second)
	if !c.stale(held, now) {
		t.Error("a holder that stopped renewing was never called stale")
	}
}
