package objectstore

import (
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinioConfig configures a MinIO-backed ObjectStore.
type MinioConfig struct {
	// Endpoint is host[:port], without a scheme — e.g. "minio.internal:9000".
	// TLS is controlled separately by Secure, matching how S3Config derives
	// it from the scheme in the agent's configured URL.
	Endpoint string
	Bucket   string
	Region   string
	// Secure enables TLS. Callers derive this from the configured endpoint's
	// URL scheme (https:// → true).
	Secure    bool
	AccessKey string
	SecretKey string
}

// minioStore implements ObjectStore against a MinIO or other S3-compatible
// endpoint using the MinIO SDK.
type minioStore struct {
	client *minio.Client
	bucket string
}

// NewMinioStore creates an ObjectStore backed by cfg. It always uses
// path-style ("BucketLookupPath") addressing rather than virtual-host style:
// self-hosted MinIO and Garage deployments generally don't have the DNS
// wildcard a bucket-subdomain scheme needs, so relying on the SDK's
// auto-detection is unreliable for the providers cara targets.
//
// This does not verify connectivity or that the bucket exists — callers that
// want a fail-fast check should Head a known key or List with an empty
// prefix after construction.
func NewMinioStore(cfg MinioConfig) (ObjectStore, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("objectstore: endpoint must not be empty")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("objectstore: bucket must not be empty")
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       cfg.Secure,
		Region:       cfg.Region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: create client: %w", err)
	}

	return &minioStore{client: client, bucket: cfg.Bucket}, nil
}

func (s *minioStore) Put(ctx context.Context, key string, body io.Reader, opts PutOptions) (ObjectMeta, error) {
	// opts.Size is passed through as-is: -1 means "unknown, stream it", any
	// other value (including 0, for a genuinely empty object) is the exact
	// length. Do not special-case 0 here — that would silently turn a real
	// empty-object upload into an unknown-size streaming upload.
	info, err := s.client.PutObject(ctx, s.bucket, key, body, opts.Size, minio.PutObjectOptions{
		ContentType: opts.ContentType,
	})
	if err != nil {
		return ObjectMeta{}, fmt.Errorf("objectstore: put %q: %w", key, err)
	}
	return ObjectMeta{
		Key:          key,
		ETag:         info.ETag,
		Size:         info.Size,
		LastModified: info.LastModified,
	}, nil
}

func (s *minioStore) Get(ctx context.Context, key string) (io.ReadCloser, ObjectMeta, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, ObjectMeta{}, fmt.Errorf("objectstore: get %q: %w", key, err)
	}
	// GetObject does not itself contact the server; the first read or an
	// explicit Stat does. Stat here so a missing key surfaces immediately as
	// ErrNotFound instead of on the caller's first Read.
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if isNotFound(err) {
			return nil, ObjectMeta{}, fmt.Errorf("objectstore: get %q: %w", key, ErrNotFound)
		}
		return nil, ObjectMeta{}, fmt.Errorf("objectstore: stat %q: %w", key, err)
	}
	return obj, toObjectMeta(info), nil
}

func (s *minioStore) Head(ctx context.Context, key string) (ObjectMeta, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return ObjectMeta{}, fmt.Errorf("objectstore: head %q: %w", key, ErrNotFound)
		}
		return ObjectMeta{}, fmt.Errorf("objectstore: head %q: %w", key, err)
	}
	return toObjectMeta(info), nil
}

func (s *minioStore) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	var out []ObjectMeta
	for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if info.Err != nil {
			return nil, fmt.Errorf("objectstore: list %q: %w", prefix, info.Err)
		}
		out = append(out, toObjectMeta(info))
	}
	return out, nil
}

func (s *minioStore) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		// S3 DELETE is idempotent — removing an already-absent key is not an
		// error — but some providers still surface NoSuchKey; normalise it
		// away rather than leak provider-specific behaviour.
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("objectstore: delete %q: %w", key, err)
	}
	return nil
}

func toObjectMeta(info minio.ObjectInfo) ObjectMeta {
	return ObjectMeta{
		Key:          info.Key,
		ETag:         info.ETag,
		Size:         info.Size,
		LastModified: info.LastModified,
	}
}

// isNotFound reports whether err is the SDK's representation of a missing
// key or bucket, across both StatObject/GetObject-style errors and the
// occasional NoSuchKey some providers report on delete.
func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.StatusCode == 404
}
