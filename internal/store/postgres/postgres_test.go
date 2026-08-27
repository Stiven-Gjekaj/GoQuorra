package postgres_test

import (
	"testing"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/pgtest"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/postgres"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/storetest"
)

// The PostgreSQL store answers to the same suite as the memory store.
//
// The database, the schema, the skip rules and the lock that keeps this suite
// out of the way of worker/pgtx are all in internal/pgtest, because both
// suites need the same answer to each of them.
//
// The old suite skipped whether or not a database was named. It also called
// sql.Open first, which does not connect, so the message it skipped with
// named a fault it had not yet looked for.
func TestPostgresStore(t *testing.T) {
	pool := pgtest.Pool(t)

	storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
		// Every test starts from an empty schema.
		pgtest.Reset(t, pool)
		return postgres.NewWithPool(pool, opts)
	})
}
