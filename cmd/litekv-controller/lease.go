package main

import (
	"context"
	"errors"
	"time"
)

// Mutual exclusion between controllers, which matters more than it looks.
//
// Two controllers that both decide the leader is gone will both promote — a
// different replica each, most likely — and the cluster ends with two stores on
// different terms and two histories. That is the failure the whole design is
// arranged to prevent, arranged instead by the thing meant to prevent it.
//
// One controller replica is not enough to rule that out, because a Deployment
// rolling over runs two pods for a few seconds, and that is exactly the moment
// somebody is restarting things and pods are unreachable.
//
// So: a Lease, held by name, renewed every round, and a compare-and-swap on
// resourceVersion for every write to it. The API server's etcd is a Raft
// cluster, so that CAS is linearizable — either this controller's write applied
// to the Lease it read, or it did not apply at all.

// microTime is how the API server insists a Lease's times are written.
//
// A Lease carries metav1.MicroTime, not a plain RFC 3339 timestamp, and it is
// decoded with exactly six decimal places. RFC3339Nano looks right and is not:
// it drops trailing zeros, so about one timestamp in ten is a 400 saying
// `cannot parse "505Z" as "Z07:00"` — and the nine in ten that work make it
// look like an intermittent fault rather than a format error.
const microTime = "2006-01-02T15:04:05.000000Z07:00"

// acquire takes the Lease, renews it, or reports that somebody else holds it.
func (c *controller) acquire(ctx context.Context) (bool, error) {
	now := time.Now()
	want := leaseSpec{
		HolderIdentity:       c.identity,
		LeaseDurationSeconds: int(c.leaseDuration.Seconds()),
		RenewTime:            now.UTC().Format(microTime),
		AcquireTime:          now.UTC().Format(microTime),
	}

	current, err := c.api.getLease(ctx, c.namespace, c.lease)
	switch {
	case errors.Is(err, errNotFound):
		// Nobody has ever held it. Creating is itself the race: two
		// controllers both creating means one gets a 409 and comes back round
		// to find the other's.
		if err := c.api.createLease(ctx, c.namespace, c.lease, want); err != nil {
			if errors.Is(err, errConflict) {
				return false, nil
			}
			return false, err
		}
		c.held = now
		c.log.Info("took the lease", "lease", c.lease)
		return true, nil

	case err != nil:
		// Could not even read it. This is the case that must not be read as
		// "so I hold it": a controller cut off from the API server is a
		// controller that cannot promote anything anyway, and one that assumed
		// the lease while another renewed it would be the second promoter.
		return false, err
	}

	mine := current.Spec.HolderIdentity == c.identity
	expired := true
	if renewed, perr := time.Parse(microTime, current.Spec.RenewTime); perr == nil {
		expired = now.Sub(renewed) > time.Duration(current.Spec.LeaseDurationSeconds)*time.Second
	}

	if !mine && !expired {
		return false, nil
	}
	if mine {
		// Keep the original acquire time, so that the Lease reads as one
		// continuous holding rather than a new one every second.
		want.AcquireTime = current.Spec.AcquireTime
	}

	if err := c.api.updateLease(ctx, c.namespace, current, want); err != nil {
		if errors.Is(err, errConflict) {
			// Somebody wrote it between the read and the write. They hold it;
			// this round does nothing. That is the compare-and-swap doing its
			// job rather than a failure.
			return false, nil
		}
		return false, err
	}

	if !mine {
		c.log.Info("took over an expired lease", "lease", c.lease,
			"from", current.Spec.HolderIdentity)
	}
	c.held = now
	return true, nil
}

// dropLease gives the Lease back on the way out, so that another controller can
// take over in the next second rather than in the next lease-duration.
//
// Best effort by design: a controller being killed has no business blocking on
// the API server, and the expiry above is what makes this an optimisation
// rather than a requirement.
func (c *controller) dropLease(ctx context.Context) {
	if c.held.IsZero() {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	current, err := c.api.getLease(ctx, c.namespace, c.lease)
	if err != nil || current.Spec.HolderIdentity != c.identity {
		return
	}

	// Renewed far enough in the past to be expired the moment it is read.
	spec := current.Spec
	spec.RenewTime = time.Now().Add(-2 * c.leaseDuration).UTC().Format(microTime)
	if err := c.api.updateLease(ctx, c.namespace, current, spec); err != nil {
		return
	}
	c.log.Info("gave the lease back", "lease", c.lease)
}
