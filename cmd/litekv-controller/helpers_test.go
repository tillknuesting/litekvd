package main

import (
	"io"
	"log/slog"
	"time"
)

// quiet is a logger that says nothing, so that a test's output is its failures.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func now() time.Time     { return time.Now() }
func longAgo() time.Time { return time.Now().Add(-time.Hour) }

// named is a pod that exists and is not ready, which is the state most of these
// tests want for the node that has gone.
func named(name string) pod {
	var p pod
	p.Metadata.Name = name
	return p
}

// ready is a pod the kubelet is happy with, which is what a promotion candidate
// has to be.
func ready(name string) pod {
	p := named(name)
	p.Status.Phase = "Running"
	p.Status.PodIP = "10.0.0.1"
	p.Status.Conditions = append(p.Status.Conditions, struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	}{Type: "Ready", Status: "True"})
	return p
}
