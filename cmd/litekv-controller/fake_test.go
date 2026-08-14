package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Kubernetes API server, in about as much detail as this controller can tell.
//
// The policy tests above run against structs and never touch the network, which
// is right for deciding but says nothing about the half of this program that
// changes a cluster. That half is four HTTP calls, and every one of them has a
// shape the API server will reject if it is wrong — a Lease whose timestamps
// have the wrong number of decimal places, a patch that replaces a selector
// instead of merging into it, a compare-and-swap that is not one.
//
// So: a stand-in that behaves like the real thing where this program can tell
// the difference, and in particular one that enforces resourceVersion on writes.
// That is what makes "two controllers, only one acts" a test rather than a hope.
type fakeAPI struct {
	mu sync.Mutex

	lease   *lease // nil until somebody creates it
	version int    // bumped on every write, as etcd does

	pods []pod
	svc  service

	// What was asked of it, in order, for a test to assert on.
	patchedPods     []string
	patchedServices []string
	conflicts       int
}

func newFakeAPI(pods []pod, leaderPod string) (*fakeAPI, *httptest.Server, *kube) {
	f := &fakeAPI{pods: pods}
	f.svc.Spec.Selector = map[string]string{
		"app.kubernetes.io/name": "litekvd",
		podNameLabel:             leaderPod,
	}

	srv := httptest.NewServer(f)
	return f, srv, &kube{host: srv.URL, client: srv.Client()}
}

func (f *fakeAPI) bump() string {
	f.version++
	return strconv.Itoa(f.version)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := r.URL.Path
	switch {
	case strings.Contains(path, "/leases"):
		f.leases(w, r)
	case strings.Contains(path, "/pods"):
		f.podRoutes(w, r)
	case strings.Contains(path, "/services"):
		f.services(w, r)
	default:
		http.Error(w, "no such route: "+path, http.StatusNotFound)
	}
}

func (f *fakeAPI) leases(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if f.lease == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(f.lease)

	case http.MethodPost:
		if f.lease != nil {
			f.conflicts++
			http.Error(w, "already exists", http.StatusConflict)
			return
		}
		var body struct {
			Spec leaseSpec `json:"spec"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if !f.timesAreValid(w, body.Spec) {
			return
		}
		l := &lease{Spec: body.Spec}
		l.Metadata.Name = "lease"
		l.Metadata.ResourceVersion = f.bump()
		f.lease = l
		json.NewEncoder(w).Encode(l)

	case http.MethodPut:
		var body struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
			Spec leaseSpec `json:"spec"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if f.lease == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// The compare-and-swap. This is the whole reason the fake exists: a
		// write that does not carry the version it read is refused, exactly as
		// etcd would refuse it.
		if body.Metadata.ResourceVersion != f.lease.Metadata.ResourceVersion {
			f.conflicts++
			http.Error(w, "the object has been modified", http.StatusConflict)
			return
		}
		if !f.timesAreValid(w, body.Spec) {
			return
		}
		f.lease.Spec = body.Spec
		f.lease.Metadata.ResourceVersion = f.bump()
		json.NewEncoder(w).Encode(f.lease)

	default:
		http.Error(w, "no", http.StatusMethodNotAllowed)
	}
}

// timesAreValid rejects a Lease whose times are not metav1.MicroTime, which is
// what the real API server does — and what it did, at some length, to the first
// version of this controller.
func (f *fakeAPI) timesAreValid(w http.ResponseWriter, spec leaseSpec) bool {
	for _, at := range []string{spec.RenewTime, spec.AcquireTime} {
		if at == "" {
			continue
		}
		// Exactly six decimal places and a Z: "2026-08-13T17:13:34.605133Z".
		dot := strings.IndexByte(at, '.')
		if dot < 0 || len(at)-dot != 8 || !strings.HasSuffix(at, "Z") {
			http.Error(w, fmt.Sprintf(
				"Lease cannot be handled: parsing time %q as MicroTime", at), http.StatusBadRequest)
			return false
		}
	}
	return true
}

