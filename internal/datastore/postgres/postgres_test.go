//go:build ci
// +build ci

package postgres

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	sq "github.com/Masterminds/squirrel"
	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/jackc/pgx/v4"
	"github.com/stretchr/testify/require"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"

	"github.com/authzed/spicedb/internal/testfixtures"
	testdatastore "github.com/authzed/spicedb/internal/testserver/datastore"
	"github.com/authzed/spicedb/pkg/datastore"
	"github.com/authzed/spicedb/pkg/datastore/test"
	"github.com/authzed/spicedb/pkg/namespace"
	"github.com/authzed/spicedb/pkg/tuple"
)

func TestPostgresDatastore(t *testing.T) {
	b := testdatastore.RunPostgresForTesting(t, "")

	test.All(t, test.DatastoreTesterFunc(func(revisionQuantization, gcWindow time.Duration, watchBufferLength uint16) (datastore.Datastore, error) {
		ds := b.NewDatastore(t, func(engine, uri string) datastore.Datastore {
			ds, err := NewPostgresDatastore(uri,
				RevisionQuantization(revisionQuantization),
				GCWindow(gcWindow),
				WatchBufferLength(watchBufferLength),
				DebugAnalyzeBeforeStatistics(),
			)
			require.NoError(t, err)
			return ds
		})
		return ds, nil
	}))

	t.Run("WithSplit", func(t *testing.T) {
		// Set the split at a VERY small size, to ensure any WithUsersets queries are split.
		test.All(t, test.DatastoreTesterFunc(func(revisionQuantization, gcWindow time.Duration, watchBufferLength uint16) (datastore.Datastore, error) {
			ds := b.NewDatastore(t, func(engine, uri string) datastore.Datastore {
				ds, err := NewPostgresDatastore(uri,
					RevisionQuantization(revisionQuantization),
					GCWindow(gcWindow),
					WatchBufferLength(watchBufferLength),
					DebugAnalyzeBeforeStatistics(),
					SplitAtUsersetCount(1), // 1 userset
				)
				require.NoError(t, err)
				return ds
			})

			return ds, nil
		}))
	})

	t.Run("GarbageCollection", createDatastoreTest(
		b,
		GarbageCollectionTest,
		RevisionQuantization(0),
		GCWindow(1*time.Millisecond),
		WatchBufferLength(1),
	))

	t.Run("TransactionTimestamps", createDatastoreTest(
		b,
		TransactionTimestampsTest,
		RevisionQuantization(0),
		GCWindow(1*time.Millisecond),
		WatchBufferLength(1),
	))

	t.Run("GarbageCollectionByTime", createDatastoreTest(
		b,
		GarbageCollectionByTimeTest,
		RevisionQuantization(0),
		GCWindow(1*time.Millisecond),
		WatchBufferLength(1),
	))

	t.Run("ChunkedGarbageCollection", createDatastoreTest(
		b,
		ChunkedGarbageCollectionTest,
		RevisionQuantization(0),
		GCWindow(1*time.Millisecond),
		WatchBufferLength(1),
	))

	t.Run("QuantizedRevisions", func(t *testing.T) {
		QuantizedRevisionTest(t, b)
	})
}

type datastoreTestFunc func(t *testing.T, ds datastore.Datastore)

func createDatastoreTest(b testdatastore.RunningEngineForTest, tf datastoreTestFunc, options ...Option) func(*testing.T) {
	return func(t *testing.T) {
		ds := b.NewDatastore(t, func(engine, uri string) datastore.Datastore {
			ds, err := NewPostgresDatastore(uri, options...)
			require.NoError(t, err)
			return ds
		})
		defer ds.Close()

		tf(t, ds)
	}
}

