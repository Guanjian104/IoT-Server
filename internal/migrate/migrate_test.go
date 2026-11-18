package migrate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func TestPartialMigrationRestartsAfterTheLastRecordedStatement(t *testing.T) {
	body, err := scripts.ReadFile("sql/0003_viewer_device.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements := splitSQL(string(body))
	if len(statements) != 4 {
		t.Fatalf("statements = %d", len(statements))
	}
	files := fstest.MapFS{"sql/0003_viewer_device.sql": &fstest.MapFile{Data: body}}
	db := &scriptDB{migrationsTable: true, failStatement: statements[1]}
	if err := apply(context.Background(), db, files); err == nil {
		t.Fatal("partial migration succeeded")
	}
	if got := db.count(statements[0]); got != 1 {
		t.Fatalf("first statement ran %d times", got)
	}
	if got := db.count(statements[1]); got != 1 {
		t.Fatalf("failed statement ran %d times", got)
	}
	if got := db.count(statements[2]); got != 0 || db.count(statements[3]) != 0 {
		t.Fatal("later statements ran before the failure")
	}
	if db.versions["sql/0003_viewer_device.sql"] {
		t.Fatal("version recorded before the file finished")
	}

	db.executed = nil
	db.failStatement = ""
	if err := apply(context.Background(), db, files); err != nil {
		t.Fatal(err)
	}
	if got := db.count(statements[0]); got != 0 {
		t.Fatalf("recorded statement ran again %d times", got)
	}
	if got := db.count(statements[1]); got != 1 || db.count(statements[2]) != 1 || db.count(statements[3]) != 1 {
		t.Fatalf("remaining statements = %q", db.executed)
	}
	if !db.versions["sql/0003_viewer_device.sql"] {
		t.Fatal("version was not recorded")
	}

	db.executed = nil
	if err := apply(context.Background(), db, files); err != nil {
		t.Fatal(err)
	}
	if len(db.executed) != 0 {
		t.Fatalf("finished migration ran %q", db.executed)
	}
}

func TestEarlierFileStaysFinishedWhenALaterFileFails(t *testing.T) {
	files := fstest.MapFS{
		"sql/0001_done.sql": &fstest.MapFile{Data: []byte("CREATE TABLE earlier (id INT)")},
		"sql/0002_next.sql": &fstest.MapFile{Data: []byte("ALTER TABLE later ADD COLUMN a INT;\nALTER TABLE later ADD COLUMN b INT")},
	}
	db := &scriptDB{migrationsTable: true, failStatement: "ADD COLUMN a"}
	if err := apply(context.Background(), db, files); err == nil {
		t.Fatal("later file succeeded")
	}
	db.executed = nil
	db.failStatement = ""
	if err := apply(context.Background(), db, files); err != nil {
		t.Fatal(err)
	}
	if got := db.count("CREATE TABLE earlier (id INT)"); got != 0 {
		t.Fatalf("finished file ran again %d times", got)
	}
	if got := db.count("ALTER TABLE later ADD COLUMN a INT"); got != 1 {
		t.Fatalf("failed statement restarted %d times", got)
	}
	if got := db.count("ALTER TABLE later ADD COLUMN b INT"); got != 1 {
		t.Fatalf("following statement ran %d times", got)
	}
}

type scriptDB struct {
	migrationsTable bool
	versions        map[string]bool
	progress        map[string]int
	executed        []string
	failStatement   string
}

func (d *scriptDB) count(statement string) int {
	n := 0
	for _, item := range d.executed {
		if item == statement {
			n++
		}
	}
	return n
}

func (d *scriptDB) QueryRowContext(_ context.Context, query string, args ...any) row {
	if d.versions == nil {
		d.versions = map[string]bool{}
	}
	if d.progress == nil {
		d.progress = map[string]int{}
	}
	switch {
	case strings.Contains(query, "FROM schema_migrations"):
		if !d.migrationsTable {
			return scriptRow{err: errors.New("Error 1146: Table 'schema_migrations' doesn't exist")}
		}
		if d.versions[args[0].(string)] {
			return scriptRow{values: []any{1}}
		}
		return scriptRow{err: sql.ErrNoRows}
	case strings.Contains(query, "FROM schema_migration_progress"):
		done, ok := d.progress[args[0].(string)]
		if !ok {
			return scriptRow{err: sql.ErrNoRows}
		}
		return scriptRow{values: []any{done}}
	default:
		return scriptRow{err: errors.New("unexpected query")}
	}
}

func (d *scriptDB) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	if d.versions == nil {
		d.versions = map[string]bool{}
	}
	if d.progress == nil {
		d.progress = map[string]int{}
	}
	switch {
	case strings.Contains(query, "CREATE TABLE IF NOT EXISTS schema_migration_progress"):
		return nil, nil
	case strings.Contains(query, "INSERT INTO schema_migration_progress"):
		d.progress[args[0].(string)] = args[1].(int)
		return nil, nil
	case strings.Contains(query, "INSERT INTO schema_migrations"):
		d.versions[args[0].(string)] = true
		return nil, nil
	default:
		if strings.Contains(query, "CREATE TABLE IF NOT EXISTS schema_migrations") {
			d.migrationsTable = true
		}
		d.executed = append(d.executed, query)
		if d.failStatement != "" && strings.Contains(query, d.failStatement) {
			return nil, errors.New("statement failed")
		}
		return nil, nil
	}
}

type scriptRow struct {
	values []any
	err    error
}

func (r scriptRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("scan width")
	}
	for i := range dest {
		switch out := dest[i].(type) {
		case *int:
			*out = r.values[i].(int)
		default:
			return errors.New("scan type")
		}
	}
	return nil
}