func (f *fakeAPI) podRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(podList{Items: f.pods})
		return
	}

	// A merge patch of labels: a null removes one, a string sets it.
	name := path.Base(r.URL.Path)
	var body struct {
		Metadata struct {
			Labels map[string]*string `json:"labels"`
		} `json:"metadata"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	for i := range f.pods {
		if f.pods[i].Metadata.Name != name {
			continue
		}
		if f.pods[i].Metadata.Labels == nil {
			f.pods[i].Metadata.Labels = map[string]string{}
		}
		for k, v := range body.Metadata.Labels {
			if v == nil {
				delete(f.pods[i].Metadata.Labels, k)
				f.patchedPods = append(f.patchedPods, name+" -"+k)
			} else {
				f.pods[i].Metadata.Labels[k] = *v
				f.patchedPods = append(f.patchedPods, name+" +"+k+"="+*v)
			}
		}
		json.NewEncoder(w).Encode(f.pods[i])
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (f *fakeAPI) services(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(f.svc)
		return
	}

	var body struct {
		Spec struct {
			Selector map[string]string `json:"selector"`
		} `json:"spec"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	// Merged, not replaced. A patch that replaced the selector would drop the
	// app labels and the Service would start selecting the whole namespace.
	for k, v := range body.Spec.Selector {
		f.svc.Spec.Selector[k] = v
		f.patchedServices = append(f.patchedServices, k+"="+v)
	}
	json.NewEncoder(w).Encode(f.svc)
}

func (f *fakeAPI) selector() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := map[string]string{}
	maps.Copy(out, f.svc.Spec.Selector)
	return out
}

func (f *fakeAPI) labelsOf(name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, p := range f.pods {
		if p.Metadata.Name == name {
			out := map[string]string{}
			maps.Copy(out, p.Metadata.Labels)
			return out
		}
	}
	return nil
}

// A litekvd, in as much detail as the controller asks for: a status and a
// promotion that raises the term.
type fakeStore struct {
	mu     sync.Mutex
	status status

	// Every leader it was told to follow, in order.
	followed []string

	// Every address a promotion was addressed to, in order.
	//
	// The count alone cannot say which node was promoted: every stand-in is on
	// 127.0.0.1 and the controller uses one port for all of them, so a mutation
	// that promotes the wrong node reaches the same listener and the count is
	// identical. An empty pod IP does not separate them either — Go reads
	// http://:8080 as localhost. The Host header does separate them, so that is
	// what is recorded.
	promotedTo []string
}

func newFakeStore(t *testing.T, s status) (*fakeStore, string) {
	t.Helper()

	f := &fakeStore{status: s}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/follow"):
			f.followed = append(f.followed, r.URL.Query().Get("leader"))
			f.status.Role = "replica"
			f.status.Leader = r.URL.Query().Get("leader")
			json.NewEncoder(w).Encode(f.status)

		case strings.HasSuffix(r.URL.Path, "/v1/promote"):
			f.promotedTo = append(f.promotedTo, r.Host)
			f.status.Role = "leader"
			f.status.Term++
			f.status.Leader = ""
			json.NewEncoder(w).Encode(map[string]uint64{"term": f.status.Term})
		default:
			json.NewEncoder(w).Encode(f.status)
		}
	}))
	t.Cleanup(srv.Close)

	// The controller builds http://<ip>:<port>, so the "pod IP" is the host and
	// the port is what it was told litekvd listens on.
	at, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return f, at.Host
}

// splitHostPort is host and port from a "127.0.0.1:54321" the test server gave.
func splitHostPort(t *testing.T, hostport string) (string, int) {
	t.Helper()

	host, port, found := strings.Cut(hostport, ":")
	if !found {
		t.Fatalf("no port in %q", hostport)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return host, n
}

// enlistments is what this node was told to follow, in order.
func (f *fakeStore) enlistments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.followed...)
}

func (f *fakeStore) promotions() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.promotedTo)
}

// promotionsFrom is how many promotions were addressed to a particular host,
// which is the only way to tell which node the controller meant.
func (f *fakeStore) promotionsFrom(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0
	for _, at := range f.promotedTo {
		if strings.HasPrefix(at, host+":") {
			n++
		}
	}
	return n
}

// newFakeStoreDelayed is a node that answers, eventually. For checking that a
// node slower than the probe timeout counts as silence rather than as an
// answer — the difference between "we could not tell" and "it said it was fine".
func newFakeStoreDelayed(t *testing.T, s status, after time.Duration) (*fakeStore, string) {
	t.Helper()

	f := &fakeStore{status: s}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(after):
		case <-r.Context().Done():
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.status)
	}))
	t.Cleanup(srv.Close)

	at, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return f, at.Host
}
