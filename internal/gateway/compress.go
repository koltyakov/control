package gateway

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Directory and status reads are the gateway's largest periodic responses:
// every console tab polls status and CLI name lookups read the directory.
// They carry fleet metadata, not credentials, so they are compressed when the
// caller accepts gzip. BestSpeed keeps most of the size reduction at a small
// CPU cost on single-core gateways.
var gzipWriters = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return w
}}

// acceptsGzip reports whether gzip is an acceptable response coding.
func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		for param := range strings.SplitSeq(params, ";") {
			name, value, _ := strings.Cut(strings.TrimSpace(param), "=")
			if strings.EqualFold(strings.TrimSpace(name), "q") {
				if q, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && q <= 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

func writeConditionalMetadataJSON(w http.ResponseWriter, r *http.Request, value any, etag string, unchanged bool) {
	compress := acceptsGzip(r)
	if etag != "" {
		// The gzip body is a different byte representation of the same view.
		if compress {
			etag = "W/" + etag
		}
		w.Header().Set("ETag", etag)
	}
	if unchanged {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeMetadataJSON(w, value, compress)
}

// writeMetadataJSON encodes a fleet metadata response, compressed when compress
// is set. Callers set caching headers and validators first.
func writeMetadataJSON(w http.ResponseWriter, value any, compress bool) {
	w.Header().Set("Content-Type", "application/json")
	if !compress {
		_ = json.NewEncoder(w).Encode(value)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	gz := gzipWriters.Get().(*gzip.Writer)
	gz.Reset(w)
	_ = json.NewEncoder(gz).Encode(value)
	_ = gz.Close()
	gz.Reset(io.Discard)
	gzipWriters.Put(gz)
}
