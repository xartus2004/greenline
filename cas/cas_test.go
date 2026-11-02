package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var ctx = context.Background()

func TestDigestParse(t *testing.T) {
	d := Of([]byte("hello"))
	got, err := Parse(d.String())
	if err != nil || got != d {
		t.Fatalf("roundtrip: %v %v", got, err)
	}
	bad := []string{"", "abc/1", d.Hash, d.Hash + "/-1", d.Hash + "/+1", d.Hash + "/x",
		strings.ToUpper(d.Hash) + "/5", "../../etc/passwd/1", d.Hash[:63] + "//5"}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add(Of([]byte("x")).String())
	f.Add("../" + strings.Repeat("a", 61) + "/1")
	f.Add(strings.Repeat("a", 64) + "/0")
	f.Fuzz(func(t *testing.T, s string) {
		d, err := Parse(s)
		if err != nil {
			return
		}
		// Anything accepted must be safe to embed in a path.
		if strings.ContainsAny(d.Hash, "/\\.") || len(d.Hash) != 64 || d.Size < 0 {
			t.Fatalf("unsafe digest accepted: %q", s)
		}
		if filepath.Base(d.Hash) != d.Hash {
			t.Fatalf("hash not a single path element: %q", d.Hash)
		}
	})
}

func stores(t *testing.T) map[string]Store {
	disk, err := OpenDisk(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Store{"disk": disk, "memory": NewMemory()}
}

func TestPutGet(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			d, err := PutBytes(ctx, s, []byte("content"))
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.Get(ctx, d)
			if err != nil || string(b) != "content" {
				t.Fatalf("get: %q %v", b, err)
			}
			if ok, _ := s.Has(ctx, d); !ok {
				t.Fatal("Has = false")
			}
			// Mismatched content is rejected.
			if err := s.Put(ctx, d, strings.NewReader("other!!")); !errors.Is(err, ErrDigestMismatch) {
				t.Fatalf("mismatch: %v", err)
			}
			// Missing blob.
			if _, err := s.Get(ctx, Of([]byte("nope"))); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing: %v", err)
			}
			// Empty blob always present.
			if b, err := s.Get(ctx, Empty); err != nil || len(b) != 0 {
				t.Fatalf("empty: %v", err)
			}
			// Invalid digests never touch the filesystem.
			if _, err := s.Get(ctx, Digest{Hash: "../../x", Size: 1}); !errors.Is(err, ErrBadDigest) {
				t.Fatalf("bad digest: %v", err)
			}
		})
	}
}

func TestDiskLongerContentRejected(t *testing.T) {
	s, _ := OpenDisk(t.TempDir(), 1<<20)
	d := Of([]byte("abc"))
	if err := s.Put(ctx, d, strings.NewReader("abcdef")); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("got %v", err)
	}
	if s.Stats().Items != 0 {
		t.Fatal("rejected blob indexed")
	}
}

func TestDiskCorruptBlobIsMissAndDeleted(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenDisk(dir, 1<<20)
	d, _ := PutBytes(ctx, s, []byte("precious"))
	p := filepath.Join(dir, "blobs", d.Hash[:2], d.Hash)
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt read: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("corrupt file not deleted")
	}
	if st := s.Stats(); st.Corrupt != 1 || st.Items != 0 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestDiskLRUEviction(t *testing.T) {
	s, _ := OpenDisk(t.TempDir(), 300)
	var ds []Digest
	for i := 0; i < 3; i++ {
		d, err := PutBytes(ctx, s, bytes.Repeat([]byte{byte('a' + i)}, 100))
		if err != nil {
			t.Fatal(err)
		}
		ds = append(ds, d)
	}
	// Touch the oldest so the middle one becomes LRU.
	if _, err := s.Get(ctx, ds[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := PutBytes(ctx, s, bytes.Repeat([]byte("z"), 100)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Has(ctx, ds[1]); ok {
		t.Fatal("LRU blob not evicted")
	}
	for _, d := range []Digest{ds[0], ds[2]} {
		if ok, _ := s.Has(ctx, d); !ok {
			t.Fatal("recent blob evicted")
		}
	}
	st := s.Stats()
	if st.Bytes > 300 || st.Evictions != 1 {
		t.Fatalf("stats %+v", st)
	}
	if _, err := PutBytes(ctx, s, bytes.Repeat([]byte("q"), 301)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
}

func TestDiskReopenKeepsBlobsAndDropsTemp(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenDisk(dir, 1<<20)
	d, _ := PutBytes(ctx, s, []byte("persist"))
	os.WriteFile(filepath.Join(dir, "tmp", "put-123"), []byte("half"), 0o644)
	s2, err := OpenDisk(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := s2.Get(ctx, d); err != nil || string(b) != "persist" {
		t.Fatalf("after reopen: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(ents) != 0 {
		t.Fatal("temp files not cleaned")
	}
}

func TestDiskConcurrent(t *testing.T) {
	s, _ := OpenDisk(t.TempDir(), 64<<10)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				b := []byte(fmt.Sprintf("blob-%d-%s", i%50, strings.Repeat("x", i)))
				d, err := PutBytes(ctx, s, b)
				if err != nil {
					t.Error(err)
					return
				}
				if got, err := s.Get(ctx, d); err == nil && !bytes.Equal(got, b) {
					t.Error("wrong content")
				}
			}
		}(g)
	}
	wg.Wait()
	if st := s.Stats(); st.Bytes > st.MaxBytes {
		t.Fatalf("over capacity: %+v", st)
	}
}

func benchStore(b *testing.B, size int) {
	s, _ := OpenDisk(b.TempDir(), 1<<30)
	blob := bytes.Repeat([]byte("g"), size)
	b.SetBytes(int64(size))
	b.Run("put", func(b *testing.B) {
		b.SetBytes(int64(size))
		for i := 0; i < b.N; i++ {
			blob[0] = byte(i)
			blob[1] = byte(i >> 8)
			blob[2] = byte(i >> 16)
			if _, err := PutBytes(ctx, s, blob); err != nil {
				b.Fatal(err)
			}
		}
	})
	d, _ := PutBytes(ctx, s, blob)
	b.Run("get", func(b *testing.B) {
		b.SetBytes(int64(size))
		for i := 0; i < b.N; i++ {
			if _, err := s.Get(ctx, d); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("get-parallel", func(b *testing.B) {
		b.SetBytes(int64(size))
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if _, err := s.Get(ctx, d); err != nil {
					b.Error(err)
				}
			}
		})
	})
}

func BenchmarkDisk4K(b *testing.B) { benchStore(b, 4<<10) }
func BenchmarkDisk1M(b *testing.B) { benchStore(b, 1<<20) }
