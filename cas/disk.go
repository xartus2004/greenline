package cas

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
)

// ErrTooLarge is returned when a single blob exceeds the store capacity.
var ErrTooLarge = errors.New("cas: blob larger than store capacity")

// Disk is a size-bounded, LRU-evicting blob store on the local filesystem.
//
// Layout: <root>/blobs/<hh>/<hash> where hh is the first two hex chars, and
// <root>/tmp for in-progress writes. Writes go to a temp file and are renamed
// into place only after the hash is verified, so a crash never leaves a
// partially written blob under a valid name. Reads re-hash the content; a
// mismatch (bit rot, manual tampering) deletes the file and reports a miss.
type Disk struct {
	root     string
	maxBytes int64

	mu    sync.Mutex
	lru   *list.List // front = most recently used; values are *entry
	index map[string]*list.Element
	bytes int64

	hits, misses, puts, evictions, corrupt atomic.Int64
}

type entry struct {
	hash string
	size int64
}

// OpenDisk opens (creating if needed) a disk store rooted at dir holding at
// most maxBytes of blob content. Existing blobs are indexed; their LRU order
// is approximated by modification time.
func OpenDisk(dir string, maxBytes int64) (*Disk, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("cas: maxBytes must be positive")
	}
	d := &Disk{root: dir, maxBytes: maxBytes, lru: list.New(), index: map[string]*list.Element{}}
	for _, sub := range []string{"blobs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	// Leftover temp files are from interrupted writes; they are garbage.
	tmps, _ := os.ReadDir(filepath.Join(dir, "tmp"))
	for _, t := range tmps {
		os.Remove(filepath.Join(dir, "tmp", t.Name()))
	}
	type found struct {
		e   entry
		mod int64
	}
	var all []found
	err := filepath.WalkDir(filepath.Join(dir, "blobs"), func(p string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		if !ValidHash(de.Name()) {
			os.Remove(p)
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return nil
		}
		all = append(all, found{entry{de.Name(), info.Size()}, info.ModTime().UnixNano()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Oldest first, pushed to front, so newest ends at front.
	sort.Slice(all, func(i, j int) bool { return all[i].mod < all[j].mod })
	for _, f := range all {
		e := f.e
		d.index[e.hash] = d.lru.PushFront(&e)
		d.bytes += e.size
	}
	d.mu.Lock()
	d.evictLocked()
	d.mu.Unlock()
	return d, nil
}

func (d *Disk) path(hash string) string {
	return filepath.Join(d.root, "blobs", hash[:2], hash)
}

// Has implements Store.
func (d *Disk) Has(_ context.Context, dg Digest) (bool, error) {
	if err := dg.Validate(); err != nil {
		return false, err
	}
	if dg.Size == 0 && dg.Hash == Empty.Hash {
		return true, nil
	}
	d.mu.Lock()
	el, ok := d.index[dg.Hash]
	if ok {
		d.lru.MoveToFront(el)
	}
	d.mu.Unlock()
	return ok, nil
}

// Get implements Store.
func (d *Disk) Get(ctx context.Context, dg Digest) ([]byte, error) {
	if err := dg.Validate(); err != nil {
		return nil, err
	}
	if dg.Hash == Empty.Hash && dg.Size == 0 {
		d.hits.Add(1)
		return []byte{}, nil
	}
	d.mu.Lock()
	el, ok := d.index[dg.Hash]
	if ok {
		d.lru.MoveToFront(el)
	}
	d.mu.Unlock()
	if !ok {
		d.misses.Add(1)
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(d.path(dg.Hash))
	if errors.Is(err, os.ErrNotExist) {
		d.drop(dg.Hash, false)
		d.misses.Add(1)
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != dg.Hash || (dg.Size >= 0 && int64(len(b)) != dg.Size) {
		if hex.EncodeToString(sum[:]) != dg.Hash {
			d.corrupt.Add(1)
			d.drop(dg.Hash, true)
		}
		d.misses.Add(1)
		return nil, ErrNotFound
	}
	d.hits.Add(1)
	return b, nil
}

// GetByHash implements Store.
func (d *Disk) GetByHash(ctx context.Context, hash string) ([]byte, error) {
	if !ValidHash(hash) {
		return nil, ErrBadDigest
	}
	if hash == Empty.Hash {
		return d.Get(ctx, Empty)
	}
	d.mu.Lock()
	el, ok := d.index[hash]
	var size int64
	if ok {
		size = el.Value.(*entry).size
	}
	d.mu.Unlock()
	if !ok {
		d.misses.Add(1)
		return nil, ErrNotFound
	}
	return d.Get(ctx, Digest{Hash: hash, Size: size})
}

// drop removes hash from the index (and the file if remove).
func (d *Disk) drop(hash string, remove bool) {
	d.mu.Lock()
	if el, ok := d.index[hash]; ok {
		d.bytes -= el.Value.(*entry).size
		d.lru.Remove(el)
		delete(d.index, hash)
	}
	d.mu.Unlock()
	if remove {
		os.Remove(d.path(hash))
	}
}

// Put implements Store.
func (d *Disk) Put(ctx context.Context, dg Digest, r io.Reader) error {
	if err := dg.Validate(); err != nil {
		return err
	}
	if dg.Size > d.maxBytes {
		return ErrTooLarge
	}
	tmp, err := os.CreateTemp(filepath.Join(d.root, "tmp"), "put-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	h := sha256.New()
	// Read at most Size+1 bytes so a lying client cannot fill the disk.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, dg.Size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != dg.Size || hex.EncodeToString(h.Sum(nil)) != dg.Hash {
		return fmt.Errorf("%w: got %d bytes", ErrDigestMismatch, n)
	}
	if dg.Size == 0 {
		return nil
	}
	d.puts.Add(1)
	d.mu.Lock()
	if el, ok := d.index[dg.Hash]; ok {
		d.lru.MoveToFront(el)
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()
	final := d.path(dg.Hash)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return err
	}
	d.mu.Lock()
	if _, ok := d.index[dg.Hash]; !ok {
		d.index[dg.Hash] = d.lru.PushFront(&entry{dg.Hash, dg.Size})
		d.bytes += dg.Size
	}
	d.evictLocked()
	d.mu.Unlock()
	return nil
}

func (d *Disk) evictLocked() {
	for d.bytes > d.maxBytes {
		el := d.lru.Back()
		if el == nil {
			return
		}
		e := el.Value.(*entry)
		d.lru.Remove(el)
		delete(d.index, e.hash)
		d.bytes -= e.size
		d.evictions.Add(1)
		os.Remove(d.path(e.hash))
	}
}

// Stats implements Store.
func (d *Disk) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Stats{
		Hits: d.hits.Load(), Misses: d.misses.Load(), Puts: d.puts.Load(),
		Evictions: d.evictions.Load(), Corrupt: d.corrupt.Load(),
		Bytes: d.bytes, Items: int64(len(d.index)), MaxBytes: d.maxBytes,
	}
}

// Memory is an unbounded in-memory Store for tests.
type Memory struct {
	mu sync.Mutex
	m  map[string][]byte
	st Stats
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{m: map[string][]byte{}} }

// Has implements Store.
func (m *Memory) Has(_ context.Context, d Digest) (bool, error) {
	if err := d.Validate(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.m[d.Hash]
	return ok || d == Empty, nil
}

// Get implements Store.
func (m *Memory) Get(_ context.Context, d Digest) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if d == Empty {
		m.st.Hits++
		return []byte{}, nil
	}
	b, ok := m.m[d.Hash]
	if !ok {
		m.st.Misses++
		return nil, ErrNotFound
	}
	m.st.Hits++
	return bytes.Clone(b), nil
}

// GetByHash implements Store.
func (m *Memory) GetByHash(ctx context.Context, hash string) ([]byte, error) {
	if !ValidHash(hash) {
		return nil, ErrBadDigest
	}
	m.mu.Lock()
	b, ok := m.m[hash]
	m.mu.Unlock()
	if !ok && hash != Empty.Hash {
		m.mu.Lock()
		m.st.Misses++
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	return m.Get(ctx, Digest{Hash: hash, Size: int64(len(b))})
}

// Put implements Store.
func (m *Memory) Put(_ context.Context, d Digest, r io.Reader) error {
	if err := d.Validate(); err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(r, d.Size+1))
	if err != nil {
		return err
	}
	if Of(b) != d {
		return ErrDigestMismatch
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Puts++
	if _, ok := m.m[d.Hash]; !ok {
		m.m[d.Hash] = b
		m.st.Bytes += d.Size
		m.st.Items++
	}
	return nil
}

// Stats implements Store.
func (m *Memory) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st
}
