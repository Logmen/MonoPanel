package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Статика панели — SvelteKit-сборка и урезанный Monaco (4,7 МБ) — вшита в
// бинарник. http.FileServer отдавал её как есть: без сжатия, без ETag и без
// Cache-Control, так что каждая перезагрузка страницы заново тянула редактор
// целиком. Здесь файл читается один раз, сжимается gzip в память, получает
// ETag, а хешированные чанки (`_app/immutable/*`, `editor-KLE6jdfb.js`) —
// кэш на год: их имя меняется вместе с содержимым.

// immutableRe matches content-hashed asset names and nothing else: the
// SvelteKit immutable directory and Monaco's `name-<8-char hash>.js` chunks
// under monaco/vs. Anything human-named (loader.js, editor.main.css,
// nls.messages-loader.js) must keep revalidating, or an upgrade would leave
// browsers on the old build for a year.
var immutableRe = regexp.MustCompile(`^_app/immutable/|^monaco/vs/.*-[A-Za-z0-9_-]{8}\.js$`)

// assetPrefixes are directories where a missing file is a 404, not the SPA
// shell: a script URL must never answer with HTML.
var assetPrefixes = []string{"_app/", "monaco/", "api/"}

type staticFile struct {
	raw       []byte
	gz        []byte // nil when compression does not pay off
	etag      string
	ctype     string
	immutable bool
}

// staticEntry makes preparation single-flight: concurrent cold requests for
// the same file wait for one read+compress instead of repeating it.
type staticEntry struct {
	once sync.Once
	file *staticFile
}

type staticServer struct {
	fsys  fs.FS
	index *staticFile
	files sync.Map // path → *staticEntry
}

func newStaticServer(fsys fs.FS) *staticServer {
	st := &staticServer{fsys: fsys}
	if f, ok := st.load("index.html"); ok {
		st.index = f
	} else {
		st.index = st.prepare("index.html", []byte("<!doctype html><title>MonoPanel</title>"))
	}
	// Прогрев: вся сборка сжимается за ~0,2 с в фоне, чтобы первый
	// посетитель после рестарта не ждал gzip уровня 9 на 2 МБ редактора.
	go func() {
		fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error { //nolint:errcheck // best effort warm-up
			if err == nil && !d.IsDir() {
				st.load(p)
			}
			return nil
		})
	}()
	return st
}

// compressible says whether gzip is worth it for a content type: text and
// scripts shrink 3–4×, fonts and images are already packed.
func compressible(ctype string) bool {
	switch {
	case strings.HasPrefix(ctype, "text/"), strings.HasPrefix(ctype, "application/javascript"),
		strings.HasPrefix(ctype, "application/json"), strings.HasPrefix(ctype, "application/xml"),
		strings.HasPrefix(ctype, "image/svg"), strings.HasPrefix(ctype, "application/wasm"):
		return true
	}
	return false
}

func (st *staticServer) prepare(p string, raw []byte) *staticFile {
	ctype := mime.TypeByExtension(path.Ext(p))
	if ctype == "" {
		ctype = http.DetectContentType(raw)
	}
	sum := sha256.Sum256(raw)
	// Слабый ETag: сжатое и несжатое представление одного файла считаются
	// одинаковыми, как делает nginx с gzip.
	f := &staticFile{raw: raw, ctype: ctype, etag: `W/"` + hex.EncodeToString(sum[:8]) + `"`, immutable: immutableRe.MatchString(p)}
	if compressible(ctype) && len(raw) > 1024 {
		var buf bytes.Buffer
		buf.Grow(len(raw) / 3)
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		zw.Write(raw) //nolint:errcheck // bytes.Buffer never fails
		zw.Close()    //nolint:errcheck // bytes.Buffer never fails
		if buf.Len() < len(raw)*9/10 {
			f.gz = bytes.Clone(buf.Bytes()) // без запаса буфера: живёт всё время процесса
		}
	}
	return f
}

// load returns the prepared file, reading and compressing it on first use.
func (st *staticServer) load(p string) (*staticFile, bool) {
	v, _ := st.files.LoadOrStore(p, &staticEntry{})
	e := v.(*staticEntry)
	e.once.Do(func() {
		if info, err := fs.Stat(st.fsys, p); err != nil || info.IsDir() {
			return
		}
		raw, err := fs.ReadFile(st.fsys, p)
		if err != nil {
			return
		}
		e.file = st.prepare(p, raw)
	})
	if e.file == nil {
		// Не файл: запись не держим, чтобы чужие пути не копились в карте.
		st.files.CompareAndDelete(p, e)
		return nil, false
	}
	return e.file, true
}

// acceptsGzip reads Accept-Encoding the RFC 9110 way: a listed gzip (or a
// bare *) with q>0 accepts it, `gzip;q=0` refuses it, tokens are
// case-insensitive.
func acceptsGzip(header string) bool {
	if header == "" {
		return false
	}
	star := false
	for _, member := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(member), ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		q := 1.0
		for _, param := range strings.Split(params, ";") {
			if k, v, ok := strings.Cut(strings.TrimSpace(param), "="); ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = n
				}
			}
		}
		switch coding {
		case "gzip", "x-gzip":
			return q > 0
		case "*":
			star = q > 0
		}
	}
	return star
}

// etagMatch implements the weak comparison If-None-Match asks for: the
// opaque tags are compared without their W/ prefix, and * matches anything.
func etagMatch(header, etag string) bool {
	want := strings.TrimPrefix(etag, "W/")
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || strings.TrimPrefix(t, "W/") == want {
			return true
		}
	}
	return false
}

func (st *staticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	f, ok := (*staticFile)(nil), false
	if p != "" && p != "index.html" && !strings.HasSuffix(r.URL.Path, "/") {
		f, ok = st.load(p)
	}
	if !ok {
		for _, prefix := range assetPrefixes {
			if strings.HasPrefix(p, prefix) {
				http.NotFound(w, r)
				return
			}
		}
		// SPA: every other unknown path is the app itself, so its router
		// can take over.
		f = st.index
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	if f.immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if f.gz != nil {
		h.Add("Vary", "Accept-Encoding")
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(inm, f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := f.raw
	if f.gz != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		h.Set("Content-Encoding", "gzip")
		body = f.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body) //nolint:errcheck // a closed client is not our problem
}
