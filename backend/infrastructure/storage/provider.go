// Package storage defines the StorageProvider boundary for future media
// (product images, Status screenshots, receipts). Per PRD 0.2 §14 and
// PRD 0 FINAL §18: no MinIO/object-storage deployment is required in
// Phase 0 — only the interface needs to exist.
package storage

import "context"

type Provider interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	GenerateURL(ctx context.Context, key string) (string, error)
}
