package state

import (
	"context"
	"database/sql/driver"
	"reflect"
)

// The PostgreSQL backend reuses every existing repo query verbatim. Those queries
// are written with '?' placeholders, so the driver stack gets a thin adapter that
// rebinds placeholders on the way to pgx. Doing it here (instead of editing ~30
// query strings and their tests) keeps the SQLite path byte-identical and makes
// the whole existing test suite runnable against PostgreSQL unchanged.
//
// The adapter forwards every optional database/sql driver interface it wraps so
// that connection pooling, pinging, session resets and multi-result sets keep
// working exactly as they do without the adapter.

type rebindConnector struct {
	inner driver.Connector
}

func newRebindConnector(inner driver.Connector) driver.Connector {
	return &rebindConnector{inner: inner}
}

func (c *rebindConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &rebindConn{inner: conn}, nil
}

func (c *rebindConnector) Driver() driver.Driver {
	return c.inner.Driver()
}

type rebindConn struct {
	inner driver.Conn
}

var (
	_ driver.Conn               = (*rebindConn)(nil)
	_ driver.ConnPrepareContext = (*rebindConn)(nil)
	_ driver.ConnBeginTx        = (*rebindConn)(nil)
	_ driver.ExecerContext      = (*rebindConn)(nil)
	_ driver.QueryerContext     = (*rebindConn)(nil)
	_ driver.Pinger             = (*rebindConn)(nil)
	_ driver.SessionResetter    = (*rebindConn)(nil)
	_ driver.Validator          = (*rebindConn)(nil)
)

func (c *rebindConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *rebindConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	rebound := SQLRebind(query)
	if pc, ok := c.inner.(driver.ConnPrepareContext); ok {
		stmt, err := pc.PrepareContext(ctx, rebound)
		if err != nil {
			return nil, err
		}
		return &rebindStmt{inner: stmt}, nil
	}
	stmt, err := c.inner.Prepare(rebound)
	if err != nil {
		return nil, err
	}
	return &rebindStmt{inner: stmt}, nil
}

func (c *rebindConn) Close() error { return c.inner.Close() }

func (c *rebindConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *rebindConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bt, ok := c.inner.(driver.ConnBeginTx); ok {
		tx, err := bt.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &rebindTx{inner: tx}, nil
	}
	tx, err := c.inner.Begin()
	if err != nil {
		return nil, err
	}
	return &rebindTx{inner: tx}, nil
}

func (c *rebindConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, SQLRebind(query), args)
}

func (c *rebindConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, SQLRebind(query), args)
	if err != nil {
		return nil, err
	}
	return &rebindRows{inner: rows}, nil
}

func (c *rebindConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *rebindConn) ResetSession(ctx context.Context) error {
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *rebindConn) IsValid() bool {
	if v, ok := c.inner.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

type rebindStmt struct {
	inner driver.Stmt
}

var (
	_ driver.Stmt             = (*rebindStmt)(nil)
	_ driver.StmtExecContext  = (*rebindStmt)(nil)
	_ driver.StmtQueryContext = (*rebindStmt)(nil)
)

func (s *rebindStmt) Close() error  { return s.inner.Close() }
func (s *rebindStmt) NumInput() int { return s.inner.NumInput() }

func (s *rebindStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.inner.Exec(args)
}

func (s *rebindStmt) Query(args []driver.Value) (driver.Rows, error) {
	rows, err := s.inner.Query(args)
	if err != nil {
		return nil, err
	}
	return &rebindRows{inner: rows}, nil
}

func (s *rebindStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if ec, ok := s.inner.(driver.StmtExecContext); ok {
		return ec.ExecContext(ctx, args)
	}
	values, err := namedValues(args)
	if err != nil {
		return nil, err
	}
	return s.inner.Exec(values)
}

func (s *rebindStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if qc, ok := s.inner.(driver.StmtQueryContext); ok {
		rows, err := qc.QueryContext(ctx, args)
		if err != nil {
			return nil, err
		}
		return &rebindRows{inner: rows}, nil
	}
	values, err := namedValues(args)
	if err != nil {
		return nil, err
	}
	rows, err := s.inner.Query(values)
	if err != nil {
		return nil, err
	}
	return &rebindRows{inner: rows}, nil
}

type rebindTx struct {
	inner driver.Tx
}

var (
	_ driver.Tx             = (*rebindTx)(nil)
	_ driver.ExecerContext  = (*rebindTx)(nil)
	_ driver.QueryerContext = (*rebindTx)(nil)
)

func (t *rebindTx) Commit() error   { return t.inner.Commit() }
func (t *rebindTx) Rollback() error { return t.inner.Rollback() }

func (t *rebindTx) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := t.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, SQLRebind(query), args)
}

func (t *rebindTx) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := t.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, SQLRebind(query), args)
	if err != nil {
		return nil, err
	}
	return &rebindRows{inner: rows}, nil
}

type rebindRows struct {
	inner driver.Rows
}

var (
	_ driver.Rows                           = (*rebindRows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*rebindRows)(nil)
	_ driver.RowsColumnTypeScanType         = (*rebindRows)(nil)
	_ driver.RowsColumnTypeNullable         = (*rebindRows)(nil)
	_ driver.RowsColumnTypeLength           = (*rebindRows)(nil)
	_ driver.RowsNextResultSet              = (*rebindRows)(nil)
)

func (r *rebindRows) Columns() []string { return r.inner.Columns() }
func (r *rebindRows) Close() error      { return r.inner.Close() }
func (r *rebindRows) Next(dest []driver.Value) error {
	return r.inner.Next(dest)
}

func (r *rebindRows) ColumnTypeDatabaseTypeName(index int) string {
	if t, ok := r.inner.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return t.ColumnTypeDatabaseTypeName(index)
	}
	return ""
}

func (r *rebindRows) ColumnTypeScanType(index int) reflect.Type {
	if t, ok := r.inner.(driver.RowsColumnTypeScanType); ok {
		return t.ColumnTypeScanType(index)
	}
	return reflect.TypeOf(new(any)).Elem()
}

func (r *rebindRows) ColumnTypeNullable(index int) (nullable, ok bool) {
	if t, ok := r.inner.(driver.RowsColumnTypeNullable); ok {
		return t.ColumnTypeNullable(index)
	}
	return false, false
}

func (r *rebindRows) ColumnTypeLength(index int) (length int64, ok bool) {
	if t, ok := r.inner.(driver.RowsColumnTypeLength); ok {
		return t.ColumnTypeLength(index)
	}
	return 0, false
}

func (r *rebindRows) HasNextResultSet() bool {
	if n, ok := r.inner.(driver.RowsNextResultSet); ok {
		return n.HasNextResultSet()
	}
	return false
}

func (r *rebindRows) NextResultSet() error {
	if n, ok := r.inner.(driver.RowsNextResultSet); ok {
		return n.NextResultSet()
	}
	return nil
}

func namedValues(args []driver.NamedValue) ([]driver.Value, error) {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		if arg.Name != "" {
			return nil, errNamedArgsUnsupported
		}
		values[i] = arg.Value
	}
	return values, nil
}
