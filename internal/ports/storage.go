package ports

import "context"

type StorageSnapshot struct {
	SchemaVersion int
	Tables        map[string]int64
}

type Storage interface {
	Ping(context.Context) error
	Snapshot(context.Context) (StorageSnapshot, error)
	Close() error
}
