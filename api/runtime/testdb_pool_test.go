package runtime

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Harness databases get the schema once and are reused: after a passing test
// every collection is emptied (the collections, indexes and validators stay)
// and the database goes back to the pool. Creating a fresh database for each
// of the package's tests made the suite slow and inflated
// mongod's memory with thousands of created and dropped collections. A failed
// test's database is dropped instead, so a broken state is never reused.
var testDBPool struct {
	mu   sync.Mutex
	free []string // database URIs with the schema in place, empty
	all  []string // every pooled database, dropped when the package finishes
}

func TestMain(m *testing.M) {
	code := m.Run()
	testDBPool.mu.Lock()
	all := testDBPool.all
	testDBPool.mu.Unlock()
	for _, uri := range all {
		dropTestDB(uri)
	}
	os.Exit(code)
}

// pooledTestDB returns a database URI for a harness and whether it still
// needs the schema. The database is wiped and returned to the pool after the
// test (registered first, so it runs after the container has closed).
func pooledTestDB(t *testing.T) (uri string, fresh bool) {
	t.Helper()
	raw := os.Getenv("OPEN_KASE_TEST_MONGO_URI")
	if raw == "" {
		t.Skip("set OPEN_KASE_TEST_MONGO_URI to run MongoDB integration tests")
	}
	testDBPool.mu.Lock()
	if n := len(testDBPool.free); n > 0 {
		uri = testDBPool.free[n-1]
		testDBPool.free = testDBPool.free[:n-1]
	} else {
		u, err := url.Parse(raw)
		if err != nil {
			testDBPool.mu.Unlock()
			t.Fatal(err)
		}
		u.Path = "/kase_test_" + bson.NewObjectID().Hex()
		uri, fresh = u.String(), true
		testDBPool.all = append(testDBPool.all, uri)
	}
	testDBPool.mu.Unlock()
	t.Cleanup(func() {
		if t.Failed() || !wipeTestDB(uri) {
			dropTestDB(uri)
			testDBPool.mu.Lock()
			for i, u := range testDBPool.all {
				if u == uri {
					testDBPool.all = append(testDBPool.all[:i], testDBPool.all[i+1:]...)
					break
				}
			}
			testDBPool.mu.Unlock()
			return
		}
		testDBPool.mu.Lock()
		testDBPool.free = append(testDBPool.free, uri)
		testDBPool.mu.Unlock()
	})
	return uri, fresh
}

func testDBName(uri string) string {
	u, _ := url.Parse(uri)
	return strings.TrimPrefix(u.Path, "/")
}

// wipeTestDB empties every collection except system collections. It
// reports false when the database could not be cleaned.
func wipeTestDB(uri string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return false
	}
	defer client.Disconnect(ctx)
	db := client.Database(testDBName(uri))
	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "type", Value: "collection"}})
	if err != nil {
		return false
	}
	for _, n := range names {
		if strings.HasPrefix(n, "system.") {
			continue
		}
		if _, err := db.Collection(n).DeleteMany(ctx, bson.D{}); err != nil {
			return false
		}
	}
	return true
}

func dropTestDB(uri string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return
	}
	defer client.Disconnect(ctx)
	_ = client.Database(testDBName(uri)).Drop(ctx)
}
