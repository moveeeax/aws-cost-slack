package state

import (
	"context"
	"fmt"
	"strings"

	"github.com/moveeeax/aws-cost-slack/internal/costs"
)

// Store loads and saves the alert state at one location.
type Store interface {
	Load(ctx context.Context) (costs.AlertState, error)
	Save(ctx context.Context, st costs.AlertState) error
	String() string
}

// IsS3 reports whether location is an s3://bucket/key URI rather than a local
// path.
func IsS3(location string) bool {
	return strings.HasPrefix(location, "s3://")
}

// ParseS3URI splits s3://bucket/key into bucket and key. The key must name an
// object, not a prefix.
func ParseS3URI(uri string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(uri, "s3://")
	if !ok {
		return "", "", fmt.Errorf("state location %q is not an s3:// URI", uri)
	}
	bucket, key, _ = strings.Cut(rest, "/")
	if bucket == "" || key == "" || strings.HasSuffix(key, "/") {
		return "", "", fmt.Errorf("state location %q: want s3://bucket/key", uri)
	}
	return bucket, key, nil
}

// Open returns the store for location: an S3 object for an s3:// URI (using
// api), a local file otherwise. api may be nil for a local path.
func Open(location string, api S3API) (Store, error) {
	if !IsS3(location) {
		return &File{Path: location}, nil
	}
	bucket, key, err := ParseS3URI(location)
	if err != nil {
		return nil, err
	}
	if api == nil {
		return nil, fmt.Errorf("state location %s: no S3 client", location)
	}
	return &S3{API: api, Bucket: bucket, Key: key}, nil
}

// File is a state file on local disk.
type File struct {
	Path string
}

func (f *File) Load(context.Context) (costs.AlertState, error) { return Load(f.Path) }

func (f *File) Save(_ context.Context, st costs.AlertState) error { return Save(f.Path, st) }

func (f *File) String() string { return f.Path }
