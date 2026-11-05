package mongo

import (
	"errors"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

func asCommandError(err error, target *mongo.CommandError) bool {
	return errors.As(err, target)
}
