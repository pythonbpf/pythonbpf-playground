package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
)

// assets serves the embedded UI from memory. Everything is prepared once at
// startup: gzip at max compression, ETags, and a content hash that index.html
// uses as ?v= so browsers and Cloudflare's edge can cache assets for a year.
// After that, a request costs a map lookup and a write.
type asset struct {
	body, gz []byte
	ctype    string
	etag     string
	html     bool
}

type assets struct {
	files   map[string]*asset
	version string
}

func loadAssets(fsys fs.FS) (*assets, error) {
	raw := map[string][]byte{}
	var names []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		raw["/"+p] = b
		names = append(names, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	h := sha256.New()
	var examples []string
	for _, n := range names {
		h.Write([]byte(n))
		h.Write(raw["/"+n])
		if strings.HasPrefix(n, "examples/") && strings.HasSuffix(n, ".py") {
			examples = append(examples, strings.TrimPrefix(n, "examples/"))
		}
	}
	a := &assets{files: map[string]*asset{}, version: hex.EncodeToString(h.Sum(nil))[:12]}
	if examples == nil {
		examples = []string{}
	}
	ex, _ := json.Marshal(examples)
	raw["/examples.json"] = ex

	for p, b := range raw {
		isHTML := strings.HasSuffix(p, ".html")
		if isHTML {
			b = bytes.ReplaceAll(b, []byte("?v=dev"), []byte("?v="+a.version))
		}
		ctype := mime.TypeByExtension(path.Ext(p))
		switch path.Ext(p) {
		case ".py":
			ctype = "text/plain; charset=utf-8"
		case ".json":
			ctype = "application/json"
		case ".ico": // not in Go's built-in table; don't depend on /etc/mime.types
			ctype = "image/x-icon"
		}
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		sum := sha256.Sum256(b)
		f := &asset{body: b, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, html: isHTML}
		if compressible(ctype) && len(b) > 512 {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(b)
			zw.Close()
			if buf.Len() < len(b) {
				f.gz = buf.Bytes()
			}
		}
		a.files[p] = f
	}
	return a, nil
}

func compressible(ctype string) bool {
	return strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "javascript") ||
		strings.Contains(ctype, "json") || strings.Contains(ctype, "svg") || ctype == "image/x-icon"
}

func (a *assets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch p {
	case "/":
		p = "/index.html"
	case "/about":
		p = "/about.html"
	}
	f := a.files[p]
	if f == nil {
		http.NotFound(w, r)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", f.ctype)
	hd.Set("ETag", f.etag)
	hd.Set("Vary", "Accept-Encoding")
	switch {
	case f.html:
		hd.Set("Cache-Control", "no-cache") // tiny; always revalidated (304)
	case r.URL.Query().Get("v") == a.version:
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		hd.Set("Cache-Control", "public, max-age=300")
	}
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := f.body
	if f.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		hd.Set("Content-Encoding", "gzip")
		body = f.gz
	}
	w.Write(body)
}