func GarbageCollectionTest(t *testing.T, ds datastore.Datastore) {
	require := require.New(t)

	ctx := context.Background()
	ok, err := ds.IsReady(ctx)
	require.NoError(err)
	require.True(ok)

	writtenAt, err := ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		// Write basic namespaces.
		return rwt.WriteNamespaces(namespace.Namespace(
			"resource",
			namespace.Relation("reader", nil),
		), namespace.Namespace("user"))
	})
	require.NoError(err)

	// Run GC at the transaction and ensure no relationships are removed.
	pds := ds.(*pgDatastore)

	relsDeleted, _, err := pds.collectGarbageForTransaction(ctx, uint64(writtenAt.IntPart()))
	require.Equal(int64(0), relsDeleted)
	require.NoError(err)

	// Write a relationship.
	tpl := &core.RelationTuple{
		ObjectAndRelation: &core.ObjectAndRelation{
			Namespace: "resource",
			ObjectId:  "someresource",
			Relation:  "reader",
		},
		User: &core.User{UserOneof: &core.User_Userset{Userset: &core.ObjectAndRelation{
			Namespace: "user",
			ObjectId:  "someuser",
			Relation:  "...",
		}}},
	}
	relationship := tuple.ToRelationship(tpl)

	relWrittenAt, err := ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteRelationships([]*v1.RelationshipUpdate{{
			Operation:    v1.RelationshipUpdate_OPERATION_CREATE,
			Relationship: relationship,
		}})
	})
	require.NoError(err)

	// Run GC at the transaction and ensure no relationships are removed, but 1 transaction (the previous write namespace) is.
	relsDeleted, transactionsDeleted, err := pds.collectGarbageForTransaction(ctx, uint64(relWrittenAt.IntPart()))
	require.Equal(int64(0), relsDeleted)
	require.Equal(int64(1), transactionsDeleted)
	require.NoError(err)

	// Run GC again and ensure there are no changes.
	relsDeleted, transactionsDeleted, err = pds.collectGarbageForTransaction(ctx, uint64(relWrittenAt.IntPart()))
	require.Equal(int64(0), relsDeleted)
	require.Equal(int64(0), transactionsDeleted)
	require.NoError(err)

	// Ensure the relationship is still present.
	tRequire := testfixtures.TupleChecker{Require: require, DS: ds}
	tRequire.TupleExists(ctx, tpl, relWrittenAt)

	// Overwrite the relationship.
	relOverwrittenAt, err := ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteRelationships([]*v1.RelationshipUpdate{{
			Operation:    v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: relationship,
		}})
	})
	require.NoError(err)

	// Run GC at the transaction and ensure the (older copy of the) relationship is removed, as well as 1 transaction (the write).
	relsDeleted, transactionsDeleted, err = pds.collectGarbageForTransaction(ctx, uint64(relOverwrittenAt.IntPart()))
	require.Equal(int64(1), relsDeleted)
	require.Equal(int64(1), transactionsDeleted)
	require.NoError(err)

	// Run GC again and ensure there are no changes.
	relsDeleted, transactionsDeleted, err = pds.collectGarbageForTransaction(ctx, uint64(relOverwrittenAt.IntPart()))
	require.Equal(int64(0), relsDeleted)
	require.Equal(int64(0), transactionsDeleted)
	require.NoError(err)

	// Ensure the relationship is still present.
	tRequire.TupleExists(ctx, tpl, relOverwrittenAt)

	// Delete the relationship.
	relDeletedAt, err := ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteRelationships([]*v1.RelationshipUpdate{{
			Operation:    v1.RelationshipUpdate_OPERATION_DELETE,
			Relationship: relationship,
		}})
	})
	require.NoError(err)

	// Ensure the relationship is gone.
	tRequire.NoTupleExists(ctx, tpl, relDeletedAt)

	// Run GC at the transaction and ensure the relationship is removed, as well as 1 transaction (the overwrite).
	relsDeleted, transactionsDeleted, err = pds.collectGarbageForTransaction(ctx, uint64(relDeletedAt.IntPart()))
	require.Equal(int64(1), relsDeleted)
	require.Equal(int64(1), transactionsDeleted)
	require.NoError(err)

	// Run GC again and ensure there are no changes.
	relsDeleted, transactionsDeleted, err = pds.collectGarbageForTransaction(ctx, uint64(relDeletedAt.IntPart()))
	require.Equal(int64(0), relsDeleted)
	require.Equal(int64(0), transactionsDeleted)
	require.NoError(err)

	// Write a the relationship a few times.
	var relLastWriteAt datastore.Revision
	for i := 0; i < 3; i++ {
		var err error
		relLastWriteAt, err = ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
			return rwt.WriteRelationships([]*v1.RelationshipUpdate{{
				Operation:    v1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: relationship,
			}})
		})
		require.NoError(err)
	}

	// Run GC at the transaction and ensure the older copies of the relationships are removed,
	// as well as the 2 older write transactions and the older delete transaction.
	relsDeleted, transactionsDeleted, err = pds.collectGarbageForTransaction(ctx, uint64(relLastWriteAt.IntPart()))
	require.Equal(int64(2), relsDeleted)
	require.Equal(int64(3), transactionsDeleted)
	require.NoError(err)

	// Ensure the relationship is still present.
	tRequire.TupleExists(ctx, tpl, relLastWriteAt)
}

