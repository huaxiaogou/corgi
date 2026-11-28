package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

type staticAsset struct {
	raw          []byte
	gz           []byte
	etag         string
	gzETag       string
	contentType  string
	compressible bool
	immutable    bool
}

func (s *Server) staticHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqPath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if reqPath == "" || reqPath == "." {
			reqPath = "index.html"
		}

		isAssetsPath := strings.HasPrefix(reqPath, "assets/")
		servePath := reqPath
		asset, ok := s.loadStaticAsset(servePath)
		if !ok {
			if isAssetsPath {
				http.NotFound(w, r)
				return
			}
			servePath = "index.html"
			asset, ok = s.loadStaticAsset(servePath)
			if !ok {
				http.NotFound(w, r)
				return
			}
		}

		if asset.contentType != "" {
			w.Header().Set("Content-Type", asset.contentType)
		}
		if asset.compressible {
			w.Header().Set("Vary", "Accept-Encoding")
		}
		if asset.immutable {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}

		body := asset.raw
		if len(asset.gz) > 0 && acceptsGzip(r) && r.Header.Get("Range") == "" {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("ETag", asset.gzETag)
			body = asset.gz
		} else {
			w.Header().Set("ETag", asset.etag)
		}

		http.ServeContent(w, r, path.Base(servePath), time.Time{}, bytes.NewReader(body))
	})
}

func (s *Server) loadStaticAsset(p string) (*staticAsset, bool) {
	if !fs.ValidPath(p) {
		return nil, false
	}
	if v, ok := s.staticAssets.Load(p); ok {
		return v.(*staticAsset), true
	}
	fi, err := fs.Stat(s.StaticFS, p)
	if err != nil || fi.IsDir() {
		return nil, false
	}
	raw, err := fs.ReadFile(s.StaticFS, p)
	if err != nil {
		return nil, false
	}

	sum := sha256.Sum256(raw)
	hexSum := hex.EncodeToString(sum[:8])
	asset := &staticAsset{
		raw:          raw,
		etag:         `"` + hexSum + `"`,
		contentType:  staticContentType(p, raw),
		compressible: isCompressibleStatic(p),
		immutable:    isHashedStaticAsset(p),
	}

	if asset.compressible && len(raw) > 0 {
		var buf bytes.Buffer
		if gw, gErr := gzip.NewWriterLevel(&buf, gzip.BestCompression); gErr == nil {
			_, wErr := gw.Write(raw)
			cErr := gw.Close()
			if wErr == nil && cErr == nil && buf.Len() < len(raw) {
				asset.gz = buf.Bytes()
				asset.gzETag = `"` + hexSum + `-gz"`
			}
		}
	}

	actual, _ := s.staticAssets.LoadOrStore(p, asset)
	return actual.(*staticAsset), true
}

func isCompressibleStatic(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".html", ".js", ".mjs", ".css", ".svg", ".webmanifest", ".json":
		return true
	default:
		return false
	}
}

func isHashedStaticAsset(p string) bool {
	if !strings.HasPrefix(p, "assets/") {
		return false
	}
	base := path.Base(p)
	stem := strings.TrimSuffix(base, path.Ext(base))
	idx := strings.LastIndexByte(stem, '-')
	if idx <= 0 || idx >= len(stem)-1 {
		return false
	}
	hash := stem[idx+1:]
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
		if !isAlphaNum {
			return false
		}
	}
	return true
}

func staticContentType(p string, raw []byte) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".webmanifest":
		return "application/manifest+json"
	case ".json":
		return "application/json; charset=utf-8"
	}
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		return ct
	}
	if len(raw) > 0 {
		return http.DetectContentType(raw)
	}
	return "application/octet-stream"
}

func acceptsGzip(r *http.Request) bool {
	var (
		hasGzip bool
		gzipQ   float64
		hasStar bool
		starQ   float64
	)
	for _, hdr := range r.Header.Values("Accept-Encoding") {
		for _, part := range strings.Split(hdr, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			segs := strings.Split(part, ";")
			coding := strings.ToLower(strings.TrimSpace(segs[0]))
			q := 1.0
			valid := true
			for _, param := range segs[1:] {
				k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
					continue
				}
				parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				if err != nil || parsed < 0 || parsed > 1 {
					valid = false
					break
				}
				q = parsed
			}
			if !valid {
				continue
			}
			switch coding {
			case "gzip", "x-gzip":
				if !hasGzip || q > gzipQ {
					hasGzip = true
					gzipQ = q
				}
			case "*":
				if !hasStar || q > starQ {
					hasStar = true
					starQ = q
				}
			}
		}
	}
	if hasGzip {
		return gzipQ > 0
	}
	if hasStar {
		return starQ > 0
	}
	return false
}
