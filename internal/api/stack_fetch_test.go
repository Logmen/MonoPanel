package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A download is bounded by stalls, not by a total deadline: a slow but
// steady body gets through, one that stops delivering is cut off.
func TestFetchWatchesForStalls(t *testing.T) {
	prev := fetchIdle
	fetchIdle = 150 * time.Millisecond
	t.Cleanup(func() { fetchIdle = prev })

	steady := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, _ := w.(http.Flusher)
		for i := 0; i < 8; i++ {
			w.Write([]byte("chunk\n")) //nolint:errcheck // test server
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(60 * time.Millisecond) // well over the idle window in total, under it per chunk
		}
	}))
	defer steady.Close()
	b, err := fetchBytesN(context.Background(), steady.URL, 1<<20)
	if err != nil || strings.Count(string(b), "chunk") != 8 {
		t.Fatalf("steady download: %v %q", err, b)
	}

	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Write([]byte("head\n")) //nolint:errcheck // test server
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done() // never sends the rest
	}))
	defer stalled.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err = fetchOnce(ctx, stalled.URL, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "no data for") {
		t.Fatalf("stalled download must be cut off: %v", err)
	}
}

// One dropped connection or a 5xx from GitHub must not fail an install: the
// latest-release lookup and the download stream retry, a 404 does not.
func TestFetchRetriesTransientFailures(t *testing.T) {
	prev := fetchRetryBase
	fetchRetryBase = time.Millisecond
	t.Cleanup(func() { fetchRetryBase = prev })

	var tagCalls, streamCalls, goneCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/joomla/joomla-cms/releases/latest":
			tagCalls++
			if tagCalls == 1 {
				// A connection that dies before any response.
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close() //nolint:errcheck // test server
				return
			}
			if tagCalls == 2 {
				http.Error(w, "unicorn", http.StatusBadGateway)
				return
			}
			http.Redirect(w, r, "/joomla/joomla-cms/releases/tag/6.0.1", http.StatusFound)
		case "/dist.tar.gz":
			streamCalls++
			if streamCalls == 1 {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte("tarball")) //nolint:errcheck // test server
		case "/gone.tar.gz":
			goneCalls++
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tag, err := githubLatestTagAt(context.Background(), srv.URL, "joomla/joomla-cms")
	if err != nil || tag != "6.0.1" || tagCalls != 3 {
		t.Fatalf("latest tag: %q %v after %d calls", tag, err, tagCalls)
	}
	body, err := fetchStream(context.Background(), srv.URL+"/dist.tar.gz")
	if err != nil || streamCalls != 2 {
		t.Fatalf("stream: %v after %d calls", err, streamCalls)
	}
	b := make([]byte, 16)
	n, _ := body.Read(b)
	body.Close() //nolint:errcheck // test
	if string(b[:n]) != "tarball" {
		t.Fatalf("stream body: %q", b[:n])
	}
	if _, err := fetchStream(context.Background(), srv.URL+"/gone.tar.gz"); err == nil || goneCalls != 1 {
		t.Fatalf("404 must fail at once: %v after %d calls", err, goneCalls)
	}
}
