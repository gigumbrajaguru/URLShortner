package main

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoOpTimeout bounds every MongoDB operation whose context has no earlier
// deadline, so a slow or unreachable server can never hang a request.
const mongoOpTimeout = 5 * time.Second

// MongoStore is a MongoDB-backed implementation of the Store interface.
// It is safe for concurrent use by multiple goroutines.
type MongoStore struct {
	client     *mongo.Client
	collection *mongo.Collection
}

// NewMongoStore connects to MongoDB at uri, ensures a unique index exists on
// the short_code field, and returns a ready-to-use MongoStore. ctx bounds the
// connection check and index creation; on failure the client is disconnected.
func NewMongoStore(ctx context.Context, uri, dbName, collectionName string) (*MongoStore, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetTimeout(mongoOpTimeout))
	if err != nil {
		return nil, err
	}

	s, err := initMongoStore(ctx, client, dbName, collectionName)
	if err != nil {
		// Use a fresh context: ctx may be the one that just expired.
		disconnectCtx, cancel := context.WithTimeout(context.Background(), mongoOpTimeout)
		defer cancel()
		_ = client.Disconnect(disconnectCtx)
		return nil, err
	}
	return s, nil
}

func initMongoStore(ctx context.Context, client *mongo.Client, dbName, collectionName string) (*MongoStore, error) {
	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}

	coll := client.Database(dbName).Collection(collectionName)

	// Ensure unique index on short_code. Idempotent — safe to call every startup.
	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "short_code", Value: 1}},
		Options: options.Index().SetUnique(true),
	}
	if _, err := coll.Indexes().CreateOne(ctx, indexModel); err != nil {
		return nil, err
	}

	return &MongoStore{client: client, collection: coll}, nil
}

// Save inserts a new short_code → long_url mapping.
// Returns errDuplicate if the short code already exists.
func (s *MongoStore) Save(ctx context.Context, shortCode, longURL string) error {
	doc := URLRecord{
		ShortCode: shortCode,
		LongURL:   longURL,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := s.collection.InsertOne(ctx, doc); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return errDuplicate
		}
		return err
	}
	return nil
}

// GetByCode retrieves a URLRecord by its short code.
// Returns (nil, nil) when the code does not exist.
func (s *MongoStore) GetByCode(ctx context.Context, shortCode string) (*URLRecord, error) {
	filter := bson.D{{Key: "short_code", Value: shortCode}}
	var record URLRecord
	err := s.collection.FindOne(ctx, filter).Decode(&record)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// IncrementClicks atomically increments the click counter for a short code
// and reports whether the code exists.
func (s *MongoStore) IncrementClicks(ctx context.Context, shortCode string) (bool, error) {
	filter := bson.D{{Key: "short_code", Value: shortCode}}
	update := bson.D{{Key: "$inc", Value: bson.D{{Key: "clicks", Value: int64(1)}}}}
	res, err := s.collection.UpdateOne(ctx, filter, update)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// Ping checks that the MongoDB primary is reachable.
func (s *MongoStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx, nil)
}

// Close disconnects from MongoDB.
func (s *MongoStore) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}
