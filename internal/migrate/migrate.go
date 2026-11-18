// Package migrate 把嵌入的 SQL 应用到当前进程连接的数据库。
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"sort"
	"strings"
)

//go:embed sql/*.sql
var scripts embed.FS

// Up 在同一条连接上持有命名锁，按文件名顺序执行尚未记录的脚本。
func Up(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("schema migrate failed")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return errors.New("schema migrate failed")
	}
	defer conn.Close()
	var lock sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 30)", "iot_dev_schema").Scan(&lock); err != nil || !lock.Valid || lock.Int64 != 1 {
		return errors.New("schema migrate failed")
	}
	defer conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", "iot_dev_schema")
	return apply(ctx, connRunner{conn}, scripts)
}

type runner interface {
	QueryRowContext(ctx context.Context, query string, args ...any) row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type row interface {
	Scan(dest ...any) error
}

type connRunner struct {
	conn *sql.Conn
}

func (c connRunner) QueryRowContext(ctx context.Context, query string, args ...any) row {
	return c.conn.QueryRowContext(ctx, query, args...)
}

func (c connRunner) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return c.conn.ExecContext(ctx, query, args...)
}

// apply 按文件顺序执行脚本。每条语句成功后立刻记下序号。
// 中途失败再跑时从下一条开始，已经记下的语句不会再执行。文件版本仍在全部语句成功后才记录。
func apply(ctx context.Context, db runner, files fs.FS) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migration_progress (
		  version VARCHAR(64) NOT NULL,
		  statement_index INT NOT NULL,
		  PRIMARY KEY (version)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`); err != nil {
		return errors.New("schema migrate failed")
	}
	names, err := fs.Glob(files, "sql/*.sql")
	if err != nil {
		return errors.New("schema migrate failed")
	}
	sort.Strings(names)
	for _, name := range names {
		var one int
		err := db.QueryRowContext(ctx, "SELECT 1 FROM schema_migrations WHERE version = ?", name).Scan(&one)
		if err == nil {
			continue
		}
		missingTable := err != nil && strings.Contains(err.Error(), "schema_migrations")
		if !errors.Is(err, sql.ErrNoRows) && !missingTable {
			return errors.New("schema migrate failed")
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return errors.New("schema migrate failed")
		}
		statements := splitSQL(string(body))
		done, err := recordedStatements(ctx, db, name)
		if err != nil {
			return err
		}
		if done > len(statements) {
			done = len(statements)
		}
		for i := done; i < len(statements); i++ {
			if _, err := db.ExecContext(ctx, statements[i]); err != nil {
				return errors.New("schema migrate failed")
			}
			if _, err := db.ExecContext(ctx, `
				INSERT INTO schema_migration_progress (version, statement_index) VALUES (?, ?)
				ON DUPLICATE KEY UPDATE statement_index = ?`, name, i+1, i+1); err != nil {
				return errors.New("schema migrate failed")
			}
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", name); err != nil {
			return errors.New("schema migrate failed")
		}
	}
	return nil
}

func recordedStatements(ctx context.Context, db runner, name string) (int, error) {
	var done int
	err := db.QueryRowContext(ctx, "SELECT statement_index FROM schema_migration_progress WHERE version = ?", name).Scan(&done)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, errors.New("schema migrate failed")
	}
	if done < 0 {
		return 0, nil
	}
	return done, nil
}

func splitSQL(body string) []string {
	var cleaned strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		cleaned.WriteString(line)
		cleaned.WriteByte('\n')
	}
	var out []string
	for _, part := range strings.Split(cleaned.String(), ";") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
