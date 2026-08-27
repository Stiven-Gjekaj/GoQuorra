package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/postgres"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/storetest"
	"github.com/Stiven-Gjekaj/GoQuorra/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The PostgreSQL store answers to the same suite as the memory store.
//
// This test skips when no database is named, and refuses to skip when
// QUORRA_TEST_REQUIRE_POSTGRES is set. CI sets it. That pairing is the point:
// a developer with nothing installed still gets a useful run, and CI cannot
// report success on a suite that quietly ran nothing.
//
// The old suite skipped in both cases. It also called sql.Open first, which
// does not connect, so the message it skipped with named a fault it had not
// yet looked for.
func TestPostgresStore(t *testing.T) {
	pool := connect(t)

	storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
		// Every test starts from an empty table. TRUNCATE rather than DELETE,
		// because it also resets the identity that seq counts from, and two
		// tests that see the same sequence numbers are easier to compare when
		// one of them fails.
		//
		// CASCADE, because a table that references jobs cannot be left
		// behind: PostgreSQL refuses to truncate a table another one points
		// at. Naming the side tables here instead would make this line
		// something to remember to change, and it would be forgotten on the
		// one after next.
		if _, err := pool.Exec(context.Background(), `TRUNCATE TABLE jobs RESTART IDENTITY CASCADE`); err != nil {
			t.Fatalf("cannot empty the jobs table: %v", err)
		}
		return postgres.NewWithPool(pool, opts)
	})
}

func connect(t *testing.T) *pgxpool.Pool {
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
	if _, err := pool.Exec(ctx, migrations.Schema()); err != nil {
		t.Fatalf("cannot apply the schema: %v", err)
	}

	return pool
}
