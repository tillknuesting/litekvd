package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const (
	// The label Kubernetes puts on every StatefulSet pod. The write Service
	// selects on it, so promotion is a matter of changing which name it holds —
	// and a name can only ever mean one pod, which a role label could not.
	podNameLabel = "statefulset.kubernetes.io/pod-name"

	// Removed from a node this controller believes is stale, which takes it out
	// of the read Service. It is the only thing that stops a returning old
	// leader answering reads out of a history that diverged.
	servingLabel = "litekv.io/serving"
)

type controller struct {
	api *kube
	log *slog.Logger

	namespace string
	release   string
	instance  string
	identity  string
	token     string

	// port is where litekvd listens, which the chart lets you change. It was
	// hardcoded to 8080 here while values.yaml offered service.port, so any
	// cluster on another port had a controller that could reach nothing and
	// said only "no answer" about every node.
	port int

	interval       time.Duration
	grace          time.Duration
	probeTimeout   time.Duration
	lease          string
	leaseDuration  time.Duration
	requireWaitFor bool
	dryRun         bool

	// held is when this controller last knew it had the Lease. Nothing is
	// changed unless it is recent, so a controller that has been partitioned
	// from the API server stops acting before its Lease could be handed on.
	held time.Time

	// seenVersion and seenAt are the last resourceVersion this controller saw on
	// the Lease and when it saw it, by its own clock. See stale.
	seenVersion string
	seenAt      time.Time

	// unreachableSince is when the leader stopped answering. Reset the moment
	// it answers again, which is what makes -grace a run of failures rather
	// than an aggregate of them.
	unreachableSince time.Time
}

// status is what a litekvd node says about itself.
type status struct {
	Role       string `json:"role"`
	Term       uint64 `json:"term"`
	Leader     string `json:"leader"`
	Fenced     bool   `json:"fenced"`
	Seq        uint64 `json:"seq"`
	AppliedSeq uint64 `json:"applied_seq"`
	WaitFor    int    `json:"wait_for"`
	Keys       int    `json:"keys"`
}

// node is a pod and whatever it managed to say.
type node struct {
	pod    pod
	status *status // nil when it did not answer
}

// further reports whether a got further through the leader's records than b.
//
// (term, seq), which is the comparison acks.go uses to decide whether a
// follower has reached a write. A replica is ranked by what it has applied,
// since that is how far through the leader's history it took; a node with no
// applied position has taken nothing.
func further(a, b *status) bool {
	if a.Term != b.Term {
		return a.Term > b.Term
	}
	return a.AppliedSeq > b.AppliedSeq
}

func (c *controller) run(ctx context.Context) error {
	tick := time.NewTicker(c.interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			c.dropLease(context.WithoutCancel(ctx))
			return nil
		case <-tick.C:
		}

		if err := c.once(ctx); err != nil && !errors.Is(err, context.Canceled) {
			// Logged and not returned. A controller that exited on a failed
			// poll would be a controller that is not there the one time the API
			// server hiccups, which is exactly when something is wrong.
			c.log.Error("this round did nothing", "err", err)
		}
	}
}

// once is a single look at the world, and at most one decision about it.
func (c *controller) once(ctx context.Context) error {
	holding, err := c.acquire(ctx)
	if err != nil {
		return fmt.Errorf("the lease: %w", err)
	}
	if !holding {
		c.log.Debug("another controller holds the lease; standing by")
		return nil
	}

	pods, err := c.api.pods(ctx, c.namespace,
		"app.kubernetes.io/instance="+c.instance+",app.kubernetes.io/name=litekvd")
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}
	if len(pods) == 0 {
		return errors.New("no litekvd pods; is -release right?")
	}

	nodes := c.ask(ctx, pods)

	svc, err := c.api.service(ctx, c.namespace, c.release+"-leader")
	if err != nil {
		return fmt.Errorf("reading the write Service: %w", err)
	}
	pointedAt := svc.Spec.Selector[podNameLabel]

	return c.decide(ctx, nodes, pointedAt)
}

// ask polls every pod for its status, in parallel, and tolerates silence.
func (c *controller) ask(ctx context.Context, pods []pod) []node {
	nodes := make([]node, len(pods))
	done := make(chan struct{})

	for i, p := range pods {
		go func() {
			defer func() { done <- struct{}{} }()

			nodes[i] = node{pod: p}
			if p.Status.PodIP == "" {
				return
			}

			asked, cancel := context.WithTimeout(ctx, c.probeTimeout)
			defer cancel()

			if s, err := c.status(asked, p.Status.PodIP); err == nil {
				nodes[i].status = s
			} else {
				c.log.Debug("no answer", "pod", p.Metadata.Name, "err", err)
			}
		}()
	}
	for range pods {
		<-done
	}
	return nodes
}

func (c *controller) status(ctx context.Context, ip string) (*status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://%s:%d/v1/status", ip, c.port), nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status said %s", resp.Status)
	}

	var s status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

