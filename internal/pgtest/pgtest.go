// Package pgtest connects a test to the real database.
//
// Two packages test against a database rather than against a fake: the store
// contract suite, and worker/pgtx. One database serves both, and go test runs
// packages at the same time, so with nothing between them each suite empties
// tables the other is reading.
//
// That is not a worry, it is what happened. The first run of the two suites
// together reported "no such job" for a job made a moment before, and put a
// worker of the pgtx suite in the middle of a contract case that counts
// workers.
//
// So every test that wants the database takes one PostgreSQL advisory lock,
// and the two suites wait for each other instead of racing. The lock and not
// a flag on the go test command: a flag helps only the command that carries
// it, and CI runs go test directly, as does anybody who takes the URL out of
// the Makefile and runs a package by hand.
package pgtest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// key is the advisory lock every suite takes.
//
// The number means nothing. It only has to be the same one in every suite,
// which is the reason it lives here and not in each of them.
const key = 8615

// waited bounds how long one suite waits for the other.
//
// Long enough for the whole of either suite, and short enough that a run
// which is never going to start says so, instead of sitting there until the
// go test timeout kills it with no reason given.
const waited = 5 * time.Minute

// Pool reaches the database, applies the schema, and holds the lock until the
// test ends.
//
// It skips when no database is named, and refuses to skip when
// QUORRA_TEST_REQUIRE_POSTGRES is set. CI sets it. That pairing is the point:
// a developer with nothing installed still gets a useful run, and CI cannot
// report success on a suite that quietly ran nothing.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("QUORRA_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("QUORRA_TEST_REQUIRE_POSTGRES") != "" {
			t.Fatal("QUORRA_TEST_REQUIRE_POSTGRES is set and QUORRA_TEST_DATABASE_URL is not, so the PostgreSQL suite would have been skipped in silence")
		}
		t.Skip("QUORRA_TEST_DATABASE_URL is not set, so the PostgreSQL suite does not run")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("cannot build a pool for %q: %v", url, err)
	}
	t.Cleanup(pool.Close)

	// Reach the database before deciding anything. A failure here is a
	// failure, and never a skip: the caller named a database, so being unable
	// to use it is the result.
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("cannot reach the database named in QUORRA_TEST_DATABASE_URL: %v", err)
	}

	take(t, pool)

	if _, err := pool.Exec(ctx, migrations.Schema()); err != nil {
		t.Fatalf("cannot apply the schema: %v", err)
	}
	return pool
}

// take holds the lock for the rest of the test.
//
// On a connection of its own, because an advisory lock taken this way belongs
// to the session that took it. A connection handed back to the pool would
// carry the lock to whoever got it next, and releasing it would then be
// somebody else's business.
func take(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	held, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("cannot take a connection to lock the database: %v", err)
	}

	ctx, stop := context.WithTimeout(context.Background(), waited)
	defer stop()
	if _, err := held.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		held.Release()
		t.Fatalf("another suite has held the database for %s: %v", waited, err)
	}

	t.Cleanup(func() {
		// Released by hand and not left to the connection closing, because
		// the connection is going back to a pool that stays open.
		if _, err := held.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key); err != nil {
			t.Errorf("cannot let go of the database: %v", err)
		}
		held.Release()
	})
}

// Reset empties every table.
//
// TRUNCATE rather than DELETE, because it also resets the identity that seq
// counts from, and two tests that see the same sequence numbers are easier to
// compare when one of them fails.
//
// Every table, found from the catalogue rather than named here. Naming them
// was tried and was wrong within one commit: the workers table does not
// reference jobs, so a CASCADE from jobs left it behind, and every test then
// saw the workers of the test before it. A list in a harness is a list that
// goes stale.
func Reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	if _, err := pool.Exec(context.Background(), `
		DO $$
		DECLARE names TEXT;
		BEGIN
			SELECT string_agg(format('%I.%I', schemaname, tablename), ', ')
				INTO names FROM pg_tables WHERE schemaname = 'public';
			IF names IS NOT NULL THEN
				EXECUTE 'TRUNCATE TABLE ' || names || ' RESTART IDENTITY CASCADE';
			END IF;
		END
		$$`); err != nil {
		t.Fatalf("cannot empty the tables: %v", err)
	}
}
