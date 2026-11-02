package actioncache

import (
	"context"
	"errors"
	"testing"

	"github.com/xartus2004/greenline/cas"
)

func TestTypedRoundTripAndEvictedOutputs(t *testing.T) {
	ctx := context.Background()
	disk, err := OpenDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]Raw{"disk": disk, "memory": NewMemory()} {
		t.Run(name, func(t *testing.T) {
			store := cas.NewMemory()
			out, _ := cas.PutBytes(ctx, store, []byte("binary"))
			c := Typed{Raw: raw, CAS: store}
			key := cas.Of([]byte("action-1")).Hash
			if _, err := c.Get(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cold: %v", err)
			}
			r := &Result{Outputs: []OutputFile{{Path: "bin/x", Digest: out}}}
			if err := c.Put(ctx, key, r); err != nil {
				t.Fatal(err)
			}
			got, err := c.Get(ctx, key)
			if err != nil || got.Outputs[0].Digest != out {
				t.Fatalf("warm: %+v %v", got, err)
			}
			if err := c.Put(ctx, key, &Result{ExitCode: 1}); !errors.Is(err, ErrNotCacheable) {
				t.Fatalf("failed action cached: %v", err)
			}
			// Output missing from CAS → miss.
			key2 := cas.Of([]byte("action-2")).Hash
			c.Put(ctx, key2, &Result{Outputs: []OutputFile{{Path: "y", Digest: cas.Of([]byte("gone"))}}})
			if _, err := c.Get(ctx, key2); !errors.Is(err, ErrNotFound) {
				t.Fatalf("dangling entry served: %v", err)
			}
			if _, err := raw.GetRaw(ctx, "../../etc/passwd"); err == nil {
				t.Fatal("bad key accepted")
			}
		})
	}
}
