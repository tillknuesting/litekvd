// Command litekv-controller fails a litekvd cluster over, in Kubernetes,
// without a person.
//
// litekvd has no leader election and is not going to grow one: two stores
// raising the term at once puts both on the same term and gives the guarantee
// away, so exactly one thing may decide who leads. In a Kubernetes cluster
// there is already something that can be that one thing. The API server sits on
// a Raft cluster, and a Lease updated with resourceVersion optimistic
// concurrency is a linearizable compare-and-swap. This does not implement
// consensus; it consumes the consensus that is already there.
//
// What it does, once a second:
//
//   - takes or renews a Lease, and does nothing at all if it does not hold it
//   - asks every litekvd pod for /v1/status
//   - if the leader has been unreachable for longer than -grace, picks the
//     replica that got furthest, promotes it, and points the write Service at it
//   - takes a node it believes is stale out of the read Service
//
// # Why this is safe, and exactly how far that goes
//
// The dangerous failure is promoting while the old leader is alive and taking
// writes. What bounds it is semi-synchronous replication: with -wait-for 1 on
// the leader, a write is acknowledged only once a follower has it, so promoting
// a caught-up follower cannot lose an acknowledged write — even if the
// promotion turns out to have been premature.
//
// Without that, automatic failover is a gamble with somebody else's data, and
// this refuses to do it: -require-wait-for is on by default and the controller
// will report and sit still rather than promote a cluster running asynchronous
// replication. Turn it off only if you have decided that losing the last few
// writes is better than waiting for a person.
//
// What it still cannot do is stop the old leader taking writes from a client
// that reaches it without going through the Service. Nothing here can: fencing
// is something a store is told, and the old leader is told only when something
// carrying the newer term talks to it. Traffic is moved, not amputated.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "litekv-controller:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		namespace = flag.String("namespace", envOr("POD_NAMESPACE", ""),
			"the namespace the release is in. Also POD_NAMESPACE, which the chart sets from the downward API")
		release = flag.String("release", envOr("LITEKV_RELEASE", ""),
			"the fullname the chart used, which is what its objects are named after")
		instance = flag.String("instance", envOr("LITEKV_INSTANCE", ""),
			"the app.kubernetes.io/instance the pods carry. Not the same string as -release: one\n"+
				"names the objects and the other labels them, and Helm derives them differently")
		identity = flag.String("identity", envOr("POD_NAME", ""),
			"who this controller is, for the Lease. Two controllers must not share one")

		port = flag.Int("port", 8080,
			"the port litekvd listens on. Must match the chart's service.port; they were allowed to\n"+
				"disagree once, and a controller that cannot reach anything says only that nothing answered")
		interval = flag.Duration("interval", time.Second,
			"how often to look. Polling and not watching: three pods and a Lease is nothing, and it\n"+
				"costs none of the resourceVersion expiry, bookmarks and relist a watch would")
		grace = flag.Duration("grace", 15*time.Second,
			"how long the leader must be unreachable before it is failed over. The single most important\n"+
				"number here: too short and a paused GC or a rolling restart becomes a failover, too long and\n"+
				"an outage lasts that much longer. It must be comfortably above the leader's own restart time")
		timeout = flag.Duration("probe-timeout", 2*time.Second,
			"how long a single /v1/status may take before that pod counts as unreachable")

		leaseName = flag.String("lease", "",
			"the Lease to hold, so that two controllers cannot both promote (empty means <release>-controller)")
		leaseDuration = flag.Duration("lease-duration", 15*time.Second,
			"how long a Lease is honoured after its last renewal. A controller that cannot renew must stop\n"+
				"acting before this runs out, or two of them overlap")

		requireWaitFor = flag.Bool("require-wait-for", true,
			"refuse to fail over unless the leader is running -wait-for 1 or more. Without it an\n"+
				"acknowledged write may exist only on the node that just died, and promoting anything\n"+
				"loses it silently. Turning this off is a decision about somebody's data")
		dryRun = flag.Bool("dry-run", false,
			"decide everything and change nothing, saying what it would have done. The way to watch a\n"+
				"grace period and a candidate choice before trusting either")

		tokenFile = flag.String("token-file", "",
			"file holding litekvd's bearer token, when the cluster has one set")
		verbose = flag.Bool("verbose", false, "log every poll rather than only decisions")
	)
	flag.Parse()

	if *namespace == "" {
		return errors.New("-namespace is required (the chart sets POD_NAMESPACE)")
	}
	if *release == "" {
		return errors.New("-release is required (the chart sets LITEKV_RELEASE)")
	}
	if *identity == "" {
		return errors.New("-identity is required (the chart sets POD_NAME)")
	}
	if *instance == "" {
		return errors.New("-instance is required (the chart sets LITEKV_INSTANCE)")
	}
	if *leaseName == "" {
		*leaseName = *release + "-controller"
	}
	if *grace <= *interval {
		return fmt.Errorf("-grace (%s) must be longer than -interval (%s)", *grace, *interval)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	api, err := inCluster()
	if err != nil {
		return err
	}

	var storeToken string
	if *tokenFile != "" {
		raw, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("reading litekvd's token: %w", err)
		}
		storeToken = strings.TrimSpace(string(raw))
	}

	c := &controller{
		api:       api,
		log:       log,
		namespace: *namespace,
		release:   *release,
		instance:  *instance,
		identity:  *identity,
		token:     storeToken,

		port:           *port,
		interval:       *interval,
		grace:          *grace,
		probeTimeout:   *timeout,
		lease:          *leaseName,
		leaseDuration:  *leaseDuration,
		requireWaitFor: *requireWaitFor,
		dryRun:         *dryRun,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("watching", "namespace", *namespace, "release", *release, "instance", *instance,
		"identity", *identity, "grace", grace.String(), "dry-run", *dryRun,
		"require-wait-for", *requireWaitFor)

	return c.run(ctx)
}

// envOr is the environment variable, or the fallback when it is unset.
func envOr(name, fallback string) string {
	if set := os.Getenv(name); set != "" {
		return set
	}
	return fallback
}
