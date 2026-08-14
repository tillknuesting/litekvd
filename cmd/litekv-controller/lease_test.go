package main

import (
	"testing"
	"time"
)

// against builds a controller wired to a stand-in API server.
func against(t *testing.T, api *kube, identity string) *controller {
	t.Helper()

	return &controller{
		api: api, log: quiet(),
		namespace: "litekv", release: "lk-litekvd", instance: "lk",
		identity: identity, lease: "lk-litekvd-controller",
		leaseDuration: 15 * time.Second, grace: 15 * time.Second,
		probeTimeout: time.Second, port: 8080,
	}
}

// TestTwoControllersAndOnlyOneActs.
//
// The property the whole Lease exists for, tested rather than assumed. Two
// controllers, one API server that enforces resourceVersion the way etcd does,
// and a hundred rounds of both trying: at no point may both come away believing
// they hold it, because both believing it means both promoting, and both
// promoting means two stores on two terms with two histories.
func TestTwoControllersAndOnlyOneActs(t *testing.T) {
	f, srv, api := newFakeAPI(nil, "lk-litekvd-0")
	defer srv.Close()

	one, two := against(t, api, "controller-one"), against(t, api, "controller-two")

	both := 0
	for range 100 {
		gotOne, err := one.acquire(t.Context())
		if err != nil {
			t.Fatalf("controller one: %v", err)
		}
		gotTwo, err := two.acquire(t.Context())
		if err != nil {
			t.Fatalf("controller two: %v", err)
		}
		if gotOne && gotTwo {
			both++
		}
	}

	if both > 0 {
		t.Fatalf("both controllers held the lease on %d of 100 rounds", both)
	}
	if f.lease == nil {
		t.Fatal("nobody ever took the lease; the test proved nothing")
	}
	if f.lease.Spec.HolderIdentity != "controller-one" {
		t.Errorf("the holder is %q, and it should have stayed with the one that took it first",
			f.lease.Spec.HolderIdentity)
	}
}

// TestTheLeaseIsWrittenInTheFormatTheAPIServerWants.
//
// A Lease carries metav1.MicroTime and is decoded with exactly six decimal
// places. RFC3339Nano drops trailing zeros, so roughly one write in ten is a
// 400 — which reads as an intermittent fault rather than a format error, and
// cost an afternoon. The stand-in rejects the wrong shape the way the real one
// does.
func TestTheLeaseIsWrittenInTheFormatTheAPIServerWants(t *testing.T) {
	_, srv, api := newFakeAPI(nil, "lk-litekvd-0")
	defer srv.Close()

	c := against(t, api, "controller-one")

	// Enough rounds that a format which is usually right would be caught: a
	// timestamp ending in a zero is one in ten, so fifty is not a coincidence.
	for i := range 50 {
		held, err := c.acquire(t.Context())
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if !held {
			t.Fatalf("round %d: it stopped holding its own lease", i)
		}
	}
}

// TestAConflictIsNotAFailure. Somebody else wrote the Lease between this
// controller reading it and writing it back. That is the compare-and-swap doing
// its job: this round does nothing and comes back, rather than erroring.
func TestAConflictIsNotAFailure(t *testing.T) {
	f, srv, api := newFakeAPI(nil, "lk-litekvd-0")
	defer srv.Close()

	one, two := against(t, api, "controller-one"), against(t, api, "controller-two")

	if held, err := one.acquire(t.Context()); err != nil || !held {
		t.Fatalf("the first controller did not take a free lease: %v %v", held, err)
	}

	// The second sees it held and stands by, without an error.
	held, err := two.acquire(t.Context())
	if err != nil {
		t.Fatalf("standing by was an error: %v", err)
	}
	if held {
		t.Fatal("the second controller took a lease the first holds")
	}
	if f.conflicts != 0 {
		t.Errorf("it wrote to a lease it could see was held: %d conflicts", f.conflicts)
	}
}

