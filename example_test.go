package postgres_test

import (
	"context"
	"log"

	"github.com/go-rio/postgres"
	"github.com/go-rio/rio"
	"github.com/jackc/pgx/v5/pgxpool"
)

type User struct {
	ID    int64
	Email string
	Age   int
}

func ExampleOpen() {
	db, err := postgres.Open("postgres://user:pass@localhost:5432/app")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	users, err := rio.From[User]().Where("age > ?", 18).All(context.Background(), db)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%d adults", len(users))
}

// OpenPool keeps the database/sql query path while pgxpool owns the
// connections; PoolOf reaches the pool for pings and statistics.
func ExampleOpenPool() {
	ctx := context.Background()
	db, err := postgres.OpenPool(ctx, "postgres://user:pass@localhost:5432/app?pool_max_conns=10")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := postgres.PoolOf(db).Ping(ctx); err != nil {
		log.Fatal(err)
	}
}

// OpenNative executes through pgx directly: preload layers batch into one
// round trip and explicit-key InsertAll streams over COPY.
func ExampleOpenNative() {
	ctx := context.Background()
	db, err := postgres.OpenNative(ctx, "postgres://user:pass@localhost:5432/app")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	rows := []User{{ID: 1, Email: "a@example.com"}, {ID: 2, Email: "b@example.com"}}
	if err := rio.InsertAll(ctx, db, rows); err != nil {
		log.Fatal(err)
	}
}

// NewNativeFromPool takes ownership of a pool the caller configured.
func ExampleNewNativeFromPool() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://user:pass@localhost:5432/app")
	if err != nil {
		log.Fatal(err)
	}

	db := postgres.NewNativeFromPool(pool)
	defer db.Close() // closes the pool
}

// TxOf exposes the pgx transaction for session settings the rio API does
// not cover.
func ExampleTxOf() {
	ctx := context.Background()
	db, err := postgres.OpenNative(ctx, "postgres://user:pass@localhost:5432/app")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	err = db.Tx(ctx, func(tx *rio.Tx) error {
		if _, err := postgres.TxOf(tx).Exec(ctx, "SET LOCAL statement_timeout = '5s'"); err != nil {
			return err
		}
		_, err := rio.From[User]().Where("id = ?", 1).UpdateAll(ctx, tx, rio.Set{"age": 31})
		return err
	})
	if err != nil {
		log.Fatal(err)
	}
}
