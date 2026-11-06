-- Add public-facing model identity columns. Backfill and unique index are
-- applied by the Go migration hook (slug conflicts need per-row resolution).
ALTER TABLE models ADD COLUMN public_id TEXT NOT NULL DEFAULT '';
ALTER TABLE models ADD COLUMN display_name_locked INTEGER NOT NULL DEFAULT 0;
ALTER TABLE models ADD COLUMN public_id_locked INTEGER NOT NULL DEFAULT 0;
