// Package actioncache maps action keys to action results. Only successful
// actions are cached. Entries reference blobs in a cas.Store; an entry whose
// outputs have been evicted from the CAS is treated as a miss.
package actioncache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/xartus2004/greenline/cas"
)

// ErrNotFound is returned on a cache miss.
var ErrNotFound = errors.New("actioncache: not found")

// ErrNotCacheable is returned when asked to cache a failed action.
var ErrNotCacheable = errors.New("actioncache: only successful actions are cached")

// OutputFile is one file produced by an action.
type OutputFile struct {
	Path       string     `json:"path"`
	Digest     cas.Digest `json:"digest"`
	Executable bool       `json:"executable,omitempty"`
}

// Result is the cached outcome of an action.
type Result struct {
	Outputs  []OutputFile `json:"outputs"`
	ExitCode int          `json:"exit_code"`
	Stdout   cas.Digest   `json:"stdout"`
	Stderr   cas.Digest   `json:"stderr"`
}

// Raw stores opaque action-cache values by key hash. The HTTP cache uses it
// directly because Bazel writes serialized ActionResult protobufs that we do
// not need to interpret.
type Raw interface {
	GetRaw(ctx context.Context, key string) ([]byte, error)
	PutRaw(ctx context.Context, key string, value []byte) error
}

// Cache is the typed action cache used by greenline's build tool.
type Cache interface {
	Get(ctx context.Context, key string) (*Result, error)
	Put(ctx context.Context, key string, r *Result) error
}

// Typed layers JSON-encoded Results over a Raw store, checking on Get that
// every referenced blob is still present in the CAS.
type Typed struct {
	Raw Raw
	CAS cas.Store
}

// Get implements Cache.
func (t Typed) Get(ctx context.Context, key string) (*Result, error) {
	b, err := t.Raw.GetRaw(ctx, key)
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, ErrNotFound // undecodable entry: treat as miss
	}
	if t.CAS != nil {
		ds := []cas.Digest{r.Stdout, r.Stderr}
		for _, o := range r.Outputs {
			ds = append(ds, o.Digest)
		}
		for _, d := range ds {
			if d.IsZero() {
				continue
			}
			ok, err := t.CAS.Has(ctx, d)
			if err != nil || !ok {
				return nil, ErrNotFound
			}
		}
	}
	return &r, nil
}

// Put implements Cache.
func (t Typed) Put(ctx context.Context, key string, r *Result) error {
	if r.ExitCode != 0 {
		return ErrNotCacheable
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return t.Raw.PutRaw(ctx, key, b)
}

// MaxEntryBytes bounds a single action-cache value.
const MaxEntryBytes = 4 << 20

// Disk is a Raw store on the filesystem with atomic writes.
type Disk struct {
	root         string
	hits, misses atomic.Int64
}

// OpenDisk opens a disk action cache rooted at dir.
func OpenDisk(dir string) (*Disk, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o755); err != nil {
		return nil, err
	}
	return &Disk{root: dir}, nil
}

func (d *Disk) path(key string) string { return filepath.Join(d.root, "ac", key[:2], key) }

// GetRaw implements Raw.
func (d *Disk) GetRaw(_ context.Context, key string) ([]byte, error) {
	if !cas.ValidHash(key) {
		return nil, fmt.Errorf("%w: key %q", cas.ErrBadDigest, key)
	}
	b, err := os.ReadFile(d.path(key))
	if errors.Is(err, os.ErrNotExist) {
		d.misses.Add(1)
		return nil, ErrNotFound
	}
	if err == nil {
		d.hits.Add(1)
	}
	return b, err
}

// PutRaw implements Raw.
func (d *Disk) PutRaw(_ context.Context, key string, v []byte) error {
	if !cas.ValidHash(key) {
		return fmt.Errorf("%w: key %q", cas.ErrBadDigest, key)
	}
	if len(v) > MaxEntryBytes {
		return fmt.Errorf("actioncache: entry too large (%d bytes)", len(v))
	}
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Join(d.root, "tmp"), "ac-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(v); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

// Counts returns hit and miss counters.
func (d *Disk) Counts() (hits, misses int64) { return d.hits.Load(), d.misses.Load() }

// Memory is an in-memory Raw store for tests.
type Memory struct {
	mu sync.Mutex
	m  map[string][]byte
}

// NewMemory returns an empty in-memory Raw store.
func NewMemory() *Memory { return &Memory{m: map[string][]byte{}} }

// GetRaw implements Raw.
func (m *Memory) GetRaw(_ context.Context, key string) ([]byte, error) {
	if !cas.ValidHash(key) {
		return nil, cas.ErrBadDigest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

// PutRaw implements Raw.
func (m *Memory) PutRaw(_ context.Context, key string, v []byte) error {
	if !cas.ValidHash(key) {
		return cas.ErrBadDigest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[key] = append([]byte(nil), v...)
	return nil
}
