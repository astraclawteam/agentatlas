package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

// ObjectStore wraps the S3-compatible bucket that keeps everything too raw
// for metadata tables: uploaded artifacts, full AtlasDocuments, sealed dream
// summaries, keyframes.
type ObjectStore struct {
	client *minio.Client
	bucket string
}

// NewObjectStore dials the S3-compatible object store. tlsMgr configures
// the object-storage link's transport security
// (services/agentatlas/internal/transportsecurity), layered independently
// of cfg.UseSSL/the endpoint scheme (both of which keep controlling whether
// the connection is secured at all; tlsMgr additionally enforces mTLS
// client-identity + peer-pinning when configured). nil, or a Manager built
// with LinkConfig.Mode == ModeOff, keeps today's behavior.
func NewObjectStore(cfg config.ObjectStorage, tlsMgr *transportsecurity.Manager) (*ObjectStore, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("object endpoint: %w", err)
	}
	secure := u.Scheme == "https"
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure || cfg.UseSSL,
		Region: cfg.Region,
	}
	if tlsMgr != nil {
		transport := &http.Transport{}
		if err := tlsMgr.ConfigureTransport(transport); err != nil {
			return nil, fmt.Errorf("object storage tls: %w", err)
		}
		opts.Transport = transport
	}
	client, err := minio.New(u.Host, opts)
	if err != nil {
		return nil, fmt.Errorf("object client: %w", err)
	}
	return &ObjectStore{client: client, bucket: cfg.Bucket}, nil
}

func (s *ObjectStore) EnsureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("bucket check: %w", err)
	}
	if !exists {
		if err := s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("make bucket: %w", err)
		}
	}
	return nil
}

func (s *ObjectStore) Put(ctx context.Context, key, contentType string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

func (s *ObjectStore) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	return data, nil
}
