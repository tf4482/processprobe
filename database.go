package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Deadline for reaching the database
const connectSeconds = 10

// Update rows matched by (name, host, os); insert the missing ones
const persistQuery = `
WITH source AS (
    SELECT *
      FROM unnest($1::boolean[], $2::text[], $3::text[], $4::text[])
           AS item(running, name, host, os)
),
updated AS (
    UPDATE processes AS target
       SET status = source.running,
           last_check = $5::timestamp
      FROM source
     WHERE target.name = source.name
       AND target.host = source.host
       AND target.os = source.os
    RETURNING source.name, source.host, source.os
),
inserted AS (
    INSERT INTO processes (status, last_check, name, host, os)
    SELECT source.running, $5::timestamp, source.name, source.host, source.os
      FROM source
     WHERE NOT EXISTS (
         SELECT 1
           FROM updated
          WHERE updated.name = source.name
            AND updated.host = source.host
            AND updated.os = source.os
     )
    RETURNING 1
)
SELECT (SELECT count(*) FROM updated), (SELECT count(*) FROM inserted)`

// Persist every result in one transaction; anything but running is stored as false
func saveStatuses(ctx context.Context, database DatabaseConfig, results []ProcessStatus) (updated, inserted int64, err error) {
	running := make([]bool, len(results))
	names := make([]string, len(results))
	hosts := make([]string, len(results))
	systems := make([]string, len(results))
	for index, result := range results {
		running[index] = result.State == stateRunning
		names[index], hosts[index], systems[index] = result.Process, result.Host.Name, result.Host.OS
	}

	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s connect_timeout=%d",
		dsnValue(database.Host), database.Port, dsnValue(database.Name), dsnValue(database.User),
		dsnValue(database.Password), connectSeconds))
	if err != nil {
		return 0, 0, fmt.Errorf("PostgreSQL connection failed: %s", reason(err))
	}
	// Closing discards an uncommitted transaction
	defer disconnect(conn)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("PostgreSQL update failed: %s", reason(err))
	}
	// Naive UTC to match the TIMESTAMP column
	err = tx.QueryRow(ctx, persistQuery, running, names, hosts, systems, time.Now().UTC()).Scan(&updated, &inserted)
	if err != nil {
		return 0, 0, fmt.Errorf("PostgreSQL update failed: %s", reason(err))
	}
	if updated+inserted != int64(len(results)) {
		return 0, 0, fmt.Errorf("matched %d rows for %d results; existing (name, host, os) rows must be unique",
			updated+inserted, len(results))
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("PostgreSQL update failed: %s", reason(err))
	}
	return updated, inserted, nil
}

// Quote a keyword/value connection string value
func dsnValue(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

// Bounded close, even after cancellation
func disconnect(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn.Close(ctx)
}

// Short single-line cause of a PostgreSQL failure
func reason(err error) string {
	var server *pgconn.PgError
	if errors.As(err, &server) {
		return server.Message
	}
	var network *net.OpError
	if errors.As(err, &network) {
		return network.Error()
	}
	return strings.Join(strings.Fields(err.Error()), " ")
}
