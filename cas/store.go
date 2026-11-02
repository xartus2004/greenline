package cas

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned when a blob is absent (or was found corrupt and
// discarded).
var ErrNotFound = errors.New("cas: not found")

// ErrDigestMismatch is returned by Put when content does not match the
// claimed digest.
var ErrDigestMismatch = errors.New("cas: content does not match digest")

// Store is a content-addressed blob store. Implementations must be safe for
// concurrent use.
type Store interface {
	// Has reports whether the blob is present.
	Has(ctx context.Context, d Digest) (bool, error)
	// Get returns the blob contents. A blob that fails its integrity check
	// is deleted and reported as ErrNotFound.
	Get(ctx context.Context, d Digest) ([]byte, error)
	// GetByHash is Get for protocols that carry only the hash (Bazel's
	// HTTP cache). The integrity check still applies.
	GetByHash(ctx context.Context, hash string) ([]byte, error)
	// Put stores r under d, verifying the content hashes to d.
	Put(ctx context.Context, d Digest, r io.Reader) error
	// Stats returns counters for metrics.
	Stats() Stats
}

// Stats are cumulative store counters.
type Stats struct {
	Hits, Misses, Puts, Evictions, Corrupt int64
	Bytes, Items                           int64
	MaxBytes                               int64
}

// PutBytes is a convenience wrapper that computes the digest itself.
func PutBytes(ctx context.Context, s Store, b []byte) (Digest, error) {
	d := Of(b)
	return d, s.Put(ctx, d, bytesReader(b))
}

type br struct {
	b []byte
	i int
}

func (r *br) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func bytesReader(b []byte) io.Reader { return &br{b: b} }
