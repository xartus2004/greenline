package httpcache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/cas"
)

// Client talks to an HTTP cache (greenline's or any Bazel-compatible one)
// and implements both cas.Store and actioncache.Raw.
type Client struct {
	Base string // e.g. http://localhost:9092 or http://host/prefix
	HTTP *http.Client

	hits, misses, puts atomic.Int64
}

// NewClient returns a client for base.
func NewClient(base string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: http.DefaultClient}
}

func (c *Client) url(ns, hash string) string { return c.Base + "/" + ns + "/" + hash }

func (c *Client) do(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	return c.HTTP.Do(req)
}

func (c *Client) get(ctx context.Context, ns, hash string, notFound error) ([]byte, error) {
	if !cas.ValidHash(hash) {
		return nil, cas.ErrBadDigest
	}
	resp, err := c.do(ctx, http.MethodGet, c.url(ns, hash), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		c.misses.Add(1)
		return nil, notFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("httpcache: GET %s/%s: %s", ns, hash, resp.Status)
	}
	c.hits.Add(1)
	return io.ReadAll(resp.Body)
}

func (c *Client) put(ctx context.Context, ns, hash string, b []byte) error {
	if !cas.ValidHash(hash) {
		return cas.ErrBadDigest
	}
	resp, err := c.do(ctx, http.MethodPut, c.url(ns, hash), b)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == http.StatusBadRequest && ns == "cas" {
			return cas.ErrDigestMismatch
		}
		return fmt.Errorf("httpcache: PUT %s/%s: %s", ns, hash, resp.Status)
	}
	c.puts.Add(1)
	return nil
}

// Has implements cas.Store.
func (c *Client) Has(ctx context.Context, d cas.Digest) (bool, error) {
	if err := d.Validate(); err != nil {
		return false, err
	}
	if d == cas.Empty {
		return true, nil
	}
	resp, err := c.do(ctx, http.MethodHead, c.url("cas", d.Hash), nil)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("httpcache: HEAD: %s", resp.Status)
}

// Get implements cas.Store, verifying the content locally.
func (c *Client) Get(ctx context.Context, d cas.Digest) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if d == cas.Empty {
		return []byte{}, nil
	}
	b, err := c.get(ctx, "cas", d.Hash, cas.ErrNotFound)
	if err != nil {
		return nil, err
	}
	if cas.Of(b) != d {
		return nil, cas.ErrNotFound
	}
	return b, nil
}

// GetByHash implements cas.Store.
func (c *Client) GetByHash(ctx context.Context, hash string) ([]byte, error) {
	b, err := c.get(ctx, "cas", hash, cas.ErrNotFound)
	if err != nil {
		return nil, err
	}
	if cas.Of(b).Hash != hash {
		return nil, cas.ErrNotFound
	}
	return b, nil
}

// Put implements cas.Store.
func (c *Client) Put(ctx context.Context, d cas.Digest, r io.Reader) error {
	if err := d.Validate(); err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(r, d.Size+1))
	if err != nil {
		return err
	}
	if cas.Of(b) != d {
		return cas.ErrDigestMismatch
	}
	return c.put(ctx, "cas", d.Hash, b)
}

// Stats implements cas.Store (client-side counters only).
func (c *Client) Stats() cas.Stats {
	return cas.Stats{Hits: c.hits.Load(), Misses: c.misses.Load(), Puts: c.puts.Load()}
}

// GetRaw implements actioncache.Raw.
func (c *Client) GetRaw(ctx context.Context, key string) ([]byte, error) {
	return c.get(ctx, "ac", key, actioncache.ErrNotFound)
}

// PutRaw implements actioncache.Raw.
func (c *Client) PutRaw(ctx context.Context, key string, v []byte) error {
	return c.put(ctx, "ac", key, v)
}
