package main

import (
	"strconv"
	"testing"
	"time"
)

// These run the acting half against stand-ins: a Kubernetes API server that
// enforces resourceVersion, and litekvd nodes that answer /v1/status and raise
// their term when promoted. Everything the controller changes about a cluster
// goes through here, and a wrong patch shape is visible rather than theoretical.

// at builds a pod that is Ready with a given IP, for the stand-ins.
func podAt(name, ip string, labels map[string]string) pod {
	p := ready(name)
	p.Status.PodIP = ip
	if labels != nil {
		p.Metadata.Labels = labels
	} else {
		p.Metadata.Labels = map[string]string{servingLabel: "true"}
	}
	return p
}

// TestAFailoverPromotesAndMovesTheTraffic, in that order and no other.
//
// The order is the point: pointing the Service first sends writes to a node
// that is still a replica and answers every one of them 409. The candidate is
// the replica that got furthest, and the old leader comes out of the read path
// on the way.
func TestAFailoverPromotesAndMovesTheTraffic(t *testing.T) {
	ev := &events{}
	behind, behindAt := newFakeStoreIn(t,
		status{Role: "replica", Term: 1, AppliedSeq: 4, WaitFor: 1}, "lk-litekvd-replica-0", ev)
	ahead, aheadAt := newFakeStoreIn(t,
		status{Role: "replica", Term: 1, AppliedSeq: 9, WaitFor: 1}, "lk-litekvd-replica-1", ev)

	host, port := splitHostPort(t, aheadAt)
	behindHost, _ := splitHostPort(t, behindAt)

	pods := []pod{
		// "localhost" and not "" or "127.0.0.1": every stand-in listens on the
		// same address and the controller uses one port for all of them, so the
		// only thing that separates "promoted the candidate" from "promoted the
		// corpse" is the Host the request carried. An empty IP does not do it —
		// Go reads http://:8080 as localhost — and that is why a mutation
		// promoting the wrong node survived two attempts at this test.
		podAt("lk-litekvd-0", "localhost", nil),
		podAt("lk-litekvd-replica-0", behindHost, nil),
		podAt("lk-litekvd-replica-1", host, nil),
	}
	f, srv, api := newFakeAPI(pods, "lk-litekvd-0")
	defer srv.Close()
	f.events = ev

	c := against(t, api, "controller-one")
	c.port = port // both stand-ins are on the same host, different ports
	c.held = now()
	c.requireWaitFor = true
	c.unreachableSince = longAgo()

	nodes := []node{
		{pod: pods[0], status: nil},
		{pod: pods[1], status: &status{Role: "replica", Term: 1, AppliedSeq: 4, WaitFor: 1}},
		{pod: pods[2], status: &status{Role: "replica", Term: 1, AppliedSeq: 9, WaitFor: 1}},
	}

	if err := c.failover(t.Context(), nodes, &nodes[0], "unreachable"); err != nil {
		t.Fatal(err)
	}

	// The furthest replica was promoted, at its own address, and only it.
	if got := ahead.promotionsFrom("127.0.0.1"); got != 1 {
		t.Errorf("the furthest replica was promoted %d times at its own address", got)
	}
	if got := ahead.promotionsFrom("localhost"); got != 0 {
		t.Errorf("something promoted the old leader's address %d times", got)
	}
	if behind.promotions() != 0 {
		t.Errorf("a replica that was behind was promoted %d times", behind.promotions())
	}

	// The write Service points at it, and still carries its app labels — a
	// patch that replaced the selector rather than merging into it would have
	// dropped those and pointed the Service at half the namespace.
	got := f.selector()
	if got[podNameLabel] != "lk-litekvd-replica-1" {
		t.Errorf("the write Service points at %q", got[podNameLabel])
	}
	if got["app.kubernetes.io/name"] != "litekvd" {
		t.Error("the patch replaced the selector instead of merging into it")
	}

	// And the old leader is out of the read path, because it does not know it
	// has been replaced and would answer reads from a history that stopped.
	if _, serving := f.labelsOf("lk-litekvd-0")[servingLabel]; serving {
		t.Error("the old leader is still in the read Service")
	}

	// The order the doc comment above claims, now that the stand-ins share a
	// log and it can be asserted rather than described. It could not be before,
	// and a mutation swapping the two calls survived a sweep because of it:
	// pointing the Service first sends every write to a node that is still a
	// replica, and it answers all of them 409 until the promotion lands.
	if !ev.before("lk-litekvd-replica-1 was promoted",
		"the write Service points at lk-litekvd-replica-1") {
		t.Errorf("promotion and traffic went in the wrong order, or not at all: %s", ev)
	}
}

