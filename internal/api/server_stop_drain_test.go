package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A restart must let a request that is already running finish instead of cutting it.
func TestServerStopDrainsInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{server: &http.Server{Handler: handler}}
	go func() { _ = s.server.Serve(listener) }()

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, errGet := http.Get("http://" + listener.Addr().String() + "/slow")
		if errGet != nil {
			done <- result{err: errGet}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, errRead := io.ReadAll(resp.Body)
		done <- result{body: string(body), err: errRead}
	}()
	<-started

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopped <- s.Stop(ctx)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while a request was still in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)

	got := <-done
	if got.err != nil || got.body != "done" {
		t.Fatalf("in-flight request was cut: body=%q err=%v", got.body, got.err)
	}
	if errStop := <-stopped; errStop != nil {
		t.Fatalf("Stop: %v", errStop)
	}
}

// A drain that outlives its deadline still closes the server.
func TestServerStopClosesAfterDrainDeadline(t *testing.T) {
	started := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{server: &http.Server{Handler: handler}}
	go func() { _ = s.server.Serve(listener) }()
	go func() { _, _ = http.Get("http://" + listener.Addr().String() + "/hang") }()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if errStop := s.Stop(ctx); errStop != nil {
		t.Fatalf("Stop: %v", errStop)
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("Stop did not honour the drain deadline: %v", elapsed)
	}
}
