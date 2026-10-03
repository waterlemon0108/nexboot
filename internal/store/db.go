package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func Open(ctx context.Context, dsn string) (*SQLStore, error) {
	driver, dialect, err := driverForDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if driver == "sqlite" {
		db.SetMaxOpenConns(1)
		// WAL 让读者（健康探针、状态页、VACUUM INTO）与写者共存，busy timeout 把短暂冲突变成短暂等待而不是立即 SQLITE_BUSY。
		// journal_mode 是持久的，timeout 按连接生效，每次打开都重设一遍很便宜。
		for _, pragma := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", "PRAGMA busy_timeout = 5000"} {
			if _, err := db.ExecContext(ctx, pragma); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("%s: %w", pragma, err)
			}
		}
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate(db, dialect); err != nil {
		_ = db.Close()
		return nil, err
	}
	return New(db, dialect), nil
}

func migrate(db *sql.DB, dialect string) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect(dialect); err != nil {
		return err
	}
	return goose.Up(db, "migrations")
}

func driverForDSN(dsn string) (driver string, dialect string, err error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return "pgx", "postgres", nil
	}
	if strings.HasPrefix(dsn, "file:") || strings.HasPrefix(dsn, ":memory:") || strings.HasSuffix(dsn, ".db") {
		return "sqlite", "sqlite3", nil
	}
	return "", "", fmt.Errorf("unsupported database DSN %q", dsn)
}

// Snapshot 用 VACUUM INTO 把在用库复制到 path：事务性的时间点副本，不需要锁住写者。
func (s *SQLStore) Snapshot(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("snapshot path is required")
	}
	// 不能先删旧的再写：这份副本是备机接管的前提，而角色切换本身就是一次重启，最容易在写的途中被打断。
	// 写到临时文件、成功后原子改名。临时名必须唯一：复制和备份会同时写同一个副本，固定名会互删对方写到一半的文件。
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_ = f.Close()
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("database snapshot failed: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
