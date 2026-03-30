package main

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MongoStore is a MongoDB-backed implementation of the Store interface.
// It is safe for concurrent use by multiple goroutines.
type MongoStore struct {
	client     *mongo.Client
	collection *mongo.Collection
}

// NewMongoStore connects to MongoDB at uri, ensures a unique index exists on
// the short_code field, and returns a ready-to-use MongoStore. Returns an
// error if the connection or ping fails within 10 seconds.
func NewMongoStore(uri, dbName, collectionName string) (*MongoStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}

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
func (s *MongoStore) Save(shortCode, longURL string) error {
	doc := URLRecord{
		ShortCode: shortCode,
		LongURL:   longURL,
		CreatedAt: time.Now().UTC(),
		Clicks:    0,
	}
	_, err := s.collection.InsertOne(context.Background(), doc)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return errDuplicate
		}
		return err
	}
	return nil
}

// GetByCode retrieves a URLRecord by its short code.
// Returns (nil, nil) when the code does not exist.
func (s *MongoStore) GetByCode(shortCode string) (*URLRecord, error) {
	filter := bson.D{{Key: "short_code", Value: shortCode}}
	var record URLRecord
	err := s.collection.FindOne(context.Background(), filter).Decode(&record)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// IncrementClicks atomically increments the click counter for a short code.
// It is a no-op if the code does not exist.
func (s *MongoStore) IncrementClicks(shortCode string) error {
	filter := bson.D{{Key: "short_code", Value: shortCode}}
	update := bson.D{{Key: "$inc", Value: bson.D{{Key: "clicks", Value: int64(1)}}}}
	_, err := s.collection.UpdateOne(context.Background(), filter, update)
	return err
}

// GetAll returns a snapshot of all stored URL records.
func (s *MongoStore) GetAll() []URLRecord {
	cursor, err := s.collection.Find(context.Background(), bson.D{})
	if err != nil {
		return nil
	}
	defer cursor.Close(context.Background())
	var records []URLRecord
	if err := cursor.All(context.Background(), &records); err != nil {
		return nil
	}
	return records
}