func TransactionTimestampsTest(t *testing.T, ds datastore.Datastore) {
	require := require.New(t)

	ctx := context.Background()
	ok, err := ds.IsReady(ctx)
	require.NoError(err)
	require.True(ok)

	// Setting db default time zone to before UTC
	pgd := ds.(*pgDatastore)
	_, err = pgd.dbpool.Exec(ctx, "SET TIME ZONE 'America/New_York';")
	require.NoError(err)

	// Get timestamp in UTC as reference
	startTimeUTC, err := pgd.getNow(ctx)
	require.NoError(err)

	// Transaction timestamp should not be stored in system time zone
	tx, err := pgd.dbpool.Begin(ctx)
	require.NoError(err)

	txID, err := createNewTransaction(ctx, tx)
	require.NoError(err)

	err = tx.Commit(ctx)
	require.NoError(err)

	var ts time.Time
	sql, args, err := psql.Select("timestamp").From(tableTransaction).Where(sq.Eq{"id": txID}).ToSql()
	require.NoError(err)
	err = pgd.dbpool.QueryRow(
		datastore.SeparateContextWithTracing(ctx), sql, args...,
	).Scan(&ts)
	require.NoError(err)

	// Transaction timestamp will be before the reference time if it was stored
	// in the default time zone and reinterpreted
	require.True(startTimeUTC.Before(ts))
}

func GarbageCollectionByTimeTest(t *testing.T, ds datastore.Datastore) {
	require := require.New(t)

	ctx := context.Background()
	ok, err := ds.IsReady(ctx)
	require.NoError(err)
	require.True(ok)

	// Write basic namespaces.
	_, err = ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteNamespaces(namespace.Namespace(
			"resource",
			namespace.Relation("reader", nil),
		), namespace.Namespace("user"))
	})
	require.NoError(err)

	pds := ds.(*pgDatastore)

	// Sleep 1ms to ensure GC will delete the previous transaction.
	time.Sleep(1 * time.Millisecond)

	// Write a relationship.
	tpl := &core.RelationTuple{
		ObjectAndRelation: &core.ObjectAndRelation{
			Namespace: "resource",
			ObjectId:  "someresource",
			Relation:  "reader",
		},
		User: &core.User{UserOneof: &core.User_Userset{Userset: &core.ObjectAndRelation{
			Namespace: "user",
			ObjectId:  "someuser",
			Relation:  "...",
		}}},
	}
	relationship := tuple.ToRelationship(tpl)

	relLastWriteAt, err := ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteRelationships([]*v1.RelationshipUpdate{{
			Operation:    v1.RelationshipUpdate_OPERATION_CREATE,
			Relationship: relationship,
		}})
	})
	require.NoError(err)

	// Run GC and ensure only transactions were removed.
	afterWrite, err := pds.getNow(ctx)
	require.NoError(err)

	relsDeleted, transactionsDeleted, err := pds.collectGarbageBefore(ctx, afterWrite)
	require.Equal(int64(0), relsDeleted)
	require.True(transactionsDeleted > 0)
	require.NoError(err)

	// Ensure the relationship is still present.
	tRequire := testfixtures.TupleChecker{Require: require, DS: ds}
	tRequire.TupleExists(ctx, tpl, relLastWriteAt)

	// Sleep 1ms to ensure GC will delete the previous write.
	time.Sleep(1 * time.Millisecond)

	// Delete the relationship.
	relDeletedAt, err := ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteRelationships([]*v1.RelationshipUpdate{{
			Operation:    v1.RelationshipUpdate_OPERATION_DELETE,
			Relationship: relationship,
		}})
	})
	require.NoError(err)

	// Run GC and ensure the relationship is removed.
	afterDelete, err := pds.getNow(ctx)
	require.NoError(err)

	relsDeleted, transactionsDeleted, err = pds.collectGarbageBefore(ctx, afterDelete)
	require.Equal(int64(1), relsDeleted)
	require.Equal(int64(1), transactionsDeleted)
	require.NoError(err)

	// Ensure the relationship is still not present.
	tRequire.NoTupleExists(ctx, tpl, relDeletedAt)
}