// TestTheWriteTargetIsNotPromotedTwice. decide re-promotes a write target that
// is following rather than leading — the self-follow recovery — and it must do
// that once per round and not once per poll of a node.
func TestTheWriteTargetIsNotPromotedTwice(t *testing.T) {
	store, at := newFakeStore(t, status{Role: "replica", Term: 1, AppliedSeq: 9, WaitFor: 1,
		Leader: "http://lk-litekvd-leader:8080"})
	host, port := splitHostPort(t, at)

	pods := []pod{podAt("lk-litekvd-replica-0", host, nil)}
	_, srv, api := newFakeAPI(pods, "lk-litekvd-replica-0")
	defer srv.Close()

	c := against(t, api, "controller-one")
	c.port = port
	c.held = now()

	nodes := []node{{pod: pods[0], status: &status{Role: "replica", Term: 1, AppliedSeq: 9,
		WaitFor: 1, Leader: "http://lk-litekvd-leader:8080"}}}

	if err := c.decide(t.Context(), nodes, "lk-litekvd-replica-0"); err != nil {
		t.Fatal(err)
	}
	if store.promotions() != 1 {
		t.Fatalf("a following write target was promoted %d times, want 1", store.promotions())
	}

	// Now it says it is the leader, which is the state after the promotion. A
	// second round must leave it alone.
	nodes[0].status = &status{Role: "leader", Term: 2, Seq: 9, WaitFor: 1}
	if err := c.decide(t.Context(), nodes, "lk-litekvd-replica-0"); err != nil {
		t.Fatal(err)
	}
	if store.promotions() != 1 {
		t.Errorf("it promoted a healthy leader again: %d promotions", store.promotions())
	}
}

// waiting builds a cluster of one silent leader and one replica that can be
// promoted, with the controller holding a fresh lease.
func waiting(t *testing.T, leader *status) (*fakeStore, *controller, []node) {
	t.Helper()

	replica, at := newFakeStore(t, status{Role: "replica", Term: 1, AppliedSeq: 9, WaitFor: 1})
	host, port := splitHostPort(t, at)

	pods := []pod{
		podAt("lk-litekvd-0", "10.0.0.9", nil),
		podAt("lk-litekvd-replica-0", host, nil),
	}
	_, srv, api := newFakeAPI(pods, "lk-litekvd-0")
	t.Cleanup(srv.Close)

	c := against(t, api, "controller-one")
	c.port, c.held, c.requireWaitFor = port, now(), true

	return replica, c, []node{
		{pod: pods[0], status: leader},
		{pod: pods[1], status: &status{Role: "replica", Term: 1, AppliedSeq: 9, WaitFor: 1}},
	}
}

