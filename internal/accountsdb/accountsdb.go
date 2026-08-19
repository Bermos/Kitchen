/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package accountsdb dumps and reloads the identity provider's Postgres — the
// accounts, sessions, OAuth clients and passkeys that docs/SCOPE.md item 9
// deliberately keeps out of the CRDs. It is the one part of the platform's
// state a sweep of custom resources cannot recover, and so the one part a
// backup has to reach into a database for.
//
// It is a *data* dump, not pg_dump. The schema is better-auth's and the auth
// service creates it from its own plugin set on every start (auth/src/db.ts),
// so an archive that carried DDL would be carrying a second, staler opinion
// about what the tables look like. What is restored is the rows, into the
// schema the freshly installed identity provider has just migrated into place
// — which is also why a restore is only sound between installations running
// the same release, and why the archive's manifest records which one made it.
//
// The rows travel in PostgreSQL's own COPY text format rather than as JSON.
// Every type in the database round-trips through it exactly, without this
// package having to know that a timestamp is a timestamp; a JSON encoding
// would have to guess on the way back in, and would guess wrong on `bytea`
// and on anything with a time zone.
package accountsdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Table is one table's contents, as the archive carries it.
type Table struct {
	// Name of the table in the public schema.
	Name string `json:"name"`
	// Columns dumped, in the order the COPY data has them. It is written down
	// rather than inferred on the way back in: a restore into a schema that
	// has since grown a column has to name the columns it is filling, or
	// PostgreSQL will line the data up against the wrong ones.
	Columns []string `json:"columns"`
	// Rows is how many lines Data holds, for the manifest to report.
	Rows int64 `json:"rows"`

	// Data is the table in PostgreSQL's COPY text format. It is not part of
	// the JSON manifest — the archive carries each table's data as a file of
	// its own — which is what keeps a manifest readable next to a database
	// nobody wants to read.
	Data []byte `json:"-"`
}

// Dump is the whole of the identity provider's data.
type Dump struct {
	// Database the dump was taken from, for the manifest.
	Database string `json:"database"`
	// Tables in an order that satisfies the foreign keys between them, so a
	// restore can simply replay it from the top.
	Tables []Table `json:"tables"`
}

// Rows is the total across every table, which is the one number a person
// reading a backup's manifest actually wants.
func (d Dump) Rows() int64 {
	var total int64
	for _, table := range d.Tables {
		total += table.Rows
	}
	return total
}

// Client is a connection to the accounts database.
type Client struct {
	conn *pgx.Conn
	name string
}

// Connect opens one connection to the database the DSN names.
//
// One connection, not a pool: both operations here are a single sequence of
// statements — and the restore is a single transaction — so a pool would only
// add a way for half of it to run somewhere else.
func Connect(ctx context.Context, dsn string) (*Client, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("no connection string for the accounts database")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the accounts database: %w", err)
	}
	return &Client{conn: conn, name: conn.Config().Database}, nil
}

// Close releases the connection. It is safe on a nil Client, so a caller that
// never managed to connect can defer it anyway.
func (c *Client) Close(ctx context.Context) {
	if c == nil || c.conn == nil {
		return
	}
	_ = c.conn.Close(ctx)
}

// Database is the name the connection resolved to.
func (c *Client) Database() string { return c.name }

// Dump reads every table in the public schema.
func (c *Client) Dump(ctx context.Context) (Dump, error) {
	tables, err := c.tables(ctx)
	if err != nil {
		return Dump{}, err
	}
	dump := Dump{Database: c.name, Tables: make([]Table, 0, len(tables))}
	for _, name := range tables {
		columns, err := c.columns(ctx, name)
		if err != nil {
			return Dump{}, err
		}
		if len(columns) == 0 {
			// A table with nothing but dropped or generated columns has no
			// data a COPY could carry, and `COPY t () TO STDOUT` is a syntax
			// error rather than an empty answer.
			continue
		}
		buffer := &bytes.Buffer{}
		if _, err := c.conn.PgConn().CopyTo(ctx, buffer,
			fmt.Sprintf("COPY %s (%s) TO STDOUT", qualified(name), columnList(columns))); err != nil {
			return Dump{}, fmt.Errorf("cannot read the %s table: %w", name, err)
		}
		data := buffer.Bytes()
		dump.Tables = append(dump.Tables, Table{
			Name:    name,
			Columns: columns,
			// COPY's text format is one line per row, and a value containing a
			// newline is escaped rather than written raw, so the line count is
			// the row count exactly.
			Rows: int64(bytes.Count(data, []byte("\n"))),
			Data: data,
		})
	}
	return dump, nil
}

