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
	behind, behindAt := newFakeStore(t, status{Role: "replica", Term: 1, AppliedSeq: 4, WaitFor: 1})
	ahead, aheadAt := newFakeStore(t, status{Role: "replica", Term: 1, AppliedSeq: 9, WaitFor: 1})

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
	strandedHost, port := splitHostPort(t, strandedAt)

	pods := []pod{
		podAt("lk-litekvd-replica-0", "10.0.0.9", nil), // the current leader
		podAt("lk-litekvd-0", strandedHost, nil),       // the stranded ex-leader
	}
	_, srv, api := newFakeAPI(pods, "lk-litekvd-replica-0")
	defer srv.Close()

	c := against(t, api, "controller-one")
	c.port = port

	nodes := []node{
		{pod: pods[0], status: &status{Role: "leader", Term: 1, Seq: 9, WaitFor: 1}},
		{pod: pods[1], status: &status{Role: "leader", Term: 0, Seq: 3, WaitFor: 1}},
	}

	if err := c.decide(t.Context(), nodes, "lk-litekvd-replica-0"); err != nil {
		t.Fatal(err)
	}

	told := stranded.enlistments()
	if len(told) != 1 {
		t.Fatalf("the stranded leader was told to follow %d times, want 1: %v", len(told), told)
	}
	if want := "http://10.0.0.9:" + strconv.Itoa(port); told[0] != want {
		t.Errorf("it was pointed at %q, want %q", told[0], want)
	}
}

// TestANodeOnTheLeadersTermOrAboveIsLeftAlone. Following is how a newer term
// fences an older one, so enlisting a node with as much claim as the leader
// would take the cluster down rather than heal it. That is somebody's decision,
// not a controller's.
func TestANodeOnTheLeadersTermOrAboveIsLeftAlone(t *testing.T) {
	rival, rivalAt := newFakeStore(t, status{Role: "leader", Term: 2, Seq: 9, WaitFor: 1})
	rivalHost, port := splitHostPort(t, rivalAt)

	pods := []pod{
		podAt("lk-litekvd-replica-0", "10.0.0.9", nil),
		podAt("lk-litekvd-0", rivalHost, nil),
	}
	_, srv, api := newFakeAPI(pods, "lk-litekvd-replica-0")
	defer srv.Close()

	c := against(t, api, "controller-one")
	c.port = port

	nodes := []node{
		{pod: pods[0], status: &status{Role: "leader", Term: 1, Seq: 9, WaitFor: 1}},
		{pod: pods[1], status: &status{Role: "leader", Term: 2, Seq: 9, WaitFor: 1}},
	}
	if err := c.decide(t.Context(), nodes, "lk-litekvd-replica-0"); err != nil {
		t.Fatal(err)
	}
	if told := rival.enlistments(); len(told) != 0 {
		t.Errorf("a node on a term above the leader's was enlisted anyway: %v", told)
	}
}
