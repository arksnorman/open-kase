package mongo

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// testStore connects to OPEN_KASE_TEST_MONGO_URI using a fresh database
// that is dropped after the test. Tests are skipped when the variable is
// unset; they require a replica set (single node is fine).
func testStore(t *testing.T) *Store {
	t.Helper()
	raw := os.Getenv("OPEN_KASE_TEST_MONGO_URI")
	if raw == "" {
		t.Skip("set OPEN_KASE_TEST_MONGO_URI to run MongoDB integration tests")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("invalid OPEN_KASE_TEST_MONGO_URI: %v", err)
	}
	name := "kase_test_" + strings.ToLower(bson.NewObjectID().Hex())
	u.Path = "/" + name
	s, err := Connect(u.String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.ReplicaSet(ctx); err != nil {
		t.Fatalf("test MongoDB must be a replica set: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.db.Drop(ctx)
		_ = s.Close(ctx)
	})
	return s
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}
