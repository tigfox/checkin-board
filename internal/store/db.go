package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// pragmas apply to every connection. synchronous=FULL because nodes are
// battery-powered Pis on SD cards: a confirmed write must survive a
// power cut. foreign_keys enforces the void_of / batch_id references.
var pragmas = []string{
	"foreign_keys(1)",
	"journal_mode(WAL)",
	"synchronous(FULL)",
	"busy_timeout(5000)",
}

var memSeq atomic.Uint64

// Open opens (creating if needed) the SQLite database at path and
// applies pending migrations.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: database path is required")
	}
	return open("file:" + path + "?" + pragmaQuery())
}

// OpenMemory opens a private in-memory database, for tests. Each call
// gets its own database.
func OpenMemory() (*Store, error) {
	name := "mem" + strconv.FormatUint(memSeq.Add(1), 10)
	return open("file:" + name + "?mode=memory&cache=shared&" + pragmaQuery())
}

func pragmaQuery() string {
	q := make([]string, len(pragmas))
	for i, p := range pragmas {
		q[i] = "_pragma=" + url.QueryEscape(p)
	}
	return strings.Join(q, "&")
}

func open(dsn string) (*Store, error) {
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:  logger.Discard,
		NowFunc: func() time.Time { return normTime(time.Now()) },
	})
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// One connection serializes writes (SQLite allows one writer anyway)
	// and keeps an in-memory database alive for the Store's lifetime.
	sqlDB.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// migrate applies every embedded migration newer than the recorded
// schema version, each in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	db := s.db.WithContext(ctx)
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL
	)`).Error; err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	var current int
	if err := db.Raw(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current).Error; err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	files, err := migrationFiles()
	if err != nil {
		return err
	}
	for _, m := range files {
		if m.version <= current {
			continue
		}
		body, err := fs.ReadFile(migrationFS, m.path)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", m.path, err)
		}
		err = db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(string(body)).Error; err != nil {
				return err
			}
			return tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
				m.version, normTime(s.now())).Error
		})
		if err != nil {
			return fmt.Errorf("store: apply %s: %w", m.path, err)
		}
	}
	return nil
}

type migration struct {
	version int
	path    string
}

// migrationFiles lists migrations/NNNN_name.sql in version order.
func migrationFiles() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: list migrations: %w", err)
	}
	out := make([]migration, 0, len(entries))
	seen := map[int]string{}
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("store: migration %q must be named NNNN_name.sql", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", prev, e.Name(), v)
		}
		seen[v] = e.Name()
		out = append(out, migration{version: v, path: "migrations/" + e.Name()})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	return out, nil
}

// SchemaVersion returns the highest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.WithContext(ctx).Raw(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v).Error
	return v, err
}

// DBSize is the database's size in bytes (page count × page size), for
// soak tests and diagnostics.
func (s *Store) DBSize(ctx context.Context) (int64, error) {
	var pages, size int64
	if err := s.db.WithContext(ctx).Raw("PRAGMA page_count").Scan(&pages).Error; err != nil {
		return 0, err
	}
	if err := s.db.WithContext(ctx).Raw("PRAGMA page_size").Scan(&size).Error; err != nil {
		return 0, err
	}
	return pages * size, nil
}
