package media

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/google/uuid"
)

const MaxAssetBytes = 100 * 1024 * 1024

var safeFilename = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type Store struct {
	db   *sql.DB
	root string
}

func NewStore(db *sql.DB, databasePath string) *Store {
	return &Store{db: db, root: filepath.Join(filepath.Dir(databasePath), "media-assets")}
}

func (s *Store) ListMedia(ctx context.Context, query ports.MediaQuery) ([]ports.MediaAsset, int, error) {
	where := []string{"actor = ?"}
	args := []any{query.Actor}
	if query.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, query.Kind)
	}
	if query.Channel != "" {
		where = append(where, "channel = ?")
		args = append(args, query.Channel)
	}
	if query.Search != "" {
		value := "%" + query.Search + "%"
		where = append(where, "(filename LIKE ? OR model LIKE ? OR channel LIKE ?)")
		args = append(args, value, value, value)
	}
	whereSQL := " WHERE " + strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM media_assets"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page := query.Page
	if page < 1 {
		page = 1
	}
	size := query.PageSize
	if size < 1 {
		size = 24
	}
	if size > 200 {
		size = 200
	}
	args = append(args, size, (page-1)*size)
	rows, err := s.db.QueryContext(ctx, `SELECT id, actor, run_id, conversation_id, channel, model, kind, mime_type, filename, storage_path, source_url, size_bytes, metadata, created_at, updated_at FROM media_assets`+whereSQL+" ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := []ports.MediaAsset{}
	for rows.Next() {
		item, err := scanAsset(rows)
		if err != nil {
			return nil, 0, err
		}
		result = append(result, item)
	}
	return result, total, rows.Err()
}

func (s *Store) GetMedia(ctx context.Context, id, actor string) (ports.MediaAsset, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, actor, run_id, conversation_id, channel, model, kind, mime_type, filename, storage_path, source_url, size_bytes, metadata, created_at, updated_at FROM media_assets WHERE id = ? AND actor = ?`, id, actor)
	item, err := scanAsset(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.MediaAsset{}, ports.ErrKeyNotFound
	}
	return item, err
}

func (s *Store) DeleteMedia(ctx context.Context, id, actor string, audit ports.AuditEvent) error {
	asset, err := s.GetMedia(ctx, id, actor)
	if err != nil {
		return err
	}
	if path, ok := s.MediaContentPath(asset); ok {
		_ = os.Remove(path)
		_ = os.Remove(filepath.Dir(path))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM media_assets WHERE id = ? AND actor = ?", id, actor); err != nil {
		_ = tx.Rollback()
		return err
	}
	audit.Target = id
	if audit.Action == "" {
		audit.Action = "delete_media"
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_logs(ts,actor,action,target,detail,ip) VALUES(unixepoch(),?,?,?,?,?)", audit.Actor, audit.Action, audit.Target, audit.Detail, audit.IP); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) AddMediaBytes(ctx context.Context, asset ports.MediaAsset, data []byte) (ports.MediaAsset, error) {
	if len(data) == 0 || len(data) > MaxAssetBytes {
		return ports.MediaAsset{}, fmt.Errorf("media asset exceeds size limit")
	}
	if asset.ID == "" {
		asset.ID = "asset_" + uuid.NewString()
	}
	if asset.Filename == "" {
		asset.Filename = "asset.bin"
	}
	asset.Filename = safeFilename.ReplaceAllString(filepath.Base(asset.Filename), "-")
	if asset.Filename == "" {
		asset.Filename = "asset.bin"
	}
	if err := os.MkdirAll(filepath.Join(s.root, asset.ID), 0750); err != nil {
		return ports.MediaAsset{}, err
	}
	path := filepath.Join(s.root, asset.ID, asset.Filename)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return ports.MediaAsset{}, err
	}
	relative, _ := filepath.Rel(s.root, path)
	asset.StoragePath = ptrString(relative)
	asset.SizeBytes = int64(len(data))
	asset.CreatedAt = time.Now().Unix()
	asset.UpdatedAt = asset.CreatedAt
	if err := s.insertMedia(ctx, asset); err != nil {
		_ = os.Remove(path)
		_ = os.Remove(filepath.Dir(path))
		return ports.MediaAsset{}, err
	}
	return asset, nil
}

func (s *Store) AddMediaRemote(ctx context.Context, asset ports.MediaAsset, sourceURL string) (ports.MediaAsset, error) {
	sourceURL = strings.TrimSpace(sourceURL)
	parsed, err := url.Parse(sourceURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ports.MediaAsset{}, fmt.Errorf("media source URL is invalid")
	}
	if asset.ID == "" {
		asset.ID = "asset_" + uuid.NewString()
	}
	if asset.Filename == "" {
		asset.Filename = "asset.bin"
	}
	asset.Filename = safeFilename.ReplaceAllString(filepath.Base(asset.Filename), "-")
	if asset.Filename == "" {
		asset.Filename = "asset.bin"
	}
	asset.StoragePath = nil
	asset.SourceURL = ptrString(sourceURL)
	asset.SizeBytes = 0
	asset.CreatedAt = time.Now().Unix()
	asset.UpdatedAt = asset.CreatedAt
	if asset.Metadata == nil {
		asset.Metadata = map[string]any{}
	}
	if _, exists := asset.Metadata["storage_status"]; !exists {
		asset.Metadata["storage_status"] = "remote"
	}
	if err := s.insertMedia(ctx, asset); err != nil {
		return ports.MediaAsset{}, err
	}
	return asset, nil
}

func (s *Store) insertMedia(ctx context.Context, asset ports.MediaAsset) error {
	if asset.Metadata == nil {
		asset.Metadata = map[string]any{}
	}
	metadata, _ := json.Marshal(asset.Metadata)
	_, err := s.db.ExecContext(ctx, `INSERT INTO media_assets(id,actor,run_id,conversation_id,channel,model,kind,mime_type,filename,storage_path,source_url,size_bytes,metadata,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, asset.ID, asset.Actor, asset.RunID, asset.ConversationID, asset.Channel, asset.Model, asset.Kind, asset.MIMEType, asset.Filename, nullableString(asset.StoragePath), asset.SourceURL, asset.SizeBytes, string(metadata), asset.CreatedAt, asset.UpdatedAt)
	return err
}

func (s *Store) MediaContentPath(asset ports.MediaAsset) (string, bool) {
	if asset.StoragePath == nil || *asset.StoragePath == "" {
		return "", false
	}
	root, _ := filepath.Abs(s.root)
	path, _ := filepath.Abs(filepath.Join(s.root, *asset.StoragePath))
	if path == root || !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return "", false
	}
	if stat, err := os.Stat(path); err != nil || stat.IsDir() {
		return "", false
	}
	return path, true
}

type scanner interface{ Scan(...any) error }

func scanAsset(row scanner) (ports.MediaAsset, error) {
	var item ports.MediaAsset
	var metadata string
	var storage, source sql.NullString
	if err := row.Scan(&item.ID, &item.Actor, &item.RunID, &item.ConversationID, &item.Channel, &item.Model, &item.Kind, &item.MIMEType, &item.Filename, &storage, &source, &item.SizeBytes, &metadata, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return ports.MediaAsset{}, err
	}
	if storage.Valid {
		item.StoragePath = &storage.String
	}
	if source.Valid {
		item.SourceURL = &source.String
	}
	if json.Unmarshal([]byte(metadata), &item.Metadata) != nil {
		item.Metadata = map[string]any{}
	}
	return item, nil
}
func ptrString(value string) *string { return &value }

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
