package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

// A stream cut off by the stall watch says so: the context's own error,
// which is all the reader sees, names no reason.
func TestFetchStreamSaysItStalled(t *testing.T) {
	prev := fetchIdle
	fetchIdle = 150 * time.Millisecond
	t.Cleanup(func() { fetchIdle = prev })
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Write([]byte("head\n")) //nolint:errcheck // test server
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done()
	}))
	defer stalled.Close()
	body, err := fetchStream(context.Background(), stalled.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	_, err = io.ReadAll(body)
	if err == nil || !strings.Contains(err.Error(), "no data for") || !strings.Contains(err.Error(), stalled.URL) {
		t.Fatalf("a stalled stream must say why it ended: %v", err)
	}
}

// rangeServer serves data like a file server that takes byte ranges, and
// breaks off the first cuts responses after half of what they would send.
func rangeServer(t *testing.T, data []byte, etag string, cuts int, ranges bool) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Range")+"|"+r.Header.Get("If-Range"))
		cut := len(seen) <= cuts
		mu.Unlock()
		body, status := data, http.StatusOK
		if ranges {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.Header().Set("ETag", etag)
		if rg := r.Header.Get("Range"); rg != "" && ranges && r.Header.Get("If-Range") == etag {
			var off int
			fmt.Sscanf(rg, "bytes=%d-", &off) //nolint:errcheck // test server
			body, status = data[off:], http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, len(data)-1, len(data)))
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		if cut {
			w.Write(body[:len(body)/2]) //nolint:errcheck // test server
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler) // the connection drops mid-body
		}
		w.Write(body) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// A download that breaks off resumes where it stopped, as often as it
// breaks: the reader gets the file, byte for byte, without seeing a break.
func TestFetchStreamResumesAfterABreak(t *testing.T) {
	prev := fetchRetryBase
	fetchRetryBase = time.Millisecond
	t.Cleanup(func() { fetchRetryBase = prev })
	data := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB
	srv, seen := rangeServer(t, data, `"v1"`, 3, true)
	body, err := fetchStream(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("resumed download: %v, %d of %d bytes, equal %v", err, len(got), len(data), bytes.Equal(got, data))
	}
	if len(*seen) != 4 || (*seen)[0] != "|" || !strings.HasSuffix((*seen)[1], `|"v1"`) || !strings.HasPrefix((*seen)[1], "bytes=") {
		t.Fatalf("one request and three resumes with Range and If-Range: %q", *seen)
	}
}

// A file that changed between the break and the resume comes back whole
// (If-Range does not match): the download fails rather than splices two
// files; so does one from a server that takes no ranges.
func TestFetchStreamDoesNotSplice(t *testing.T) {
	prev := fetchRetryBase
	fetchRetryBase = time.Millisecond
	t.Cleanup(func() { fetchRetryBase = prev })
	data := bytes.Repeat([]byte("x"), 1<<20)
	changed, _ := rangeServer(t, data, `"v1"`, 1, true)
	body, err := fetchStream(context.Background(), changed.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The server's file is "v2" by the time the download resumes.
	changedETag(t, body, `"v2"`)
	if _, err := io.ReadAll(body); err == nil || !strings.Contains(err.Error(), "whole file again") {
		t.Fatalf("a changed file must fail the download: %v", err)
	}
	body.Close()

	plain, seen := rangeServer(t, data, `"v1"`, 1, false)
	body, err = fetchStream(context.Background(), plain.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if _, err := io.ReadAll(body); err == nil || len(*seen) != 1 {
		t.Fatalf("without ranges the break is the reader's: %v, %d requests", err, len(*seen))
	}
}

// changedETag makes the resume of body ask for another version of the file,
// as the server's own file would be after a change.
func changedETag(t *testing.T, body io.ReadCloser, etag string) {
	t.Helper()
	r, ok := body.(*resumingBody)
	if !ok {
		t.Fatalf("fetchStream returned %T", body)
	}
	r.validator = etag
}

// A stall is a break too: the download is cut off after fetchIdle without
// data and picked up where it stopped.
func TestFetchStreamResumesAfterAStall(t *testing.T) {
	prevIdle, prevBase := fetchIdle, fetchRetryBase
	fetchIdle, fetchRetryBase = 150*time.Millisecond, time.Millisecond
	t.Cleanup(func() { fetchIdle, fetchRetryBase = prevIdle, prevBase })
	data := bytes.Repeat([]byte("stall"), 50<<10)
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		first := requests == 1
		mu.Unlock()
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Last-Modified", "Thu, 02 Apr 2026 08:40:37 GMT")
		if first {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			w.Write(data[:1000]) //nolint:errcheck // test server
			w.(http.Flusher).Flush()
			<-r.Context().Done() // and then nothing
			return
		}
		var off int
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &off) //nolint:errcheck // test server
		if r.Header.Get("If-Range") != "Thu, 02 Apr 2026 08:40:37 GMT" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[off:]) //nolint:errcheck // test server
	}))
	defer srv.Close()
	body, err := fetchStream(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(got, data) || requests != 2 {
		t.Fatalf("stalled download resumed: %v, %d of %d bytes, %d requests", err, len(got), len(data), requests)
	}
}
