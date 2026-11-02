package httpcache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/cas"
)

func newServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	st, err := cas.OpenDisk(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ac, _ := actioncache.OpenDisk(t.TempDir())
	s := &Server{CAS: st, AC: ac}
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	return s, hs
}

func req(t *testing.T, method, url string, body []byte, chunked bool) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
		if chunked {
			r = io.MultiReader(r) // hides length → chunked encoding
		}
	}
	rq, _ := http.NewRequest(method, url, r)
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestProtocol(t *testing.T) {
	_, hs := newServer(t)
	blob := []byte("hello bazel")
	d := cas.Of(blob)

	if r := req(t, "GET", hs.URL+"/cas/"+d.Hash, nil, false); r.StatusCode != 404 {
		t.Fatalf("cold GET: %d", r.StatusCode)
	}
	for _, chunked := range []bool{false, true} {
		if r := req(t, "PUT", hs.URL+"/cas/"+d.Hash, blob, chunked); r.StatusCode != 200 {
			t.Fatalf("PUT chunked=%v: %d", chunked, r.StatusCode)
		}
	}
	r := req(t, "GET", hs.URL+"/cas/"+d.Hash, nil, false)
	got, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !bytes.Equal(got, blob) {
		t.Fatalf("GET: %d %q", r.StatusCode, got)
	}
	if r := req(t, "HEAD", hs.URL+"/cas/"+d.Hash, nil, false); r.StatusCode != 200 {
		t.Fatalf("HEAD: %d", r.StatusCode)
	}
	// Prefixed path (bazel --remote_cache=http://host/some/prefix).
	if r := req(t, "GET", hs.URL+"/some/prefix/cas/"+d.Hash, nil, false); r.StatusCode != 200 {
		t.Fatalf("prefixed GET: %d", r.StatusCode)
	}
	// Hash mismatch.
	if r := req(t, "PUT", hs.URL+"/cas/"+d.Hash, []byte("evil"), false); r.StatusCode != 400 {
		t.Fatalf("mismatch PUT: %d", r.StatusCode)
	}
	// AC: opaque bytes.
	key := cas.Of([]byte("action")).Hash
	if r := req(t, "PUT", hs.URL+"/ac/"+key, []byte{0x0a, 0x01, 0xff}, false); r.StatusCode != 200 {
		t.Fatalf("AC PUT: %d", r.StatusCode)
	}
	r = req(t, "GET", hs.URL+"/ac/"+key, nil, false)
	got, _ = io.ReadAll(r.Body)
	if !bytes.Equal(got, []byte{0x0a, 0x01, 0xff}) {
		t.Fatalf("AC GET: %x", got)
	}
	// Bad paths and methods.
	for _, p := range []string{"/cas/abc", "/cas/../" + d.Hash, "/foo/" + d.Hash, "/cas/" + strings.ToUpper(d.Hash), "/"} {
		if r := req(t, "GET", hs.URL+p, nil, false); r.StatusCode != 400 && r.StatusCode != 404 {
			t.Errorf("GET %s: %d", p, r.StatusCode)
		}
	}
	if r := req(t, "DELETE", hs.URL+"/cas/"+d.Hash, nil, false); r.StatusCode != 405 {
		t.Fatalf("DELETE: %d", r.StatusCode)
	}
}

func TestParsePathRejectsTraversal(t *testing.T) {
	h := cas.Of(nil).Hash
	ok := []string{"/cas/" + h, "/ac/" + h, "/x/y/cas/" + h}
	bad := []string{"/cas/" + h + "/", "/../cas/" + h, "/cas/" + h[:63], "/cas/" + h + "0", "cas"}
	for _, p := range ok {
		if _, _, good := parsePath(p); !good {
			t.Errorf("rejected %s", p)
		}
	}
	for _, p := range bad {
		if _, _, good := parsePath(p); good {
			t.Errorf("accepted %s", p)
		}
	}
}

func FuzzParsePath(f *testing.F) {
	f.Add("/cas/" + cas.Of(nil).Hash)
	f.Add("/ac/../../" + cas.Of(nil).Hash)
	f.Fuzz(func(t *testing.T, p string) {
		ns, hash, ok := parsePath(p)
		if !ok {
			return
		}
		if (ns != "ac" && ns != "cas") || !cas.ValidHash(hash) {
			t.Fatalf("accepted %q → %q %q", p, ns, hash)
		}
	})
}

func TestClientAgainstServer(t *testing.T) {
	ctx := context.Background()
	_, hs := newServer(t)
	c := NewClient(hs.URL)
	d, err := cas.PutBytes(ctx, c, []byte("via client"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Has(ctx, d); !ok {
		t.Fatal("Has false")
	}
	if b, err := c.Get(ctx, d); err != nil || string(b) != "via client" {
		t.Fatalf("get %v", err)
	}
	if _, err := c.Get(ctx, cas.Of([]byte("x"))); !errors.Is(err, cas.ErrNotFound) {
		t.Fatalf("miss: %v", err)
	}
	ac := actioncache.Typed{Raw: c, CAS: c}
	key := cas.Of([]byte("k")).Hash
	if err := ac.Put(ctx, key, &actioncache.Result{Outputs: []actioncache.OutputFile{{Path: "o", Digest: d}}}); err != nil {
		t.Fatal(err)
	}
	if r, err := ac.Get(ctx, key); err != nil || r.Outputs[0].Digest != d {
		t.Fatalf("ac get %v", err)
	}
}
