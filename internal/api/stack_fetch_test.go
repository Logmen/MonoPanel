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