const chunkRelationshipCount = 2000

func ChunkedGarbageCollectionTest(t *testing.T, ds datastore.Datastore) {
	require := require.New(t)

	ctx := context.Background()
	ok, err := ds.IsReady(ctx)
	require.NoError(err)
	require.True(ok)

	// Write basic namespaces.
	_, err = ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteNamespaces(namespace.Namespace(
			"resource",
			namespace.Relation("reader", nil),
		), namespace.Namespace("user"))
	})
	require.NoError(err)

	pds := ds.(*pgDatastore)

	// Prepare relationships to write.
	var tpls []*core.RelationTuple
	for i := 0; i < chunkRelationshipCount; i++ {
		tpl := &core.RelationTuple{
			ObjectAndRelation: &core.ObjectAndRelation{
				Namespace: "resource",
				ObjectId:  fmt.Sprintf("resource-%d", i),
				Relation:  "reader",
			},
			User: &core.User{UserOneof: &core.User_Userset{Userset: &core.ObjectAndRelation{
				Namespace: "user",
				ObjectId:  "someuser",
				Relation:  "...",
			}}},
		}
		tpls = append(tpls, tpl)
	}

	// Write a large number of relationships.
	updates := make([]*v1.RelationshipUpdate, 0, len(tpls))
	for _, tpl := range tpls {
		relationship := tuple.ToRelationship(tpl)
		updates = append(updates, &v1.RelationshipUpdate{
			Operation:    v1.RelationshipUpdate_OPERATION_CREATE,
			Relationship: relationship,
		})
	}

	writtenAt, err := ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteRelationships(updates)
	})
	require.NoError(err)

	// Ensure the relationships were written.
	tRequire := testfixtures.TupleChecker{Require: require, DS: ds}
	for _, tpl := range tpls {
		tRequire.TupleExists(ctx, tpl, writtenAt)
	}

	// Run GC and ensure only transactions were removed.
	afterWrite, err := pds.getNow(ctx)
	require.NoError(err)

	relsDeleted, transactionsDeleted, err := pds.collectGarbageBefore(ctx, afterWrite)
	require.Equal(int64(0), relsDeleted)
	require.True(transactionsDeleted > 0)
	require.NoError(err)

	// Sleep to ensure the relationships will GC.
	time.Sleep(1 * time.Millisecond)

	// Delete all the relationships.
	deletes := make([]*v1.RelationshipUpdate, 0, len(tpls))
	for _, tpl := range tpls {
		relationship := tuple.ToRelationship(tpl)
		deletes = append(deletes, &v1.RelationshipUpdate{
			Operation:    v1.RelationshipUpdate_OPERATION_DELETE,
			Relationship: relationship,
		})
	}

	deletedAt, err := ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteRelationships(deletes)
	})
	require.NoError(err)

	// Ensure the relationships were deleted.
	for _, tpl := range tpls {
		tRequire.NoTupleExists(ctx, tpl, deletedAt)
	}

	// Sleep to ensure GC.
	time.Sleep(1 * time.Millisecond)

	// Run GC and ensure all the stale relationships are removed.
	afterDelete, err := pds.getNow(ctx)
	require.NoError(err)

	relsDeleted, transactionsDeleted, err = pds.collectGarbageBefore(ctx, afterDelete)
	require.Equal(int64(chunkRelationshipCount), relsDeleted)
	require.Equal(int64(1), transactionsDeleted)
	require.NoError(err)
}