// TestTheGracePeriodIsWaitedOutAndThenActedOn.
//
// TestASilentLeaderIsNotImmediatelyGone covers the first silent round, which
// only starts the clock. This is the round after it — inside the period, with a
// promotable replica sitting right there — and then the round after the period
// has run out. Checking the period once and never again is a hair trigger with
// a reassuring comment above it, and it is what a surviving mutation said this
// suite could not tell apart.
func TestTheGracePeriodIsWaitedOutAndThenActedOn(t *testing.T) {
	replica, c, nodes := waiting(t, nil) // nil: the leader says nothing at all

	// Two rounds, microseconds apart, against a fifteen-second grace period.
	for i := range 2 {
		if err := c.decide(t.Context(), nodes, "lk-litekvd-0"); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	if got := replica.promotions(); got != 0 {
		t.Fatalf("it failed over inside the grace period: %d promotions", got)
	}

	// And when the silence has outlasted the period, it acts. Without this
	// half, a controller that never failed over at all would pass the above.
	c.unreachableSince = longAgo()
	if err := c.decide(t.Context(), nodes, "lk-litekvd-0"); err != nil {
		t.Fatal(err)
	}
	if got := replica.promotions(); got != 1 {
		t.Errorf("the grace period ran out and it promoted %d times, want 1", got)
	}
}

// TestAFencedLeaderIsNotMadeToWait.
//
// The grace period is there for a leader that might come back: a rolling
// restart, a long GC, a node rebooting. A fenced one is not that. Something
// carrying a newer term has already told it that it is finished, so there is
// nothing to wait for and the wait is an outage nobody chose.
func TestAFencedLeaderIsNotMadeToWait(t *testing.T) {
	replica, c, nodes := waiting(t, &status{Role: "leader", Term: 1, Fenced: true, WaitFor: 1})

	// The first round starts the clock whatever the reason, so the second round
	// is where the two policies differ — and it is still well inside the
	// fifteen seconds a silent leader would get.
	if err := c.decide(t.Context(), nodes, "lk-litekvd-0"); err != nil {
		t.Fatal(err)
	}
	if got := replica.promotions(); got != 0 {
		t.Fatalf("it failed over on first sight, without a round to confirm: %d", got)
	}

	if err := c.decide(t.Context(), nodes, "lk-litekvd-0"); err != nil {
		t.Fatal(err)
	}
	if got := replica.promotions(); got != 1 {
		t.Errorf("a fenced leader was made to sit out the grace period: %d promotions", got)
	}
}

// TestTidyPutsTheReadPathBackTogether.
//
// Every round, not only at a failover. The case that matters is the old leader
// coming *back*: its StatefulSet recreates the pod, the template stamps the
// serving label on, and it rejoins the read Service holding a history that
// stopped being the history at the promotion.
func TestTidyPutsTheReadPathBackTogether(t *testing.T) {
	pods := []pod{
		// A stale leader, back from the dead, wrongly serving.
		podAt("lk-litekvd-0", "10.0.0.1", map[string]string{servingLabel: "true"}),
		// The real leader, wrongly not serving.
		podAt("lk-litekvd-replica-0", "10.0.0.2", map[string]string{}),
		// A replica, correctly serving.
		podAt("lk-litekvd-replica-1", "10.0.0.3", map[string]string{servingLabel: "true"}),
	}
	f, srv, api := newFakeAPI(pods, "lk-litekvd-replica-0")
	defer srv.Close()

	c := against(t, api, "controller-one")

	nodes := []node{
		{pod: pods[0], status: &status{Role: "leader", Term: 0}}, // the stale one
		{pod: pods[1], status: &status{Role: "leader", Term: 1}}, // the real one
		{pod: pods[2], status: &status{Role: "replica", Term: 1}},
	}

	if err := c.tidy(t.Context(), nodes, "lk-litekvd-replica-0"); err != nil {
		t.Fatal(err)
	}

	if _, serving := f.labelsOf("lk-litekvd-0")[servingLabel]; serving {
		t.Error("a node claiming to be a leader it is not is still serving reads")
	}
	if _, serving := f.labelsOf("lk-litekvd-replica-0")[servingLabel]; !serving {
		t.Error("the actual leader was not put back into the read Service")
	}
	if _, serving := f.labelsOf("lk-litekvd-replica-1")[servingLabel]; !serving {
		t.Error("a healthy replica was taken out of the read Service")
	}

	// And a second round changes nothing, because it is a rule and not a
	// reaction: a controller that patched every round would rewrite three
	// objects a second for ever.
	before := len(f.patchedPods)
	if err := c.tidy(t.Context(), nodes, "lk-litekvd-replica-0"); err != nil {
		t.Fatal(err)
	}
	if len(f.patchedPods) != before {
		t.Errorf("a settled cluster was patched again: %v", f.patchedPods[before:])
	}
}

// TestDryRunChangesNothing. The way to watch a policy for a week before
// trusting it, which is worth nothing if it acts anyway.
func TestDryRunChangesNothing(t *testing.T) {
	store, at := newFakeStore(t, status{Role: "replica", Term: 1, AppliedSeq: 9, WaitFor: 1})
	host, port := splitHostPort(t, at)

	pods := []pod{
		podAt("lk-litekvd-0", "", nil),
		podAt("lk-litekvd-replica-0", host, nil),
	}
	f, srv, api := newFakeAPI(pods, "lk-litekvd-0")
	defer srv.Close()

	c := against(t, api, "controller-one")
	c.port, c.dryRun, c.held = port, true, now()

	nodes := []node{
		{pod: pods[0], status: nil},
		{pod: pods[1], status: &status{Role: "replica", Term: 1, AppliedSeq: 9, WaitFor: 1}},
	}
	if err := c.failover(t.Context(), nodes, &nodes[0], "unreachable"); err != nil {
		t.Fatal(err)
	}

	if store.promotions() != 0 {
		t.Error("dry run promoted something")
	}
	if f.selector()[podNameLabel] != "lk-litekvd-0" {
		t.Error("dry run moved the write Service")
	}
	if len(f.patchedPods) != 0 {
		t.Errorf("dry run patched pods: %v", f.patchedPods)
	}

	// Enlisting is the other thing it changes about a node, and it has a
	// dry-run branch of its own. A stranded ex-leader must be found exactly as
	// it was left, which is the whole point of watching a policy for a week
	// before turning it on.
	leader := node{pod: podAt("lk-litekvd-0", "10.0.0.9", nil),
		status: &status{Role: "leader", Term: 1, Seq: 9, WaitFor: 1}}
	stranded := node{pod: pods[1], status: &status{Role: "leader", Term: 0, Seq: 3, WaitFor: 1}}

	if err := c.enlist(t.Context(), []node{leader, stranded}, &leader); err != nil {
		t.Fatal(err)
	}
	if told := store.enlistments(); len(told) != 0 {
		t.Errorf("dry run told a stranded leader to follow: %v", told)
	}
}

// TestASlowNodeIsNotAnAnswer. A node that takes longer than the probe timeout
// is a node that did not answer, and must not be ranked as though it had.
func TestASlowNodeIsNotAnAnswer(t *testing.T) {
	_, srv, api := newFakeAPI(nil, "lk-litekvd-0")
	defer srv.Close()

	c := against(t, api, "controller-one")
	c.probeTimeout = 50 * time.Millisecond

	// A port nothing is listening on, which is the fastest way to be sure of a
	// failure that is not a timeout, plus one that is genuinely slow.
	slow, slowAt := newFakeStoreDelayed(t, status{Role: "replica", Term: 1, AppliedSeq: 9}, 2*time.Second)
	host, port := splitHostPort(t, slowAt)
	c.port = port
	_ = slow

	nodes := c.ask(t.Context(), []pod{podAt("lk-litekvd-replica-0", host, nil)})
	if len(nodes) != 1 {
		t.Fatalf("asked one node and got %d back", len(nodes))
	}
	if nodes[0].status != nil {
		t.Error("a node slower than the probe timeout was counted as having answered")
	}
}

// TestAStrandedLeaderIsPutBackToWork.
//
// The cluster used to lose a replica at every failover: the old leader's
// StatefulSet has no -leader flag, so it came back reporting role=leader on an
// old term and there was nothing to be done with it. Three failovers in, nothing
// left to promote.
func TestAStrandedLeaderIsPutBackToWork(t *testing.T) {
	stranded, strandedAt := newFakeStore(t, status{Role: "leader", Term: 0, Seq: 3, WaitFor: 1})
	host, port := splitHostPort(t, strandedAt)

	// All three pods carry the same address on purpose. The controller uses one
	// port for every node, so anything it decides to enlist arrives at this one
	// stand-in — which turns "it enlisted exactly one node" into an assertion
	// instead of a hope. Give the other two an address nothing answers and a
	// controller that enlisted the leader itself, or a replica that was already
	// following, would look identical to one that did neither.
	pods := []pod{
		podAt("lk-litekvd-replica-0", host, nil), // the current leader
		podAt("lk-litekvd-0", host, nil),         // the stranded ex-leader
		podAt("lk-litekvd-replica-1", host, nil), // a replica, already following
	}
	_, srv, api := newFakeAPI(pods, "lk-litekvd-replica-0")
	defer srv.Close()

	c := against(t, api, "controller-one")
	c.port = port

	nodes := []node{
		{pod: pods[0], status: &status{Role: "leader", Term: 1, Seq: 9, WaitFor: 1}},
		{pod: pods[1], status: &status{Role: "leader", Term: 0, Seq: 3, WaitFor: 1}},
		// On a lower term than the leader, which is what a replica looks like
		// between a failover and catching up with it. That is the only state
		// where "it is already a replica" is load-bearing: on the leader's own
		// term the term check would refuse it anyway, and a test that used one
		// would be testing the wrong guard.
		{pod: pods[2], status: &status{Role: "replica", Term: 0, AppliedSeq: 9, WaitFor: 1,
			Leader: "http://lk-litekvd-leader:8080"}},
	}

	if err := c.decide(t.Context(), nodes, "lk-litekvd-replica-0"); err != nil {
		t.Fatal(err)
	}

	// Once, for the one node that needed it. Not the leader — being told to
	// follow itself is precisely the self-follow that empties a store — and not
	// the replica, which is already doing what it would be told to do.
	told := stranded.enlistments()
	if len(told) != 1 {
		t.Fatalf("it enlisted %d nodes, want 1 (the leader and the healthy replica are"+
			" on this address too): %v", len(told), told)
	}

	// The Service, not the leader's pod IP. A pod IP pins it to whoever leads
	// today and strands it again at the next failover — which a cluster
	// demonstrated, leaving a node on term 1 while the leader was on term 3,
	// following a pod that had itself become a replica and was refusing to
	// serve it.
	if want := "http://lk-litekvd-leader:" + strconv.Itoa(port); told[0] != want {
		t.Errorf("it was pointed at %q, want %q", told[0], want)
	}
}

// TestANodeOnTheLeadersTermOrAboveIsLeftAlone. Following is how a newer term
// fences an older one, so enlisting a node with as much claim as the leader
// would take the cluster down rather than heal it. That is somebody's decision,
// not a controller's.
func TestANodeOnTheLeadersTermOrAboveIsLeftAlone(t *testing.T) {
	// Both, because the boundary is the whole rule. Equal is the case that
	// matters more and reads like the safe one: two nodes on term 1, and
	// enlisting either against the other fences a leader that is doing nothing
	// wrong. A sweep found the equal case untested.
	for _, c := range []struct {
		name string
		term uint64
	}{
		{"on the leader's own term", 1},
		{"on a term above the leader's", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			rival, rivalAt := newFakeStore(t, status{Role: "leader", Term: c.term, Seq: 9, WaitFor: 1})
			rivalHost, port := splitHostPort(t, rivalAt)

			pods := []pod{
				podAt("lk-litekvd-replica-0", "10.0.0.9", nil),
				podAt("lk-litekvd-0", rivalHost, nil),
			}
			_, srv, api := newFakeAPI(pods, "lk-litekvd-replica-0")
			defer srv.Close()

			ctl := against(t, api, "controller-one")
			ctl.port = port

			nodes := []node{
				{pod: pods[0], status: &status{Role: "leader", Term: 1, Seq: 9, WaitFor: 1}},
				{pod: pods[1], status: &status{Role: "leader", Term: c.term, Seq: 9, WaitFor: 1}},
			}
			if err := ctl.decide(t.Context(), nodes, "lk-litekvd-replica-0"); err != nil {
				t.Fatal(err)
			}
			if told := rival.enlistments(); len(told) != 0 {
				t.Errorf("a node %s was enlisted against it anyway: %v", c.name, told)
			}
		})
	}
}
