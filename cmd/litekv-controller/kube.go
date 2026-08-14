package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// The API server, reached with the pod's own service account and nothing else.
//
// No client-go. What this needs of Kubernetes is four calls — list pods, patch
// a service, read a lease, write a lease — and every one of them is a plain
// HTTP request against a documented REST API. The dependency would be larger
// than the program, and this module has none.
//
// It polls rather than watches, which is the other half of that trade. A watch
// brings resourceVersion expiry, bookmarks, 410 Gone and a relist path to get
// right; a poll of three pods every second is a rounding error against what the
// kubelet already does to them.
const (
	serviceAccount = "/var/run/secrets/kubernetes.io/serviceaccount"
	tokenPath      = serviceAccount + "/token"
	caPath         = serviceAccount + "/ca.crt"
)

type kube struct {
	host   string
	token  string
	client *http.Client
}

// inCluster builds a client from what Kubernetes mounts into every pod.
func inCluster() (*kube, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("no KUBERNETES_SERVICE_HOST; this runs inside a cluster")
	}

	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("reading the service account token: %w", err)
	}

	authority, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("reading the cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(authority) {
		return nil, fmt.Errorf("the cluster CA at %s is not a certificate", caPath)
	}

	return &kube{
		host:  fmt.Sprintf("https://%s:%s", host, port),
		token: strings.TrimSpace(string(token)),
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		},
	}, nil
}

// do makes one request and decodes the answer into out, which may be nil.
//
// A 409 is handed back as errConflict rather than as an error message, because
// it is not a failure: on a Lease it is the compare-and-swap saying somebody
// else got there, which is the mechanism working.
func (k *kube) do(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, k.host+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusConflict:
		io.Copy(io.Discard, resp.Body)
		return errConflict
	case resp.StatusCode == http.StatusNotFound:
		io.Copy(io.Discard, resp.Body)
		return errNotFound
	case resp.StatusCode/100 != 2:
		// The body, not just the code. A 403 from the API server names the
		// verb and the resource it refused, which is the difference between
		// "RBAC is wrong" and half a day of guessing which rule is missing.
		said, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(said)))
	}

	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

var (
	errConflict = fmt.Errorf("the object changed underneath")
	errNotFound = fmt.Errorf("no such object")
)

// The slivers of the Kubernetes API this needs. Written out rather than
// imported: what is used is a name, a label, a pod IP and a resourceVersion,
// and a struct saying so is easier to read than a dependency saying everything.

type podList struct {
	Items []pod `json:"items"`
}

type pod struct {
	Metadata struct {
		Name              string            `json:"name"`
		Labels            map[string]string `json:"labels"`
		DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
	} `json:"metadata"`
	Status struct {
		PodIP      string `json:"podIP"`
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

// ready is what the kubelet thinks, which is what decides Service membership.
// A pod can be Running and not Ready, and a pod that is terminating is on its
// way out however it currently reads.
func (p pod) ready() bool {
	if p.Metadata.DeletionTimestamp != nil || p.Status.Phase != "Running" {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

type service struct {
	Spec struct {
		Selector map[string]string `json:"selector"`
	} `json:"spec"`
}

type lease struct {
	Metadata struct {
		Name            string `json:"name"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Spec leaseSpec `json:"spec"`
}

type leaseSpec struct {
	HolderIdentity       string `json:"holderIdentity"`
	LeaseDurationSeconds int    `json:"leaseDurationSeconds"`
	AcquireTime          string `json:"acquireTime,omitempty"`
	RenewTime            string `json:"renewTime"`
}

func (k *kube) pods(ctx context.Context, namespace, selector string) ([]pod, error) {
	var list podList
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s", namespace, selector)
	if err := k.do(ctx, http.MethodGet, path, "", nil, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (k *kube) service(ctx context.Context, namespace, name string) (*service, error) {
	var s service
	path := fmt.Sprintf("/api/v1/namespaces/%s/services/%s", namespace, name)
	if err := k.do(ctx, http.MethodGet, path, "", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// point sets one key of a Service's selector, leaving the rest alone.
//
// A merge patch of a nested object touches only the keys named, which is what
// is wanted: the selector keeps its app labels and changes which pod it means.
//
// One patch type for both this and label, and that is now a measured choice
// rather than a guess. This used to send strategic-merge-patch, on a hunch that
// a plain merge patch might replace the map instead of merging into it — which
// would have left a Service selecting every pod in the namespace. Both were
// tried against a real API server on a selector with three keys, setting one:
//
//	before            {instance: lk, name: litekvd, pod-name: lk-litekvd-0}
//	after json-merge  {instance: lk, name: litekvd, pod-name: promoted-A}
//	after strategic   {instance: lk, name: litekvd, pod-name: promoted-B}
//
// Identical. Strategic merge patch only differs on lists with a patch strategy,
// and spec.selector is a plain map, so the two agree here and the program is
// better for speaking one of them. A mutation swapping them is an equivalent
// mutant and there is deliberately no entry for it.
func (k *kube) point(ctx context.Context, namespace, name, key, value string) error {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"selector": map[string]string{key: value}},
	})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/services/%s", namespace, name)
	return k.do(ctx, http.MethodPatch, path, "application/merge-patch+json", patch, nil)
}

// label sets or removes one label on a pod. An empty value removes it, which in
// a JSON merge patch is a null — checked against a real API server rather than
// assumed, since "the key is set to the string null" and "the key is gone" look
// the same until something selects on it:
//
//	before  {app: x, litekv.io/serving: true}
//	after   {app: x}
func (k *kube) label(ctx context.Context, namespace, name, key, value string) error {
	var labels map[string]any
	if value == "" {
		labels = map[string]any{key: nil}
	} else {
		labels = map[string]any{key: value}
	}

	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", namespace, name)
	return k.do(ctx, http.MethodPatch, path, "application/merge-patch+json", patch, nil)
}

func (k *kube) getLease(ctx context.Context, namespace, name string) (*lease, error) {
	var l lease
	path := fmt.Sprintf("/apis/coordination.k8s.io/v1/namespaces/%s/leases/%s", namespace, name)
	if err := k.do(ctx, http.MethodGet, path, "", nil, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

func (k *kube) createLease(ctx context.Context, namespace, name string, spec leaseSpec) error {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "coordination.k8s.io/v1",
		"kind":       "Lease",
		"metadata":   map[string]string{"name": name, "namespace": namespace},
		"spec":       spec,
	})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/apis/coordination.k8s.io/v1/namespaces/%s/leases", namespace)
	return k.do(ctx, http.MethodPost, path, "application/json", body, nil)
}

// updateLease writes the Lease back, but only if nobody else has written it
// since it was read.
//
// This is the whole of the mutual exclusion. A PUT carrying a resourceVersion
// is a compare-and-swap against etcd, which is a Raft cluster: it either
// applies to exactly the object that was read or it is refused with a 409.
// There is no window in it, and that is why this controller does not need to
// implement any agreement of its own.
func (k *kube) updateLease(ctx context.Context, namespace string, l *lease, spec leaseSpec) error {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "coordination.k8s.io/v1",
		"kind":       "Lease",
		"metadata": map[string]string{
			"name":            l.Metadata.Name,
			"namespace":       namespace,
			"resourceVersion": l.Metadata.ResourceVersion,
		},
		"spec": spec,
	})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/apis/coordination.k8s.io/v1/namespaces/%s/leases/%s", namespace, l.Metadata.Name)
	return k.do(ctx, http.MethodPut, path, "application/json", body, nil)
}