// Restore replaces the database's contents with the dump's.
//
// It is one transaction: an archive that turns out to be unreadable half way
// through leaves the database exactly as it was, rather than leaving an
// identity provider with users and no sessions. Every table is emptied in a
// single TRUNCATE for the same reason it is one transaction — truncating them
// one at a time would have to fight the foreign keys between them, and
// disabling those needs a superuser the platform has no reason to require.
func (c *Client) Restore(ctx context.Context, dump Dump) error {
	if len(dump.Tables) == 0 {
		return errors.New("this archive carries no accounts data")
	}
	present, err := c.tables(ctx)
	if err != nil {
		return err
	}
	known := map[string]struct{}{}
	for _, name := range present {
		known[name] = struct{}{}
	}
	var missing []string
	for _, table := range dump.Tables {
		if _, ok := known[table.Name]; !ok {
			missing = append(missing, table.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the accounts database has no %s table (the identity provider migrates the schema on "+
			"start, so wait for it to be ready before restoring, and restore into the release the archive was "+
			"taken from)", strings.Join(missing, ", "))
	}

	tx, err := c.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	names := make([]string, 0, len(present))
	for _, name := range present {
		names = append(names, qualified(name))
	}
	// RESTART IDENTITY, and then setval below: the truncate takes every
	// sequence back to its start, and the restored rows carry the identifiers
	// they had, so anything the identity provider inserts next has to be told
	// where the old data got to.
	if _, err := tx.Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")+" RESTART IDENTITY"); err != nil {
		return fmt.Errorf("cannot empty the accounts database: %w", err)
	}
	for _, table := range dump.Tables {
		if len(table.Data) == 0 {
			continue
		}
		if _, err := tx.Conn().PgConn().CopyFrom(ctx, bytes.NewReader(table.Data),
			fmt.Sprintf("COPY %s (%s) FROM STDIN", qualified(table.Name), columnList(table.Columns))); err != nil {
			return fmt.Errorf("cannot restore the %s table: %w", table.Name, err)
		}
	}
	if err := resetSequences(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// tables is every ordinary table in the public schema, ordered so that a table
// comes after everything it references.
func (c *Client) tables(ctx context.Context) ([]string, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("cannot list the accounts database's tables: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	edges, err := c.references(ctx)
	if err != nil {
		return nil, err
	}
	return dependencyOrder(names, edges), nil
}

// references is which table points at which, by foreign key.
func (c *Client) references(ctx context.Context) (map[string][]string, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT child.relname, parent.relname
		FROM pg_constraint k
		JOIN pg_class child ON child.oid = k.conrelid
		JOIN pg_class parent ON parent.oid = k.confrelid
		JOIN pg_namespace n ON n.oid = child.relnamespace
		WHERE k.contype = 'f' AND n.nspname = 'public'`)
	if err != nil {
		return nil, fmt.Errorf("cannot read the accounts database's foreign keys: %w", err)
	}
	edges := map[string][]string{}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, err
		}
		edges[child] = append(edges[child], parent)
	}
	return edges, rows.Err()
}

// columns is what a COPY of the table should carry: everything that is still
// there and that PostgreSQL will accept a value for. A generated column is
// computed on insert and refuses one, so dumping it would make the archive
// unrestorable.
func (c *Client) columns(ctx context.Context, table string) ([]string, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT a.attname
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname = $1
		  AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
		ORDER BY a.attnum`, table)
	if err != nil {
		return nil, fmt.Errorf("cannot read the %s table's columns: %w", table, err)
	}
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// resetSequences moves every sequence past the identifiers the restore just
// put back. Without it the next row the identity provider writes reuses an
// identifier that is already taken, and the insert fails — some time later,
// on somebody's login, a long way from the restore that caused it.
func resetSequences(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		SELECT c.relname, a.attname, pg_get_serial_sequence(quote_ident(n.nspname) || '.' ||
			quote_ident(c.relname), a.attname)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		  AND a.attnum > 0 AND NOT a.attisdropped
		  AND pg_get_serial_sequence(quote_ident(n.nspname) || '.' || quote_ident(c.relname), a.attname) IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("cannot find the accounts database's sequences: %w", err)
	}
	type sequence struct{ table, column, name string }
	var sequences []sequence
	for rows.Next() {
		var found sequence
		if err := rows.Scan(&found.table, &found.column, &found.name); err != nil {
			rows.Close()
			return err
		}
		sequences = append(sequences, found)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, seq := range sequences {
		statement := fmt.Sprintf(
			"SELECT setval($1, COALESCE((SELECT MAX(%s) FROM %s), 1), COALESCE((SELECT MAX(%s) FROM %s), 0) > 0)",
			quoted(seq.column), qualified(seq.table), quoted(seq.column), qualified(seq.table))
		if _, err := tx.Exec(ctx, statement, seq.name); err != nil {
			return fmt.Errorf("cannot move the %s.%s sequence past the restored rows: %w",
				seq.table, seq.column, err)
		}
	}
	return nil
}

// dependencyOrder sorts tables so that every table follows the ones it
// references. A cycle — two tables referencing each other — cannot be ordered
// at all, and the tables in it come out in name order rather than not at all:
// the restore is one transaction, so an ordering that a foreign key rejects
// fails the whole restore and says which table it was, which is a better
// answer than silently leaving those tables out.
func dependencyOrder(names []string, references map[string][]string) []string {
	sort.Strings(names)
	known := map[string]struct{}{}
	for _, name := range names {
		known[name] = struct{}{}
	}

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := map[string]int{}
	ordered := make([]string, 0, len(names))

	var visit func(name string)
	visit = func(name string) {
		if state[name] != unvisited {
			return
		}
		state[name] = visiting
		parents := append([]string(nil), references[name]...)
		sort.Strings(parents)
		for _, parent := range parents {
			if _, ok := known[parent]; !ok || parent == name {
				continue
			}
			visit(parent)
		}
		state[name] = done
		ordered = append(ordered, name)
	}
	for _, name := range names {
		visit(name)
	}
	return ordered
}

// quoted is an identifier as PostgreSQL spells it. Every name here comes from
// the catalogue rather than from a request, but the tables are better-auth's
// and one of them is called "user" — a reserved word, and unquotable only
// once.
func quoted(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// qualified names a table with its schema. The connection's search_path is
// whatever the database was created with, and every name here is one of
// better-auth's own; saying "public" out loud is what keeps a restore from
// filling a table that merely shares a name.
func qualified(table string) string {
	return `"public".` + quoted(table)
}

func columnList(columns []string) string {
	quotedColumns := make([]string, 0, len(columns))
	for _, column := range columns {
		quotedColumns = append(quotedColumns, quoted(column))
	}
	return strings.Join(quotedColumns, ", ")
}
