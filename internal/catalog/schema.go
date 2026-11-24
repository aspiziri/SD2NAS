package catalog

// migrations[i] upgrades a database from user_version i to i+1. Append only;
// never edit a migration that has shipped.
var migrations = []string{schemaV1, schemaV2}

const schemaV1 = `
CREATE TABLE ingests (
  id             INTEGER PRIMARY KEY,
  card_path      TEXT NOT NULL,
  card_label     TEXT NOT NULL DEFAULT '',
  card_uuid      TEXT NOT NULL DEFAULT '',     -- filesystem UUID (the reader's SERIAL is shared by every card)
  card_size      INTEGER NOT NULL DEFAULT 0,   -- bytes of media files found
  started_at     TEXT NOT NULL,
  finished_at    TEXT,
  status         TEXT NOT NULL CHECK (status IN ('queued','running','done','failed','refused','cancelled')),
  seen           INTEGER NOT NULL DEFAULT 0,
  new            INTEGER NOT NULL DEFAULT 0,
  in_archive     INTEGER NOT NULL DEFAULT 0,
  in_staging     INTEGER NOT NULL DEFAULT 0,
  seen_before    INTEGER NOT NULL DEFAULT 0,
  failed         INTEGER NOT NULL DEFAULT 0,
  unsupported    INTEGER NOT NULL DEFAULT 0,
  unsupported_exts TEXT NOT NULL DEFAULT '', -- JSON array of the unsupported files' distinct extensions; '' = none
  clock_warnings INTEGER NOT NULL DEFAULT 0,
  safe_to_format INTEGER NOT NULL DEFAULT 0,
  error          TEXT NOT NULL DEFAULT ''
);

-- One row per unique file content ever seen, whether imported or indexed.
CREATE TABLE assets (
  sha256        BLOB PRIMARY KEY CHECK (length(sha256) = 32),
  size          INTEGER NOT NULL,
  kind          TEXT NOT NULL CHECK (kind IN ('image','raw','video')),
  capture_time  TEXT,                         -- local wall clock as named, 2006-01-02T15:04:05
  tz_offset     TEXT NOT NULL DEFAULT '',     -- EXIF OffsetTimeOriginal, if any
  state         TEXT NOT NULL CHECK (state IN ('staged','archived','culled','missing')),
  rel_path      TEXT,                         -- slash-separated, relative to /photos; NULL when culled
  mtime         INTEGER,                      -- UnixNano of the stored file, for cheap rescans
  first_ingest  INTEGER REFERENCES ingests(id),  -- NULL if found by the indexer
  updated_at    TEXT NOT NULL
);
CREATE INDEX assets_path ON assets(rel_path);

-- Quick fingerprints, many-to-one, never deleted (§6 "aliases").
CREATE TABLE fingerprints (
  quick_fp  BLOB NOT NULL CHECK (length(quick_fp) = 32),
  sha256    BLOB NOT NULL REFERENCES assets(sha256),
  PRIMARY KEY (quick_fp, sha256)
);

-- Content that used to be an asset but changed in place.
CREATE TABLE aliases (
  old_sha256 BLOB PRIMARY KEY CHECK (length(old_sha256) = 32),
  sha256     BLOB NOT NULL REFERENCES assets(sha256),
  seen_at    TEXT NOT NULL
);

-- What happened to each file on the card.
CREATE TABLE ingest_files (
  ingest_id  INTEGER NOT NULL REFERENCES ingests(id),
  card_path  TEXT NOT NULL,                   -- slash-separated, relative to the card root
  size       INTEGER NOT NULL,
  quick_fp   BLOB,
  sha256     BLOB,
  outcome    TEXT NOT NULL CHECK (outcome IN ('new','in_archive','in_staging','seen_before','failed','unsupported')),
  error      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX ingest_files_ingest ON ingest_files(ingest_id);

CREATE TABLE clock_warnings (
  ingest_id  INTEGER NOT NULL REFERENCES ingests(id),
  card_path  TEXT NOT NULL,
  sha256     BLOB,
  kind       TEXT NOT NULL CHECK (kind IN ('future','reset_default','gps_mismatch')),
  detail     TEXT NOT NULL
);
CREATE INDEX clock_warnings_ingest ON clock_warnings(ingest_id);

CREATE TABLE staging_folders (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,            -- directory name under Staging/
  dates      TEXT NOT NULL,                   -- JSON array of YYYY-MM-DD it covers, sorted
  created_at TEXT NOT NULL
);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// schemaV2 (M4): "Forget this import" marks the ingest instead of deleting
// it, and forgetting finds an import's assets by first_ingest.
const schemaV2 = `
ALTER TABLE ingests ADD COLUMN forgotten_at TEXT;   -- set by "Forget this import"; NULL = never
CREATE INDEX assets_first_ingest ON assets(first_ingest);
-- Forgetting deletes per asset: keep those deletes (and the foreign-key checks) off full scans.
CREATE INDEX fingerprints_sha256 ON fingerprints(sha256);
CREATE INDEX aliases_sha256 ON aliases(sha256);

-- Folder ids are never reused (a lock or a waiter may still hold a deleted
-- folder's id): rebuild staging_folders with AUTOINCREMENT, keeping every id.
CREATE TABLE staging_folders_new (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL UNIQUE,
  dates      TEXT NOT NULL,
  created_at TEXT NOT NULL
);
INSERT INTO staging_folders_new (id, name, dates, created_at) SELECT id, name, dates, created_at FROM staging_folders;
DROP TABLE staging_folders;
ALTER TABLE staging_folders_new RENAME TO staging_folders;
`