// decide is the whole of the policy, and it is deliberately reluctant.
func (c *controller) decide(ctx context.Context, nodes []node, pointedAt string) error {
	var leader *node
	for i := range nodes {
		if nodes[i].pod.Metadata.Name == pointedAt {
			leader = &nodes[i]
		}
	}

	// The write Service points at a pod that is not in the list at all.
	//
	// The first version of this refused, on the grounds that somebody must have
	// renamed something — and it was wrong in the way that only a drill shows:
	// the commonest reason a leader pod is missing is that it died, which is
	// precisely the case this exists for. A pod that is gone is at least as
	// unreachable as one that will not answer, so it is treated the same and
	// the grace period runs as usual.
	//
	// The confusing case it was written for is still handled, just further
	// down: if nothing else answers, choose refuses and says so.
	if leader == nil {
		leader = &node{}
		leader.pod.Metadata.Name = pointedAt
	}

	// The node the write Service names is up, and is not a leader.
	//
	// That is a write outage with a healthy pod in the middle of it, and it has
	// one cause worth knowing: a promoted node whose pod restarted comes back
	// with its -leader still pointing at the Service, which now names itself.
	// It follows itself, refuses every write with 409, and looks entirely well
	// from outside. litekvd refuses to serve replication to itself so the store
	// survives, but nothing puts the role back.
	//
	// The answer is to promote it rather than to fail over: the Service naming
	// it is the statement that it should be the leader, and it holds the data.
	if leader.status != nil && leader.status.Role != "leader" {
		c.unreachableSince = time.Time{}
		if c.dryRun {
			c.log.Warn("would re-promote the write target, which is following instead of leading",
				"pod", leader.pod.Metadata.Name, "following", leader.status.Leader)
			return nil
		}
		c.log.Warn("the write target is following instead of leading; promoting it",
			"pod", leader.pod.Metadata.Name, "following", leader.status.Leader,
			"term", leader.status.Term)
		if err := c.promote(ctx, leader.pod.Status.PodIP); err != nil {
			return fmt.Errorf("re-promoting %s: %w", leader.pod.Metadata.Name, err)
		}
		return nil
	}

	if leader.status != nil && !leader.status.Fenced {
		if !c.unreachableSince.IsZero() {
			c.log.Info("the leader is answering again", "pod", leader.pod.Metadata.Name,
				"was silent for", time.Since(c.unreachableSince).Round(time.Second).String())
			c.unreachableSince = time.Time{}
		}
		return c.tidy(ctx, nodes, pointedAt)
	}

	// It is either silent or has been fenced. A fenced leader is not a
	// judgement call at all: it has been told by something carrying a newer
	// term that it is finished, so there is nothing to wait out.
	why := "unreachable"
	switch {
	case leader.status != nil && leader.status.Fenced:
		why = "fenced"
	case leader.pod.Status.PodIP == "" && leader.pod.Metadata.Labels == nil:
		why = "gone"
	}

	if c.unreachableSince.IsZero() {
		c.unreachableSince = time.Now()
		c.log.Warn("the leader stopped being the leader", "pod", leader.pod.Metadata.Name,
			"why", why, "grace", c.grace.String())
		return nil
	}
	if waited := time.Since(c.unreachableSince); waited < c.grace && why != "fenced" {
		c.log.Debug("still within the grace period", "waited", waited.Round(time.Second).String())
		return nil
	}

	return c.failover(ctx, nodes, leader, why)
}

// choose is the decision, with nothing done about it.
//
// Separated from the acting so that it can be tested, which is not a
// stylistic preference: the first version decided and acted in one function,
// and its tests passed with the semi-synchronous guard deleted. A test that
// cannot see the decision is a test of the plumbing.
//
// It returns the node to promote, or nil and the reason it will not.
func (c *controller) choose(nodes []node, old *node) (*node, string) {
	var best *node
	for i := range nodes {
		n := &nodes[i]
		if n.status == nil || n.pod.Metadata.Name == old.pod.Metadata.Name {
			continue
		}
		if !n.pod.ready() || n.status.Fenced {
			continue
		}
		if best == nil || further(n.status, best.status) {
			best = n
		}
	}

	// Somebody has to be able to see the cluster. A controller that can reach
	// nothing is far more likely to be the thing that is broken than to be the
	// last witness of everything else breaking, and promoting on no evidence is
	// how a network blip becomes a split history.
	if best == nil {
		return nil, "no replica answered"
	}

	// The safety argument, and it is the only one this has. With -wait-for on
	// the leader, an acknowledged write was on a follower before the client
	// heard 204, so promoting a caught-up follower cannot lose one. Without it
	// the last writes may exist only on the node that just died, and promoting
	// anything at all loses them without saying so.
	if c.requireWaitFor && !c.sawSemiSync(nodes) {
		return nil, "the cluster is replicating asynchronously, so an acknowledged write may " +
			"exist only on the node that has gone. Set config.waitFor, or -require-wait-for=false"
	}

	// The lease was taken at the top of this round, but the round has since
	// polled every pod and each of those could have waited out -probe-timeout.
	// Acting on a lease most of the way to expiring is how two controllers come
	// to overlap: this one still believes it holds it while the next has taken
	// it over. Half the duration is the margin.
	if since := time.Since(c.held); since > c.leaseDuration/2 {
		return nil, "this round took " + since.Round(time.Millisecond).String() +
			" and its lease is no longer fresh enough to act on"
	}

	return best, ""
}

