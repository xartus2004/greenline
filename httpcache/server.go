// Package httpcache implements Bazel's HTTP remote caching protocol:
//
//	GET|HEAD|PUT  [/<prefix>]/ac/<sha256>   action cache (opaque values)
//	GET|HEAD|PUT  [/<prefix>]/cas/<sha256>  content-addressed blobs
//
// This is what `bazel build --remote_cache=http://host:port` speaks. CAS
// uploads are verified against the hash in the URL; AC values are stored
// opaquely (Bazel writes serialized ActionResult protobufs, greenline's own
// build tool writes JSON).
package httpcache

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/cas"
)

// Server serves the HTTP cache protocol.
type Server struct {
	CAS cas.Store
	AC  actioncache.Raw
	// MaxBlobBytes bounds uploads with no Content-Length (default 256 MiB).
	MaxBlobBytes int64
	// ReadOnly rejects PUTs with 403.
	ReadOnly bool
	Log      *slog.Logger

	Stats Counters
}

// Counters are request counters by namespace.
type Counters struct {
	ACHits, ACMisses, CASHits, CASMisses, Puts, BadRequests atomic.Int64
}

func (s *Server) maxBlob() int64 {
	if s.MaxBlobBytes > 0 {
		return s.MaxBlobBytes
	}
	return 256 << 20
}

// parsePath extracts namespace ("ac" or "cas") and hash from a URL path,
// tolerating an arbitrary prefix (Bazel's URL path / instance name).
func parsePath(p string) (ns, hash string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	ns, hash = parts[len(parts)-2], parts[len(parts)-1]
	if ns != "ac" && ns != "cas" {
		return "", "", false
	}
	for _, seg := range parts {
		if seg == ".." || seg == "." {
			return "", "", false
		}
	}
	return ns, hash, cas.ValidHash(hash)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ns, hash, ok := parsePath(r.URL.Path)
	if !ok {
		s.Stats.BadRequests.Add(1)
		http.Error(w, "expected /ac/<sha256> or /cas/<sha256>", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.get(w, r, ns, hash)
	case http.MethodPut:
		if s.ReadOnly {
			http.Error(w, "read-only cache", http.StatusForbidden)
			return
		}
		s.put(w, r, ns, hash)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, ns, hash string) {
	ctx := r.Context()
	var b []byte
	var err error
	if ns == "ac" {
		b, err = s.AC.GetRaw(ctx, hash)
	} else {
		// The HTTP protocol carries no size.
		b, err = s.CAS.GetByHash(ctx, hash)
	}
	if err != nil {
		if errors.Is(err, actioncache.ErrNotFound) || errors.Is(err, cas.ErrNotFound) {
			if ns == "ac" {
				s.Stats.ACMisses.Add(1)
			} else {
				s.Stats.CASMisses.Add(1)
			}
			http.NotFound(w, r)
			return
		}
		s.logErr("get", ns, hash, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if ns == "ac" {
		s.Stats.ACHits.Add(1)
	} else {
		s.Stats.CASHits.Add(1)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if r.Method == http.MethodHead {
		return
	}
	w.Write(b)
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, ns, hash string) {
	ctx := r.Context()
	s.Stats.Puts.Add(1)
	if ns == "ac" {
		b, err := io.ReadAll(io.LimitReader(r.Body, actioncache.MaxEntryBytes+1))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if len(b) > actioncache.MaxEntryBytes {
			http.Error(w, "entry too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err := s.AC.PutRaw(ctx, hash, b); err != nil {
			s.logErr("put", ns, hash, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	var body io.Reader = r.Body
	size := r.ContentLength
	if size < 0 {
		b, err := io.ReadAll(io.LimitReader(r.Body, s.maxBlob()+1))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if int64(len(b)) > s.maxBlob() {
			http.Error(w, "blob too large", http.StatusRequestEntityTooLarge)
			return
		}
		size, body = int64(len(b)), bytes.NewReader(b)
	}
	err := s.CAS.Put(ctx, cas.Digest{Hash: hash, Size: size}, body)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, cas.ErrDigestMismatch):
		s.Stats.BadRequests.Add(1)
		http.Error(w, "content does not match hash", http.StatusBadRequest)
	case errors.Is(err, cas.ErrTooLarge):
		http.Error(w, "blob too large", http.StatusRequestEntityTooLarge)
	default:
		s.logErr("put", ns, hash, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *Server) logErr(op, ns, hash string, err error) {
	if s.Log != nil {
		s.Log.Error("httpcache", "op", op, "ns", ns, "hash", hash, "err", err)
	}
}
