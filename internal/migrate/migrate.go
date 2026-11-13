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

	names, err := fs.Glob(scripts, "sql/*.sql")
	if err != nil {
		return errors.New("schema migrate failed")
	}
	sort.Strings(names)
	for _, name := range names {
		var one int
		err := conn.QueryRowContext(ctx, "SELECT 1 FROM schema_migrations WHERE version = ?", name).Scan(&one)
		if err == nil {
			continue
		}
		missingTable := err != nil && strings.Contains(err.Error(), "schema_migrations")
		if !errors.Is(err, sql.ErrNoRows) && !missingTable {
			return errors.New("schema migrate failed")
		}
		body, err := scripts.ReadFile(name)
		if err != nil {
			return errors.New("schema migrate failed")
		}
		for _, statement := range splitSQL(string(body)) {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return errors.New("schema migrate failed")
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", name); err != nil {
			return errors.New("schema migrate failed")
		}
	}
	return nil
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
