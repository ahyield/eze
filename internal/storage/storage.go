// Package storage opens the bucket that holds deployed sites and uploads when EZE_STORAGE
// points at S3 or Google Cloud Storage.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/gcsblob"
	_ "gocloud.dev/blob/s3blob"
	"gocloud.dev/gcerrors"
)

// Open connects to the bucket at rawURL and checks that eze can write, read and delete in it.
func Open(ctx context.Context, rawURL string) (*blob.Bucket, error) {
	b, err := blob.OpenBucket(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", Describe(rawURL), err)
	}
	if err := probe(ctx, b); err != nil {
		b.Close()
		return nil, fmt.Errorf("checking %s: %w", Describe(rawURL), err)
	}
	return b, nil
}

const probeKey = ".eze-probe"

func probe(ctx context.Context, b *blob.Bucket) error {
	want := []byte("ok")
	if err := b.WriteAll(ctx, probeKey, want, nil); err != nil {
		return fmt.Errorf("writing a test object: %w", err)
	}
	got, err := b.ReadAll(ctx, probeKey)
	if err != nil {
		return fmt.Errorf("reading a test object: %w", err)
	}
	if !bytes.Equal(got, want) {
		return errors.New("a test object read back different bytes")
	}
	if err := b.Delete(ctx, probeKey); err != nil {
		return fmt.Errorf("deleting a test object: %w", err)
	}
	return nil
}

// Describe names a bucket URL for logs and errors without its query, which can hold endpoints or options.
func Describe(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "storage bucket"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// IsNotFound reports whether err means the object doesn't exist.
func IsNotFound(err error) bool { return gcerrors.Code(err) == gcerrors.NotFound }

// DeletePrefix deletes every object whose key starts with prefix.
func DeletePrefix(ctx context.Context, b *blob.Bucket, prefix string) error {
	it := b.List(&blob.ListOptions{Prefix: prefix})
	for {
		obj, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := b.Delete(ctx, obj.Key); err != nil && !IsNotFound(err) {
			return err
		}
	}
}

// NewReader returns a reader for an object of a known size that only fetches from the
// bucket once it is read, reopening at the right offset after a Seek. It lets
// http.ServeContent answer Range requests with a ranged GET instead of the whole object.
func NewReader(ctx context.Context, b *blob.Bucket, key string, size int64) io.ReadSeekCloser {
	return &reader{ctx: ctx, b: b, key: key, size: size}
}

type reader struct {
	ctx  context.Context
	b    *blob.Bucket
	key  string
	size int64
	off  int64
	r    *blob.Reader // open at off, or nil
}

func (r *reader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	if r.r == nil {
		rr, err := r.b.NewRangeReader(r.ctx, r.key, r.off, -1, nil)
		if err != nil {
			return 0, err
		}
		r.r = rr
	}
	n, err := r.r.Read(p)
	r.off += int64(n)
	return n, err
}

func (r *reader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += r.off
	case io.SeekEnd:
		offset += r.size
	}
	if offset < 0 {
		return 0, errors.New("storage: seek before start of object")
	}
	if offset != r.off {
		r.closeReader()
		r.off = offset
	}
	return offset, nil
}

func (r *reader) closeReader() {
	if r.r != nil {
		r.r.Close()
		r.r = nil
	}
}

func (r *reader) Close() error {
	r.closeReader()
	return nil
}
