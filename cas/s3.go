package cas

import (
	"context"
	"errors"
	"io"
)

// ErrUnimplemented is returned by the S3 stub.
var ErrUnimplemented = errors.New("cas: S3 backend not implemented")

// S3 is a placeholder for an S3-compatible backend. It exists to show the
// Store interface is backend-agnostic; every method returns ErrUnimplemented.
type S3 struct{ Bucket, Prefix, Endpoint string }

func (S3) Has(context.Context, Digest) (bool, error)         { return false, ErrUnimplemented }
func (S3) Get(context.Context, Digest) ([]byte, error)       { return nil, ErrUnimplemented }
func (S3) GetByHash(context.Context, string) ([]byte, error) { return nil, ErrUnimplemented }
func (S3) Put(context.Context, Digest, io.Reader) error      { return ErrUnimplemented }
func (S3) Stats() Stats                                      { return Stats{} }
