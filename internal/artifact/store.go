package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config configures a Store.
type Config struct {
	Endpoint  string // host:port, no scheme — e.g. "localhost:9000"
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool
}

func (c Config) validate() error {
	for name, v := range map[string]bool{
		"Endpoint": c.Endpoint == "", "Bucket": c.Bucket == "",
		"AccessKey": c.AccessKey == "", "SecretKey": c.SecretKey == "",
	} {
		if v {
			return fmt.Errorf("artifact: config: %s is required", name)
		}
	}
	return nil
}

// Store persists job workspace archives to S3-compatible object
// storage and serves them back. A job's artifact is preserved on
// SUCCEEDED, FAILED, and CANCELLED alike (ADR-012: failure preserves
// the artifact) — Upload takes no opinion on why the job ended.
type Store struct {
	client *minio.Client
	bucket string
}

// New constructs a Store from cfg, or returns an error if cfg is
// invalid or the bucket cannot be ensured to exist.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("artifact: construct client: %w", err)
	}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("artifact: check bucket %s: %w", cfg.Bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("artifact: create bucket %s: %w", cfg.Bucket, err)
		}
	}

	return &Store{client: client, bucket: cfg.Bucket}, nil
}

// objectKey is jobID's archive path within the bucket — one object
// per job, overwritten if a job is somehow uploaded twice (a crash
// between upload and the job's terminal-status write would otherwise
// leave an orphaned prior attempt's object with the same key anyway).
func objectKey(jobID uuid.UUID) string {
	return jobID.String() + "/workspace.tar.gz"
}

// Upload streams r (a tar archive of a job's workspace) into object
// storage under a key derived from jobID and returns that key. size
// is the archive's exact byte length — required by the S3 PutObject
// API to stream without buffering the whole archive in memory first.
func (s *Store) Upload(ctx context.Context, jobID uuid.UUID, r io.Reader, size int64) (string, error) {
	key := objectKey(jobID)
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: "application/gzip"})
	if err != nil {
		return "", fmt.Errorf("artifact: upload job %s: %w", jobID, err)
	}
	return key, nil
}

// PresignedDownloadURL returns a time-limited URL a client can
// download jobID's archive from directly, bypassing the control plane
// entirely for the transfer itself (PRD §11.2: GET /jobs/{id}/artifact
// responds 302 to exactly this URL).
func (s *Store) PresignedDownloadURL(ctx context.Context, jobID uuid.UUID, expiry time.Duration) (string, error) {
	u, err := s.client.PresignedGetObject(ctx, s.bucket, objectKey(jobID), expiry, nil)
	if err != nil {
		return "", fmt.Errorf("artifact: presigned download url for job %s: %w", jobID, err)
	}
	return u.String(), nil
}

// ErrNotFound means jobID has no uploaded artifact.
var ErrNotFound = errors.New("artifact: not found")

// Download returns a reader over jobID's archive. The caller must
// close it.
func (s *Store) Download(ctx context.Context, jobID uuid.UUID) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, objectKey(jobID), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: download job %s: %w", jobID, err)
	}
	// GetObject succeeding is not proof the object exists — MinIO
	// defers the actual HEAD until the first read, so a Stat call is
	// the only way to turn a missing-key error into ErrNotFound before
	// the caller starts streaming a response body around it.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		var errResp minio.ErrorResponse
		if errors.As(err, &errResp) && errResp.Code == "NoSuchKey" {
			return nil, fmt.Errorf("artifact: download job %s: %w", jobID, ErrNotFound)
		}
		return nil, fmt.Errorf("artifact: stat job %s: %w", jobID, err)
	}
	return obj, nil
}
