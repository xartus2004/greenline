// Package cas implements a content-addressed blob store keyed by sha256
// digest, following the Bazel Remote Execution API digest model
// (hash + size).
package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// HashLen is the length of a hex-encoded sha256.
const HashLen = 64

// Digest identifies a blob: its lowercase hex sha256 and its size in bytes.
type Digest struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// Empty is the digest of the zero-length blob.
var Empty = Of(nil)

// ErrBadDigest is returned for malformed hashes or sizes.
var ErrBadDigest = errors.New("cas: malformed digest")

// Of computes the digest of b.
func Of(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest{Hash: hex.EncodeToString(sum[:]), Size: int64(len(b))}
}

// OfReader computes the digest of everything read from r.
func OfReader(r io.Reader) (Digest, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return Digest{}, err
	}
	return Digest{Hash: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

func (d Digest) String() string { return d.Hash + "/" + strconv.FormatInt(d.Size, 10) }

// IsZero reports whether d is the zero value (not the empty blob).
func (d Digest) IsZero() bool { return d.Hash == "" && d.Size == 0 }

// ValidHash reports whether s is exactly 64 lowercase hex characters. This is
// the only form accepted anywhere a hash becomes part of a filesystem path or
// URL, so it is also the path-traversal guard.
func ValidHash(s string) bool {
	if len(s) != HashLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Validate checks that d is well formed.
func (d Digest) Validate() error {
	if !ValidHash(d.Hash) {
		return fmt.Errorf("%w: hash %q", ErrBadDigest, d.Hash)
	}
	if d.Size < 0 {
		return fmt.Errorf("%w: negative size %d", ErrBadDigest, d.Size)
	}
	return nil
}

// Parse parses "hash/size" (the form produced by String).
func Parse(s string) (Digest, error) {
	h, sz, ok := strings.Cut(s, "/")
	if !ok {
		return Digest{}, fmt.Errorf("%w: %q missing size", ErrBadDigest, s)
	}
	n, err := strconv.ParseInt(sz, 10, 64)
	if err != nil || sz == "" || sz[0] == '+' || sz[0] == '-' {
		return Digest{}, fmt.Errorf("%w: size %q", ErrBadDigest, sz)
	}
	d := Digest{Hash: h, Size: n}
	return d, d.Validate()
}
