// Package artifact persists and serves back a job's workspace as one
// tar archive in S3-compatible object storage (MinIO locally, R2 in
// production — PRD §8.2). It is deliberately a thin wrapper: Store is
// the only exported type, with Upload and Download as its two entry
// points.
package artifact
