package db

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is what queries run through: a pool, or one schema of a shared pool.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Acquirer hands out a connection; migrations hold one throughout.
type Acquirer interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// pathKey is where a connection records the search_path it last set.
const pathKey = "search_path"

// Schema runs queries on a shared pool with search_path set to one schema,
// so a hub's tenants share connections instead of each holding a pool.
type Schema struct {
	pool  *pgxpool.Pool
	ident string // quoted schema name
}

// NewSchema scopes pool to schema.
func NewSchema(pool *pgxpool.Pool, schema string) *Schema {
	return &Schema{pool: pool, ident: pgx.Identifier{schema}.Sanitize()}
}

// Acquire returns a connection whose search_path is the schema, setting it
// only when the connection last served another one.
func (s *Schema) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	c, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	data := c.Conn().PgConn().CustomData()
	if data[pathKey] == s.ident {
		return c, nil
	}
	delete(data, pathKey)
	if _, err := c.Exec(ctx, "SET search_path TO "+s.ident); err != nil {
		c.Release()
		return nil, err
	}
	data[pathKey] = s.ident
	return c, nil
}

func (s *Schema) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c, err := s.Acquire(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer c.Release()
	return c.Exec(ctx, sql, args...)
}

func (s *Schema) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c, err := s.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		c.Release()
		return nil, err
	}
	return &schemaRows{Rows: rows, c: c}, nil
}

func (s *Schema) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c, err := s.Acquire(ctx)
	if err != nil {
		return errRow{err}
	}
	return &schemaRow{row: c.QueryRow(ctx, sql, args...), c: c}
}

func (s *Schema) Begin(ctx context.Context) (pgx.Tx, error) {
	c, err := s.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		c.Release()
		return nil, err
	}
	return &schemaTx{Tx: tx, c: c}, nil
}

// schemaRows releases its connection once read to the end or closed, like
// the pool's own rows.
type schemaRows struct {
	pgx.Rows
	c    *pgxpool.Conn
	once sync.Once
}

func (r *schemaRows) release() { r.once.Do(r.c.Release) }

func (r *schemaRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.Rows.Close()
	r.release()
	return false
}

func (r *schemaRows) Close() {
	r.Rows.Close()
	r.release()
}

// schemaRow releases its connection on Scan, like the pool's own row.
type schemaRow struct {
	row pgx.Row
	c   *pgxpool.Conn
}

func (r *schemaRow) Scan(dest ...any) error {
	defer r.c.Release()
	return r.row.Scan(dest...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// schemaTx releases its connection when it commits or rolls back.
type schemaTx struct {
	pgx.Tx
	c    *pgxpool.Conn
	once sync.Once
}

func (t *schemaTx) Commit(ctx context.Context) error {
	err := t.Tx.Commit(ctx)
	t.once.Do(t.c.Release)
	return err
}

func (t *schemaTx) Rollback(ctx context.Context) error {
	err := t.Tx.Rollback(ctx)
	t.once.Do(t.c.Release)
	return err
}