func QuantizedRevisionTest(t *testing.T, b testdatastore.RunningEngineForTest) {
	testCases := []struct {
		testName         string
		quantization     time.Duration
		relativeTimes    []time.Duration
		expectedRevision uint64
	}{
		{
			"DefaultRevision",
			1 * time.Second,
			[]time.Duration{},
			1,
		},
		{
			"OnlyPastRevisions",
			1 * time.Second,
			[]time.Duration{-2 * time.Second},
			2,
		},
		{
			"OnlyFutureRevisions",
			1 * time.Second,
			[]time.Duration{2 * time.Second},
			2,
		},
		{
			"QuantizedLower",
			1 * time.Second,
			[]time.Duration{-2 * time.Second, -1 * time.Nanosecond, 0},
			3,
		},
		{
			"QuantizationDisabled",
			1 * time.Nanosecond,
			[]time.Duration{-2 * time.Second, -1 * time.Nanosecond, 0},
			4,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.testName, func(t *testing.T) {
			require := require.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			var conn *pgx.Conn
			ds := b.NewDatastore(t, func(engine, uri string) datastore.Datastore {
				var err error
				conn, err = pgx.Connect(ctx, uri)
				require.NoError(err)

				ds, err := NewPostgresDatastore(
					uri,
					RevisionQuantization(5*time.Second),
					GCWindow(24*time.Hour),
					WatchBufferLength(1),
				)
				require.NoError(err)

				return ds
			})
			defer ds.Close()

			tx, err := conn.Begin(ctx)
			require.NoError(err)

			// set a random time zone to ensure the queries are unaffect by tz
			_, err = tx.Exec(ctx, fmt.Sprintf("SET TIME ZONE -%d", rand.Intn(8)+1))
			require.NoError(err)

			var dbNow time.Time
			err = tx.QueryRow(ctx, "SELECT (NOW() AT TIME ZONE 'utc')").Scan(&dbNow)
			require.NoError(err)

			if len(tc.relativeTimes) > 0 {
				psql := sq.StatementBuilder.PlaceholderFormat(sq.Dollar)
				bulkWrite := psql.Insert(tableTransaction).Columns(colTimestamp)

				for _, offset := range tc.relativeTimes {
					bulkWrite = bulkWrite.Values(dbNow.Add(offset))
				}

				sql, args, err := bulkWrite.ToSql()
				require.NoError(err)

				_, err = tx.Exec(ctx, sql, args...)
				require.NoError(err)
			}

			queryRevision := fmt.Sprintf(
				querySelectRevision,
				colID,
				tableTransaction,
				colTimestamp,
				tc.quantization.Nanoseconds(),
			)

			var revision uint64
			var validFor time.Duration
			err = tx.QueryRow(ctx, queryRevision).Scan(&revision, &validFor)
			require.NoError(err)
			require.Greater(validFor, time.Duration(0))

			require.Equal(tc.expectedRevision, revision)
		})
	}
}

func BenchmarkPostgresQuery(b *testing.B) {
	req := require.New(b)

	ds := testdatastore.RunPostgresForTesting(b, "").NewDatastore(b, func(engine, uri string) datastore.Datastore {
		ds, err := NewPostgresDatastore(uri,
			RevisionQuantization(0),
			GCWindow(time.Millisecond*1),
			WatchBufferLength(1),
		)
		require.NoError(b, err)
		return ds
	})
	defer ds.Close()
	ds, revision := testfixtures.StandardDatastoreWithData(ds, req)

	b.Run("benchmark checks", func(b *testing.B) {
		require := require.New(b)

		for i := 0; i < b.N; i++ {
			iter, err := ds.SnapshotReader(revision).QueryRelationships(context.Background(), &v1.RelationshipFilter{
				ResourceType: testfixtures.DocumentNS.Name,
			})
			require.NoError(err)

			defer iter.Close()

			for tpl := iter.Next(); tpl != nil; tpl = iter.Next() {
				require.Equal(testfixtures.DocumentNS.Name, tpl.ObjectAndRelation.Namespace)
			}
			require.NoError(iter.Err())
		}
	})
}