// TestAnAbandonedLeaseIsTakenOverAfterALeaseDuration.
//
// And not before. The first sight of a Lease is never stale — a controller that
// has just started knows nothing about how long the holder has been quiet, and
// guessing from a timestamp somebody else's clock wrote is the bug this design
// avoids.
func TestAnAbandonedLeaseIsTakenOverAfterALeaseDuration(t *testing.T) {
	f, srv, api := newFakeAPI(nil, "lk-litekvd-0")
	defer srv.Close()

	gone := against(t, api, "the-one-that-died")
	if held, _ := gone.acquire(t.Context()); !held {
		t.Fatal("setup: the first controller did not take the lease")
	}

	// A fresh controller arrives. It must not seize the lease on sight.
	fresh := against(t, api, "the-new-one")
	if held, err := fresh.acquire(t.Context()); err != nil || held {
		t.Fatalf("a starting controller seized a lease it had never seen before: %v %v", held, err)
	}

	// Nothing renews it, and its resourceVersion stops moving. Once a full
	// duration of silence has passed on this controller's own clock, it is
	// free.
	fresh.seenAt = fresh.seenAt.Add(-2 * fresh.leaseDuration)
	if held, err := fresh.acquire(t.Context()); err != nil || !held {
		t.Fatalf("an abandoned lease was never taken over: %v %v", held, err)
	}
	if f.lease.Spec.HolderIdentity != "the-new-one" {
		t.Errorf("the holder is %q", f.lease.Spec.HolderIdentity)
	}
}

// TestAControllerThatCannotReachTheAPIServerDoesNotAssumeItHoldsTheLease.
//
// The failure that must never be read as permission. A controller cut off from
// the API server cannot promote anything anyway; one that treated the silence
// as "so I hold it" would be the second promoter the moment the partition
// healed.
func TestAControllerThatCannotReachTheAPIServerDoesNotAssumeItHoldsTheLease(t *testing.T) {
	_, srv, api := newFakeAPI(nil, "lk-litekvd-0")
	c := against(t, api, "controller-one")

	if held, _ := c.acquire(t.Context()); !held {
		t.Fatal("setup: it did not take a free lease")
	}

	srv.Close() // the API server goes away

	held, err := c.acquire(t.Context())
	if held {
		t.Fatal("it decided it held the lease while unable to reach the API server")
	}
	if err == nil {
		t.Error("it lost the API server and reported no error")
	}
}

// TestARaceOnTheLeaseIsResolvedByTheAPIServer.
//
// The tests above never reach the compare-and-swap: a controller that can see
// the Lease is held stands by without writing, so the 409 path was unexercised
// and a mutation making a conflict an error survived.
//
// This is the real race. Two controllers both believe the Lease is free — which
// is exactly what happens when a holder dies and both survivors notice at the
// same instant — and both write. The API server picks one. The loser must come
// away with "somebody else holds it" and not with an error, because an error is
// logged and retried while a conflict is the mechanism working.
func TestARaceOnTheLeaseIsResolvedByTheAPIServer(t *testing.T) {
	f, srv, api := newFakeAPI(nil, "lk-litekvd-0")
	defer srv.Close()

	// Somebody held it and stopped.
	gone := against(t, api, "the-one-that-died")
	if held, _ := gone.acquire(t.Context()); !held {
		t.Fatal("setup: the lease was not taken")
	}

	two, three := against(t, api, "controller-two"), against(t, api, "controller-three")

	both, failures := 0, 0
	for range 60 {
		// Both have seen it, and both have been watching it not change for
		// longer than a lease duration. Both will try to take it.
		for _, c := range []*controller{two, three} {
			c.seenVersion = f.lease.Metadata.ResourceVersion
			c.seenAt = time.Now().Add(-2 * c.leaseDuration)
		}

		var gotTwo, gotThree bool
		var errTwo, errThree error
		done := make(chan struct{}, 2)
		go func() { gotTwo, errTwo = two.acquire(t.Context()); done <- struct{}{} }()
		go func() { gotThree, errThree = three.acquire(t.Context()); done <- struct{}{} }()
		<-done
		<-done

		if errTwo != nil || errThree != nil {
			failures++
		}
		if gotTwo && gotThree {
			both++
		}
	}

	if both > 0 {
		t.Errorf("both controllers took the lease on %d of 60 races", both)
	}
	if failures > 0 {
		t.Errorf("a lost race was reported as an error %d times; it is the CAS working", failures)
	}
	if f.conflicts == 0 {
		t.Error("no write ever conflicted, so this did not test the compare-and-swap")
	}
}