// failover promotes what choose picked, and moves the traffic to it.
func (c *controller) failover(ctx context.Context, nodes []node, old *node, why string) error {
	best, refused := c.choose(nodes, old)
	if best == nil {
		c.log.Error("not failing over", "leader", old.pod.Metadata.Name, "why", why,
			"refused because", refused)
		return nil
	}

	c.log.Warn("failing over", "from", old.pod.Metadata.Name, "to", best.pod.Metadata.Name,
		"why", why, "candidate term", best.status.Term, "candidate applied", best.status.AppliedSeq,
		"dry-run", c.dryRun)

	if c.dryRun {
		return nil
	}

	// Promote first, then move the traffic. The other order sends writes to a
	// node that is still a replica and answers every one of them 409.
	if err := c.promote(ctx, best.pod.Status.PodIP); err != nil {
		return fmt.Errorf("promoting %s: %w", best.pod.Metadata.Name, err)
	}
	if err := c.api.point(ctx, c.namespace, c.release+"-leader",
		podNameLabel, best.pod.Metadata.Name); err != nil {
		return fmt.Errorf("pointing the write Service at %s: %w", best.pod.Metadata.Name, err)
	}

	// And take the old one out of the read path. It does not know it has been
	// replaced — nothing has told it — so it will go on serving reads out of a
	// history that stopped being the history at the moment above.
	if err := c.api.label(ctx, c.namespace, old.pod.Metadata.Name, servingLabel, ""); err != nil {
		c.log.Error("could not take the old leader out of the read Service",
			"pod", old.pod.Metadata.Name, "err", err)
	}

	c.unreachableSince = time.Time{}
	c.log.Warn("failed over", "leader", best.pod.Metadata.Name, "term", best.status.Term+1)
	return nil
}

// sawSemiSync reports whether any node is running with a follower requirement.
//
// Asked of the nodes rather than of the values file, because what matters is
// what the processes were started with and a chart can be edited without them
// being restarted. A leader that is gone cannot be asked, which is why this
// takes the answer from whichever node still answers: they are configured
// together by the chart, and a replica carries the same flag for exactly this
// reason — it is the one that will need it.
func (c *controller) sawSemiSync(nodes []node) bool {
	for i := range nodes {
		if nodes[i].status != nil && nodes[i].status.WaitFor > 0 {
			return true
		}
	}
	return false
}

func (c *controller) promote(ctx context.Context, ip string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://%s:%d/v1/promote", ip, c.port), nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("promote said %s", resp.Status)
	}
	return nil
}

// tidy keeps the read Service honest, every round.
//
// Two things can be wrong with it and both are about a node that thinks it is a
// leader and is not. The obvious one is the node just failed away from. The one
// that took a drill to find is the same node coming *back*: its StatefulSet
// recreates the pod, the template stamps the serving label on, and it rejoins
// the read Service still holding the history that stopped being the history at
// the promotion. Nothing has told it — fencing is something a store is told —
// so it reports role=leader on the old term and looks perfectly well.
//
// So this is not "undo what the failover did". It is a rule enforced on every
// healthy round: a node serves reads if it is the leader the write Service
// names, or if it is following. Anything else is out.
func (c *controller) tidy(ctx context.Context, nodes []node, leaderName string) error {
	for i := range nodes {
		n := &nodes[i]
		if n.status == nil || !n.pod.ready() {
			continue
		}

		_, serving := n.pod.Metadata.Labels[servingLabel]
		should := n.pod.Metadata.Name == leaderName || n.status.Role == "replica"

		if should == serving {
			continue
		}
		if c.dryRun {
			c.log.Info("would change what serves reads", "pod", n.pod.Metadata.Name,
				"serving", should, "role", n.status.Role)
			continue
		}

		value := ""
		if should {
			value = "true"
		}
		if err := c.api.label(ctx, c.namespace, n.pod.Metadata.Name, servingLabel, value); err != nil {
			return fmt.Errorf("changing what %s serves: %w", n.pod.Metadata.Name, err)
		}

		if should {
			c.log.Info("back in the read Service", "pod", n.pod.Metadata.Name, "role", n.status.Role)
		} else {
			c.log.Warn("out of the read Service: it thinks it is a leader and is not",
				"pod", n.pod.Metadata.Name, "term", n.status.Term, "leader is", leaderName)
		}
	}
	return nil
}
