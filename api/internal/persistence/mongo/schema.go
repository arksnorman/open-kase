package mongo

import (
	"context"
	"fmt"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// EnsureSchema creates every collection with its JSON-schema validator and
// every index. It is idempotent and cheap when the schema is already in
// place: existing collections get their validator re-applied (collMod) and
// createIndexes is a no-op for indexes that already exist with the same
// definition. init runs it, and so do serve and worker at start-up.
//
// There are no migrations: the application has not been released, so a
// changed collection shape means wiping the development database.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	existing, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return fmt.Errorf("list collections: %w", err)
	}
	for _, spec := range schemaSpecs() {
		if err := ensureCollection(ctx, db, existing, spec.name, spec.validator); err != nil {
			return err
		}
		if len(spec.indexes) > 0 {
			if _, err := db.Collection(spec.name).Indexes().CreateMany(ctx, spec.indexes); err != nil {
				return fmt.Errorf("indexes for %s: %w", spec.name, err)
			}
		}
	}
	return nil
}

// EnsureSchema applies the schema to the store's database.
func (s *Store) EnsureSchema(ctx context.Context) error { return EnsureSchema(ctx, s.db) }

// ensureCollection creates name with validator, or re-applies the validator
// when the collection already exists.
func ensureCollection(ctx context.Context, db *mongo.Database, existing []string, name string, validator bson.M) error {
	if !slices.Contains(existing, name) {
		opts := options.CreateCollection()
		if validator != nil {
			opts.SetValidator(validator).SetValidationLevel("strict").SetValidationAction("error")
		}
		err := db.CreateCollection(ctx, name, opts)
		if err == nil {
			return nil
		}
		var ce mongo.CommandError
		if !asCommandError(err, &ce) || ce.Code != 48 { // 48 NamespaceExists: another process created it first
			return fmt.Errorf("create %s: %w", name, err)
		}
	}
	if validator == nil {
		return nil
	}
	cmd := bson.D{{Key: "collMod", Value: name}, {Key: "validator", Value: validator},
		{Key: "validationLevel", Value: "strict"}, {Key: "validationAction", Value: "error"}}
	if err := db.RunCommand(ctx, cmd).Err(); err != nil {
		return fmt.Errorf("update validator for %s: %w", name, err)
	}
	return nil
}

type collectionSpec struct {
	name      string
	validator bson.M
	indexes   []mongo.IndexModel
}

// JSON-schema helpers.
func schema(required []string, props bson.M) bson.M {
	return bson.M{"$jsonSchema": object(required, props)}
}

// anyOf adds alternative constraints to a schema built by schema().
func anyOf(validator bson.M, alternatives bson.A) bson.M {
	validator["$jsonSchema"].(bson.M)["anyOf"] = alternatives
	return validator
}

func object(required []string, props bson.M) bson.M {
	return bson.M{"bsonType": "object", "required": required, "properties": props}
}

var (
	tObjectID    = bson.M{"bsonType": "objectId"}
	tOptObjectID = bson.M{"bsonType": bson.A{"objectId", "null"}}
	tDate        = bson.M{"bsonType": "date"}
	tOptDate     = bson.M{"bsonType": bson.A{"date", "null"}}
	tBool        = bson.M{"bsonType": "bool"}
	tInt         = bson.M{"bsonType": bson.A{"int", "long"}}
	tPositiveInt = bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 1}
	tString      = bson.M{"bsonType": "string"}
	tStringArray = bson.M{"bsonType": "array", "items": bson.M{"bsonType": "string"}}
	tObject      = bson.M{"bsonType": "object"}
	tActor       = object([]string{"type"}, bson.M{"type": tEnum("user", "service", "operator", "system")})
)

func tStr(minLen, maxLen int) bson.M {
	return bson.M{"bsonType": "string", "minLength": minLen, "maxLength": maxLen}
}

func tEnum(values ...string) bson.M {
	a := bson.A{}
	for _, v := range values {
		a = append(a, v)
	}
	return bson.M{"bsonType": "string", "enum": a}
}

// Index helpers.
func idx(keys bson.D, name string, unique bool) mongo.IndexModel {
	o := options.Index().SetName(name)
	if unique {
		o.SetUnique(true)
	}
	return mongo.IndexModel{Keys: keys, Options: o}
}

func ttl(field, name string, after int32) mongo.IndexModel {
	return mongo.IndexModel{Keys: bson.D{{Key: field, Value: 1}}, Options: options.Index().SetName(name).SetExpireAfterSeconds(after)}
}

// textIndex builds a compound text index. The equality prefix keys
// (project_id, and visibility for messages) must be matched exactly by every
// $text query, so a search can only ever read one project's — and for
// messages one audience's — partition of the index. Language "none" disables
// stemming and stop words so matches and highlights agree.
func textIndex(prefix bson.D, field, name string) mongo.IndexModel {
	keys := append(prefix, bson.E{Key: field, Value: "text"})
	return mongo.IndexModel{Keys: keys, Options: options.Index().SetName(name).SetDefaultLanguage("none").
		SetLanguageOverride("textLanguageUnused")}
}

// schemaSpecs lists every collection with its validator and indexes. The
// GridFS bucket collections (attachmentBytes.files / .chunks) come last;
// they carry no validator.
func schemaSpecs() []collectionSpec {
	return []collectionSpec{
		{
			name: colAccountTokens,
			validator: schema([]string{"workspaceId", "userId", "purpose", "tokenHash", "createdAt", "expiresAt", "createdBy"}, bson.M{
				"workspaceId": tObjectID,
				"userId":      tObjectID,
				"purpose":     tEnum("passwordReset", "invitation"),
				"tokenHash":   tStr(64, 64),
				"createdAt":   tDate,
				"expiresAt":   tDate,
				"createdBy":   tObject,
				"deliveryId":  tOptObjectID,
				"revokedAt":   tOptDate,
				"usedAt":      tOptDate,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "tokenHash", Value: 1}}, "tokenHashUnique", true),
				idx(bson.D{{Key: "userId", Value: 1}, {Key: "purpose", Value: 1}, {Key: "createdAt", Value: -1}}, "userPurpose", false),
				ttl("expiresAt", "expiresTtl", 2592000),
			},
		},
		{
			name: colAPICredentials,
			validator: schema([]string{"workspaceId", "projectId", "principalId", "tokenHash", "createdAt"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tObjectID,
				"principalId": tObjectID,
				"tokenHash":   tStr(64, 64),
				"createdAt":   tDate,
				"lastUsedAt":  tOptDate,
				"revokedAt":   tOptDate,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "tokenHash", Value: 1}}, "tokenHashUnique", true),
				idx(bson.D{{Key: "principalId", Value: 1}}, "principal", false),
			},
		},
		{
			name: colAttachments,
			validator: anyOf(schema([]string{"workspaceId", "projectId", "uploader", "filename", "contentType", "state", "fileId", "createdAt"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tObjectID,
				"uploader": object([]string{"kind", "id"}, bson.M{
					"kind": tEnum("user", "integration"),
					"id":   tObjectID,
				}),
				"filename":    tStr(0, 255),
				"contentType": tStr(0, 255),
				"state":       tEnum("uploading", "staged", "attached", "deleting", "removed"),
				"fileId":      tObjectID,
				"createdAt":   tDate,
				"attachedAt":  tOptDate,
				"expiresAt":   tOptDate,
				"messageId":   tOptObjectID,
				"removed": object([]string{"at", "by", "reason"}, bson.M{
					"at":             tDate,
					"by":             tActor,
					"reason":         tStr(1, 1000),
					"bytesDeletedAt": tOptDate,
				}),
				"sha256":     tStr(0, 64),
				"size":       tInt,
				"ticketId":   tOptObjectID,
				"visibility": bson.M{"bsonType": bson.A{"string", "null"}, "enum": bson.A{"public", "agent", nil}},
			}), bson.A{bson.M{"properties": bson.M{"state": tEnum("removed")}, "required": bson.A{"removed"}}, bson.M{"properties": bson.M{"state": tEnum("uploading", "staged", "attached", "deleting"), "filename": tStr(1, 255), "contentType": tStr(1, 255)}}}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "messageId", Value: 1}}, "message", false),
				idx(bson.D{{Key: "state", Value: 1}, {Key: "expiresAt", Value: 1}}, "stateExpires", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "ticketId", Value: 1}}, "projectTicket", false),
				idx(bson.D{{Key: "uploader.id", Value: 1}, {Key: "projectId", Value: 1}, {Key: "state", Value: 1}, {Key: "expiresAt", Value: 1}}, "uploaderPending", false),
				{Keys: bson.D{{Key: "scheduledMessageId", Value: 1}}, Options: options.Index().SetName("scheduled").SetPartialFilterExpression(bson.D{{Key: "scheduledMessageId", Value: bson.D{{Key: "$type", Value: "objectId"}}}})},
			},
		},
		{
			name: colAuditEvents,
			validator: schema([]string{"workspaceId", "action", "actor", "outcome", "createdAt"}, bson.M{
				"workspaceId": tObjectID,
				"action":      tStr(1, 100),
				"actor":       tActor,
				"outcome":     tEnum("success", "failure"),
				"createdAt":   tDate,
				"changes":     tStringArray,
				"projectId":   tOptObjectID,
				"target":      tString,
				"ticketId":    tOptObjectID,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "workspaceId", Value: 1}, {Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}, "workspaceCreated", false),
				{Keys: bson.D{{Key: "projectId", Value: 1}, {Key: "ticketId", Value: 1}, {Key: "createdAt", Value: 1}}, Options: options.Index().SetName("projectTicketCreated").SetPartialFilterExpression(bson.D{{Key: "projectId", Value: bson.D{{Key: "$type", Value: "objectId"}}}})},
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "action", Value: 1}, {Key: "createdAt", Value: 1}}, "projectActionCreated", false),
				idx(bson.D{{Key: "ticketId", Value: 1}, {Key: "createdAt", Value: -1}}, "ticketCreated", false),
				idx(bson.D{{Key: "workspaceId", Value: 1}, {Key: "actor.id", Value: 1}, {Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}, "workspaceActorCreated", false),
			},
		},
		{
			name: colAutomationPolicies,
			validator: schema([]string{"workspaceId", "projectId", "archiveAfterResolvedDays", "customerFollowupAfterHours", "maxReminders", "reminderCooldownHours", "autoResolveAfterHours", "enabled", "version", "updatedAt"}, bson.M{
				"workspaceId":                tObjectID,
				"projectId":                  tObjectID,
				"archiveAfterResolvedDays":   bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 0, "maximum": 3650},
				"customerFollowupAfterHours": bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 0, "maximum": 2160},
				"maxReminders":               bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 1, "maximum": 5},
				"reminderCooldownHours":      bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 1, "maximum": 2160},
				"autoResolveAfterHours":      bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 0, "maximum": 2160},
				"enabled":                    tBool,
				"version":                    tPositiveInt,
				"updatedAt":                  tDate,
				"updatedBy":                  tActor,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}}, "projectUnique", true),
				idx(bson.D{{Key: "enabled", Value: 1}}, "enabled", false),
			},
		},
		{
			name: colContacts,
			validator: schema([]string{"workspaceId", "projectId", "name", "email", "emailNormalized", "createdAt", "updatedAt", "version"}, bson.M{
				"workspaceId":     tObjectID,
				"projectId":       tObjectID,
				"name":            tStr(1, 200),
				"email":           tStr(3, 254),
				"emailNormalized": tStr(3, 254),
				"createdAt":       tDate,
				"updatedAt":       tDate,
				"version":         tPositiveInt,
				"externalId":      tStr(1, 128),
				"organisationId":  tObjectID,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "organisationId", Value: 1}}, "projectOrganisation", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "emailNormalized", Value: 1}}, "projectEmailUnique", true),
				{Keys: bson.D{{Key: "projectId", Value: 1}, {Key: "externalId", Value: 1}}, Options: options.Index().SetName("projectExternalIdUnique").SetUnique(true).SetPartialFilterExpression(bson.D{{Key: "externalId", Value: bson.D{{Key: "$type", Value: "string"}}}})},
			},
		},
		{
			name: colCounters,
			validator: schema([]string{"_id", "seq"}, bson.M{
				"_id": tStr(1, 100),
				"seq": tInt,
			}),
		},
		{
			name: colDrafts,
			validator: schema([]string{"ticketId", "authorId", "visibility", "body", "updatedAt", "version"}, bson.M{
				"ticketId":         tObjectID,
				"authorId":         tObjectID,
				"visibility":       tEnum("public", "agent"),
				"body":             tStr(0, 100000),
				"updatedAt":        tDate,
				"version":          tPositiveInt,
				"baseLastPublicId": tOptObjectID,
				"createdAt":        tDate,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "ticketId", Value: 1}, {Key: "authorId", Value: 1}, {Key: "visibility", Value: 1}}, "ticketAuthorVisibilityUnique", true),
			},
		},
		{
			name: colGroupMemberships,
			validator: schema([]string{"workspaceId", "projectId", "groupId", "userId", "createdAt"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tObjectID,
				"groupId":     tObjectID,
				"userId":      tObjectID,
				"createdAt":   tDate,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "groupId", Value: 1}, {Key: "userId", Value: 1}}, "groupUserUnique", true),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "userId", Value: 1}}, "projectUser", false),
			},
		},
		{
			name: colGroups,
			validator: schema([]string{"workspaceId", "projectId", "name", "nameNormalized", "active", "isDefault", "createdAt", "updatedAt", "version"}, bson.M{
				"workspaceId":    tObjectID,
				"projectId":      tObjectID,
				"name":           tStr(1, 80),
				"nameNormalized": tStr(1, 160),
				"active":         tBool,
				"isDefault":      tBool,
				"createdAt":      tDate,
				"updatedAt":      tDate,
				"version":        tPositiveInt,
				"description":    tStr(0, 500),
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "nameNormalized", Value: 1}}, "projectNameUnique", true),
				idx(bson.D{{Key: "workspaceId", Value: 1}, {Key: "projectId", Value: 1}, {Key: "active", Value: 1}}, "workspaceProjectActive", false),
			},
		},
		{
			name: colIdempotency,
			validator: schema([]string{"scope", "key", "requestHash", "state", "createdAt", "expiresAt"}, bson.M{
				"scope":       tStr(1, 200),
				"key":         tStr(1, 200),
				"requestHash": tStr(64, 64),
				"state":       tEnum("pending", "completed"),
				"createdAt":   tDate,
				"expiresAt":   tDate,
				"body":        bson.M{"bsonType": "binData"},
				"leaseUntil":  tDate,
				"location":    tString,
				"status":      tInt,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "scope", Value: 1}, {Key: "key", Value: 1}}, "scopeKeyUnique", true),
				ttl("expiresAt", "expiresTtl", 0),
			},
		},
		{
			name: colJobs,
			validator: schema([]string{"kind", "key", "state", "payload", "attempts", "maxAttempts", "runAt", "generation", "createdAt", "updatedAt"}, bson.M{
				"kind":           tStr(1, 100),
				"key":            tStr(1, 400),
				"state":          tEnum("queued", "running", "succeeded", "dead"),
				"payload":        tObject,
				"attempts":       tInt,
				"maxAttempts":    tPositiveInt,
				"runAt":          tDate,
				"generation":     tInt,
				"createdAt":      tDate,
				"updatedAt":      tDate,
				"completedAt":    tOptDate,
				"history":        bson.M{"bsonType": "array", "maxItems": 50},
				"lastError":      tStr(0, 1000),
				"lastErrorClass": tStr(0, 100),
				"leaseOwner":     tStr(0, 200),
				"leaseUntil":     tOptDate,
				"projectId":      tOptObjectID,
				"workspaceId":    tOptObjectID,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "key", Value: 1}}, "keyUnique", true),
				idx(bson.D{{Key: "state", Value: 1}, {Key: "runAt", Value: 1}}, "stateRunAt", false),
				idx(bson.D{{Key: "state", Value: 1}, {Key: "leaseUntil", Value: 1}}, "stateLeaseUntil", false),
				idx(bson.D{{Key: "kind", Value: 1}, {Key: "state", Value: 1}, {Key: "_id", Value: -1}}, "kindState", false),
			},
		},
		{
			name: colLoginThrottle,
			validator: schema([]string{"_id", "failures", "windowStart", "expiresAt"}, bson.M{
				"_id":         tStr(1, 200),
				"failures":    tInt,
				"windowStart": tDate,
				"expiresAt":   tDate,
			}),
			indexes: []mongo.IndexModel{
				ttl("expiresAt", "expiresTtl", 0),
			},
		},
		{
			name: colMacros,
			validator: schema([]string{"workspaceId", "projectId", "name", "body", "visibility", "active", "createdAt", "updatedAt", "version"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tObjectID,
				"name":        tStr(1, 80),
				"body":        tStr(0, 20000),
				"visibility":  tEnum("public", "agent"),
				"active":      tBool,
				"createdAt":   tDate,
				"updatedAt":   tDate,
				"version":     tPositiveInt,
				"createdBy":   tOptObjectID,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "name", Value: 1}}, "projectName", false),
			},
		},
		{
			name: colOrganisations,
			validator: schema([]string{"workspaceId", "projectId", "name", "nameNormalized", "active", "createdAt", "updatedAt", "version"}, bson.M{
				"workspaceId":    tObjectID,
				"projectId":      tObjectID,
				"name":           tStr(1, 200),
				"nameNormalized": tStr(1, 400),
				"externalRef":    tStr(1, 128),
				"active":         tBool,
				"createdAt":      tDate,
				"updatedAt":      tDate,
				"lastUsedAt":     tOptDate,
				"version":        tPositiveInt,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "nameNormalized", Value: 1}}, "projectNameUnique", true),
				{Keys: bson.D{{Key: "projectId", Value: 1}, {Key: "externalRef", Value: 1}}, Options: options.Index().SetName("projectExternalRefUnique").SetUnique(true).SetPartialFilterExpression(bson.D{{Key: "externalRef", Value: bson.D{{Key: "$type", Value: "string"}}}})},
			},
		},
		{
			name: colHoldReasons,
			validator: schema([]string{"workspaceId", "projectId", "name", "nameNormalized", "maxDays", "createdAt", "version"}, bson.M{
				"workspaceId":    tObjectID,
				"projectId":      tObjectID,
				"name":           tStr(1, 80),
				"nameNormalized": tStr(1, 160),
				"maxDays":        bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 1, "maximum": 365},
				"createdAt":      tDate,
				"createdBy":      tOptObjectID,
				"lastHoldAt":     tOptDate,
				"version":        tPositiveInt,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "nameNormalized", Value: 1}}, "projectNameUnique", true),
			},
		},
		{
			name: colMessages,
			validator: schema([]string{"workspaceId", "projectId", "ticketId", "visibility", "author", "body", "seq", "createdAt"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tObjectID,
				"ticketId":    tObjectID,
				"visibility":  tEnum("public", "agent"),
				"author": object([]string{"kind", "id"}, bson.M{
					"kind":  tEnum("user", "contact", "integration"),
					"id":    tObjectID,
					"viaId": tOptObjectID,
				}),
				"body":            tStr(1, 100000),
				"seq":             tPositiveInt,
				"createdAt":       tDate,
				"attachmentCount": tInt,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "ticketId", Value: 1}, {Key: "visibility", Value: 1}, {Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}, "ticketVisibilityCreated", false),
				idx(bson.D{{Key: "ticketId", Value: 1}, {Key: "seq", Value: 1}}, "ticketSeqUnique", true),
				textIndex(bson.D{{Key: "projectId", Value: 1}, {Key: "visibility", Value: 1}}, "body", "projectVisibilityBodyText"),
				{Keys: bson.D{{Key: "scheduledMessageId", Value: 1}}, Options: options.Index().SetName("scheduledUnique").SetUnique(true).SetPartialFilterExpression(bson.D{{Key: "scheduledMessageId", Value: bson.D{{Key: "$type", Value: "objectId"}}}})},
			},
		},
		{
			name: colMFAChallenges,
			validator: schema([]string{"workspaceId", "userId", "tokenHash", "attempts", "createdAt", "expiresAt"}, bson.M{
				"workspaceId": tObjectID,
				"userId":      tObjectID,
				"tokenHash":   tStr(64, 64),
				"attempts":    tInt,
				"createdAt":   tDate,
				"expiresAt":   tDate,
				"clientIp":    tStr(0, 64),
				"usedAt":      tOptDate,
				"userAgent":   tStr(0, 256),
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "tokenHash", Value: 1}}, "tokenHashUnique", true),
				ttl("expiresAt", "expiresTtl", 86400),
			},
		},
		{
			name: colDeliveries,
			validator: schema([]string{"workspaceId", "channel", "status", "attempts", "createdAt", "updatedAt"}, bson.M{
				"workspaceId":     tObjectID,
				"channel":         tEnum("email", "webhook"),
				"status":          tEnum("pending", "retrying", "sent", "failed", "unknownOutcome", "skipped", "skippedUnconfigured"),
				"attempts":        tInt,
				"createdAt":       tDate,
				"updatedAt":       tDate,
				"body":            bson.M{"bsonType": bson.A{"binData", "null"}},
				"completedAt":     tOptDate,
				"endpointId":      tOptObjectID,
				"error":           tStr(0, 1000),
				"errorClass":      tStr(0, 100),
				"eventId":         tOptObjectID,
				"eventType":       tStr(0, 100),
				"lastAttemptAt":   tOptDate,
				"messageId":       tStr(0, 400),
				"nextAttemptAt":   tOptDate,
				"projectId":       tOptObjectID,
				"reason":          tStr(0, 40),
				"recipient":       tObject,
				"responseMs":      tInt,
				"responseSnippet": tStr(0, 1024),
				"statusCode":      tInt,
				"template":        tStr(0, 60),
				"ticketId":        tOptObjectID,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "endpointId", Value: 1}, {Key: "_id", Value: -1}}, "projectEndpoint", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "endpointId", Value: 1}, {Key: "lastAttemptAt", Value: -1}}, "projectEndpointAttempt", false),
				{Keys: bson.D{{Key: "projectId", Value: 1}, {Key: "endpointId", Value: 1}}, Options: options.Index().SetName("projectEndpointWaiting").SetPartialFilterExpression(bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"retrying", "pending"}}}}})},
				idx(bson.D{{Key: "channel", Value: 1}, {Key: "status", Value: 1}, {Key: "_id", Value: -1}}, "channelStatus", false),
				idx(bson.D{{Key: "eventId", Value: 1}}, "event", false),
				{Keys: bson.D{{Key: "ticketId", Value: 1}}, Options: options.Index().SetName("ticketPurge").SetPartialFilterExpression(bson.D{{Key: "ticketId", Value: bson.D{{Key: "$type", Value: "objectId"}}}})},
			},
		},
		{
			name: colOutbox,
			validator: schema([]string{"workspaceId", "type", "state", "schemaVersion", "createdAt"}, bson.M{
				"workspaceId":     tObjectID,
				"type":            tStr(1, 100),
				"state":           tEnum("pending", "dispatched", "suppressed", "failed"),
				"schemaVersion":   tInt,
				"createdAt":       tDate,
				"actor":           tActor,
				"attempts":        tInt,
				"changes":         tStringArray,
				"dispatchedAt":    tOptDate,
				"fanoutCount":     tInt,
				"groupId":         tOptObjectID,
				"lastError":       tStr(0, 1000),
				"leaseGeneration": tInt,
				"leaseOwner":      tStr(0, 200),
				"leaseUntil":      tOptDate,
				"messageAuthor":   tObject,
				"messageId":       tOptObjectID,
				"occurredAt":      tDate,
				"projectId":       tOptObjectID,
				"sla":             tObject,
				"snapshot":        tObject,
				"ticketId":        tOptObjectID,
				"ticketVersion":   tInt,
				"transition":      tStr(0, 40),
				"userId":          tOptObjectID,
				"visibility":      tEnum("public", "agent"),
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "state", Value: 1}, {Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}, "stateCreated", false),
				idx(bson.D{{Key: "state", Value: 1}, {Key: "leaseUntil", Value: 1}, {Key: "createdAt", Value: 1}}, "stateLease", false),
				{Keys: bson.D{{Key: "ticketId", Value: 1}}, Options: options.Index().SetName("ticketPurge").SetPartialFilterExpression(bson.D{{Key: "ticketId", Value: bson.D{{Key: "$type", Value: "objectId"}}}})},
			},
		},
		{
			name: colProjects,
			validator: schema([]string{"workspaceId", "key", "name", "status", "createdAt", "updatedAt", "version"}, bson.M{
				"workspaceId":          tObjectID,
				"key":                  bson.M{"bsonType": "string", "pattern": "^[A-Z][A-Z0-9]{1,11}$"},
				"name":                 tStr(1, 100),
				"status":               tEnum("active", "inactive"),
				"createdAt":            tDate,
				"updatedAt":            tDate,
				"version":              tPositiveInt,
				"defaultGroupId":       tOptObjectID,
				"requesterAckDisabled": tBool,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "workspaceId", Value: 1}, {Key: "key", Value: 1}}, "workspaceKeyUnique", true),
				idx(bson.D{{Key: "workspaceId", Value: 1}, {Key: "status", Value: 1}, {Key: "name", Value: 1}}, "workspaceStatusName", false),
			},
		},
		{
			name: colRefreshTokens,
			validator: schema([]string{"workspaceId", "sessionId", "tokenHash", "createdAt", "expiresAt"}, bson.M{
				"workspaceId": tObjectID,
				"sessionId":   tObjectID,
				"tokenHash":   tStr(64, 64),
				"createdAt":   tDate,
				"expiresAt":   tDate,
				"usedAt":      tOptDate,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "tokenHash", Value: 1}}, "tokenHashUnique", true),
				idx(bson.D{{Key: "sessionId", Value: 1}}, "session", false),
				ttl("expiresAt", "expiresTtl", 0),
			},
		},
		{
			name: colRequestTypes,
			validator: schema([]string{"workspaceId", "projectId", "code", "name", "active", "order", "createdAt"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tOptObjectID,
				"code":        bson.M{"bsonType": "string", "pattern": "^[a-z][a-zA-Z0-9]{1,39}$"},
				"name":        tStr(1, 60),
				"active":      tBool,
				"order":       tInt,
				"createdAt":   tDate,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "workspaceId", Value: 1}, {Key: "projectId", Value: 1}, {Key: "code", Value: 1}}, "workspaceProjectCodeUnique", true),
			},
		},
		{
			name: colScheduledMessages,
			validator: schema([]string{"workspaceId", "projectId", "ticketId", "authorId", "visibility", "body", "attachmentIds", "sendAt", "state", "revision", "createdAt", "updatedAt", "version"}, bson.M{
				"workspaceId":      tObjectID,
				"projectId":        tObjectID,
				"ticketId":         tObjectID,
				"authorId":         tObjectID,
				"visibility":       tEnum("public", "agent"),
				"body":             tStr(1, 400000),
				"attachmentIds":    bson.M{"bsonType": "array", "items": tObjectID},
				"sendAt":           tDate,
				"state":            tEnum("scheduled", "needsReview", "sent", "cancelled"),
				"revision":         tPositiveInt,
				"createdAt":        tDate,
				"updatedAt":        tDate,
				"version":          tPositiveInt,
				"baseLastPublicId": tOptObjectID,
				"reason":           tString,
				"sentAt":           tOptDate,
				"sentMessageId":    tOptObjectID,
				"thenStatusId":     tOptObjectID,
				"noSignature":      tBool,
				"ticketVersion":    tInt,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "ticketId", Value: 1}, {Key: "authorId", Value: 1}, {Key: "state", Value: 1}, {Key: "sendAt", Value: 1}}, "ticketAuthorState", false),
				idx(bson.D{{Key: "ticketId", Value: 1}, {Key: "sendAt", Value: 1}}, "ticketSendAt", false),
			},
		},
		{
			name: colServicePrincipals,
			validator: schema([]string{"workspaceId", "projectId", "kind", "name", "scopes", "status", "tokenHint", "createdAt", "version"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tObjectID,
				"kind":        tEnum("integration", "webhook"),
				"name":        tStr(1, 80),
				"scopes":      tStringArray,
				"status":      tEnum("active", "revoked"),
				"tokenHint":   tStr(1, 32),
				"createdAt":   tDate,
				"version":     tPositiveInt,
				"createdBy":   tObjectID,
				"lastUsedAt":  tOptDate,
				"revokedAt":   tOptDate,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "_id", Value: 1}}, "projectId", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "kind", Value: 1}, {Key: "_id", Value: 1}}, "projectKind", false),
			},
		},
		{
			name: colSessions,
			validator: schema([]string{"workspaceId", "userId", "csrfHash", "createdAt", "lastUsedAt", "expiresAt"}, bson.M{
				"workspaceId":   tObjectID,
				"userId":        tObjectID,
				"csrfHash":      tStr(64, 64),
				"createdAt":     tDate,
				"lastUsedAt":    tDate,
				"expiresAt":     tDate,
				"clientIp":      tStr(0, 64),
				"revokedAt":     tOptDate,
				"revokedReason": tString,
				"userAgent":     tStr(0, 256),
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "userId", Value: 1}, {Key: "revokedAt", Value: 1}}, "userRevoked", false),
				ttl("expiresAt", "expiresTtl", 2592000),
			},
		},
		{
			name: colSettings,
			validator: schema([]string{"workspaceId", "scope", "configVersion", "data", "updatedAt"}, bson.M{
				"workspaceId":   tObjectID,
				"scope":         tEnum("installation"),
				"configVersion": tPositiveInt,
				"data":          tObject,
				"updatedAt":     tDate,
				"updatedBy":     tObject,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "workspaceId", Value: 1}, {Key: "scope", Value: 1}}, "workspaceScopeUnique", true),
			},
		},
		{
			name: colSigningKeys,
			validator: schema([]string{"_id", "workspaceId", "algorithm", "publicKey", "privateCiphertext", "active", "createdAt"}, bson.M{
				"_id":               tStr(1, 64),
				"workspaceId":       tObjectID,
				"algorithm":         tEnum("EdDSA"),
				"publicKey":         tStr(1, 128),
				"privateCiphertext": tStr(1, 1024),
				"active":            tBool,
				"createdAt":         tDate,
				"retiredAt":         tOptDate,
				"verifyUntil":       tOptDate,
			}),
			indexes: []mongo.IndexModel{
				{Keys: bson.D{{Key: "workspaceId", Value: 1}}, Options: options.Index().SetName("workspaceActiveUnique").SetUnique(true).SetPartialFilterExpression(bson.D{{Key: "active", Value: true}})},
			},
		},
		{
			name: colSLAInstances,
			validator: schema([]string{"workspaceId", "projectId", "ticketId", "cycle", "current", "snapshot", "priority", "clocks", "breaches", "generation", "startedAt", "rev", "updatedAt"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tObjectID,
				"ticketId":    tObjectID,
				"cycle":       tPositiveInt,
				"current":     tBool,
				"snapshot": object([]string{"policyVersion", "targets", "warningPercent"}, bson.M{
					"policyVersion": tInt,
					"targets": bson.M{"bsonType": "array", "items": object([]string{"firstResponseMinutes"}, bson.M{
						"firstResponseMinutes": bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 0, "maximum": 525_600},
					}), "minItems": 4, "maxItems": 4},
					"warningPercent": tInt,
					"policyId":       tOptObjectID,
				}),
				"priority": bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 1, "maximum": 4},
				"clocks": bson.M{"bsonType": "array", "maxItems": 1, "items": object([]string{"kind", "state", "budgetSeconds", "elapsedSeconds"}, bson.M{
					"kind":           tEnum("firstResponse"),
					"state":          tEnum("running", "met", "stopped"),
					"budgetSeconds":  tInt,
					"elapsedSeconds": tInt,
				})},
				"breaches":     bson.M{"bsonType": "array", "maxItems": 50},
				"generation":   tInt,
				"startedAt":    tDate,
				"rev":          tPositiveInt,
				"updatedAt":    tDate,
				"closedAt":     tOptDate,
				"closedReason": tEnum("met", "resolved", "archived", "slaOff"),
				"quiet":        tBool,
			}),
			indexes: []mongo.IndexModel{
				{Keys: bson.D{{Key: "ticketId", Value: 1}}, Options: options.Index().SetName("ticketCurrentUnique").SetUnique(true).SetPartialFilterExpression(bson.D{{Key: "current", Value: true}})},
				idx(bson.D{{Key: "ticketId", Value: 1}, {Key: "cycle", Value: 1}}, "ticketCycleUnique", true),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "breaches.at", Value: 1}}, "projectBreaches", false),
				{Keys: bson.D{{Key: "projectId", Value: 1}, {Key: "closedAt", Value: 1}, {Key: "snapshot.policyVersion", Value: 1}}, Options: options.Index().SetName("projectOpenCycles").SetPartialFilterExpression(bson.D{{Key: "current", Value: true}})},
			},
		},
		{
			name: colSLAPolicies,
			validator: schema([]string{"workspaceId", "projectId", "active", "targets", "warningPercent", "version", "createdAt", "updatedAt"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tObjectID,
				"active":      tBool,
				"targets": bson.M{"bsonType": "array", "minItems": 4, "maxItems": 4, "items": object([]string{"firstResponseMinutes"}, bson.M{
					"firstResponseMinutes": bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 0, "maximum": 525_600},
				})},
				"warningPercent":     bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 50, "maximum": 95},
				"version":            tPositiveInt,
				"createdAt":          tDate,
				"updatedAt":          tDate,
				"deactivatedAt":      tOptDate,
				"deactivatedVersion": tInt,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}}, "projectUnique", true),
			},
		},
		{
			name: colStatuses,
			validator: schema([]string{"workspaceId", "projectId", "code", "label", "category", "builtin", "active", "order", "createdAt"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tOptObjectID,
				"code":        tStr(1, 40),
				"label":       tStr(1, 60),
				"category":    tEnum("open", "waitingCustomer", "onHold", "resolved", "archived"),
				"builtin":     tBool,
				"active":      tBool,
				"order":       tInt,
				"createdAt":   tDate,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "workspaceId", Value: 1}, {Key: "projectId", Value: 1}, {Key: "code", Value: 1}}, "workspaceProjectCodeUnique", true),
			},
		},
		{
			name: colTags,
			validator: schema([]string{"workspaceId", "projectId", "name", "createdAt"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tObjectID,
				"name":        bson.M{"bsonType": "string", "pattern": "^[a-z0-9][a-z0-9_-]{0,39}$"},
				"createdAt":   tDate,
				"createdBy":   tOptObjectID,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "name", Value: 1}}, "projectName", true),
			},
		},
		{
			name: colTicketLinks,
			validator: schema([]string{"workspaceId", "projectId", "sourceTicketId", "targetTicketId", "type", "createdBy", "createdAt"}, bson.M{
				"workspaceId":    tObjectID,
				"projectId":      tObjectID,
				"sourceTicketId": tObjectID,
				"targetTicketId": tObjectID,
				"type":           tEnum("relatedTo", "duplicateOf", "followUpTo", "copiedFrom"),
				"createdBy":      tActor,
				"createdAt":      tDate,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "sourceTicketId", Value: 1}, {Key: "targetTicketId", Value: 1}, {Key: "type", Value: 1}}, "linkUnique", true),
				idx(bson.D{{Key: "targetTicketId", Value: 1}}, "target", false),
			},
		},
		{
			name: colWatchers,
			validator: schema([]string{"workspaceId", "projectId", "ticketId", "userId", "createdAt"}, bson.M{
				"workspaceId": tObjectID,
				"projectId":   tObjectID,
				"ticketId":    tObjectID,
				"userId":      tObjectID,
				"createdAt":   tDate,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "ticketId", Value: 1}, {Key: "userId", Value: 1}}, "ticketUserUnique", true),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "userId", Value: 1}, {Key: "ticketId", Value: 1}}, "projectUser", false),
			},
		},
		{
			name: colTickets,
			validator: schema([]string{"workspaceId", "projectId", "number", "subject", "requesterId", "groupId", "assigneeId", "priority", "statusId", "statusCategory", "requestTypeId", "source", "watcherCount", "createdAt", "updatedAt", "publicUpdatedAt", "publicActivityAt", "staffActivityAt", "version"}, bson.M{
				"workspaceId":         tObjectID,
				"projectId":           tObjectID,
				"number":              tPositiveInt,
				"subject":             tStr(1, 200),
				"requesterId":         tObjectID,
				"groupId":             tObjectID,
				"assigneeId":          tOptObjectID,
				"priority":            bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 1, "maximum": 4},
				"statusId":            tObjectID,
				"statusCategory":      tEnum("open", "waitingCustomer", "onHold", "resolved", "archived"),
				"requestTypeId":       tObjectID,
				"source":              tEnum("web", "api"),
				"watcherCount":        bson.M{"bsonType": bson.A{"int", "long"}, "minimum": 0},
				"createdAt":           tDate,
				"updatedAt":           tDate,
				"publicUpdatedAt":     tDate,
				"publicActivityAt":    tDate,
				"staffActivityAt":     tDate,
				"version":             tPositiveInt,
				"archivedAt":          tOptDate,
				"createdBy":           tActor,
				"firstResponseAt":     tOptDate,
				"lastPublicMessageId": tOptObjectID,
				"organisationId":      tOptObjectID,
				"requestedPriority":   bson.M{"bsonType": bson.A{"int", "long", "null"}, "minimum": 1, "maximum": 4},
				"resolvedAt":          tOptDate,
				"slaDueAt":            tOptDate,
				"slaWarnAt":           tOptDate,
				"slaOutcome":          bson.M{"bsonType": bson.A{"string", "null"}, "enum": bson.A{"met", "missed", nil}},
				"holdReasonId":        tOptObjectID,
				"holdReasonName":      bson.M{"bsonType": bson.A{"string", "null"}, "maxLength": 80},
				"holdUntil":           tOptDate,
				"ccEmails":            bson.M{"bsonType": "array", "maxItems": 10, "items": bson.M{"bsonType": "string", "minLength": 3, "maxLength": 254, "pattern": "^[^A-Z@\\s]+@[^A-Z@\\s]+$"}},
				"tags":                bson.M{"bsonType": "array", "maxItems": 20, "items": bson.M{"bsonType": "string", "pattern": "^[a-z0-9][a-z0-9_-]{0,39}$"}},
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "number", Value: 1}}, "projectNumberUnique", true),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "holdReasonId", Value: 1}}, "projectHoldReason", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "organisationId", Value: 1}}, "projectOrganisation", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "requesterId", Value: 1}, {Key: "publicUpdatedAt", Value: -1}, {Key: "_id", Value: -1}}, "projectRequester", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "createdAt", Value: 1}}, "projectCreated", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "resolvedAt", Value: 1}}, "projectResolved", false),
				textIndex(bson.D{{Key: "projectId", Value: 1}}, "subject", "projectSubjectText"),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "groupId", Value: 1}, {Key: "statusCategory", Value: 1}, {Key: "priority", Value: 1}, {Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}, "groupQueue", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "assigneeId", Value: 1}, {Key: "statusCategory", Value: 1}, {Key: "updatedAt", Value: 1}, {Key: "_id", Value: 1}}, "assigneeQueue", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "statusCategory", Value: 1}, {Key: "updatedAt", Value: -1}, {Key: "_id", Value: -1}}, "projectUpdated", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "statusCategory", Value: 1}, {Key: "publicUpdatedAt", Value: -1}, {Key: "_id", Value: -1}}, "projectPublicUpdated", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "slaWarnAt", Value: 1}}, "projectSlaWarn", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "slaDueAt", Value: 1}}, "projectSlaDue", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "slaOutcome", Value: 1}, {Key: "updatedAt", Value: -1}}, "projectSlaOutcome", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "tags", Value: 1}}, "projectTags", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "statusCategory", Value: 1}, {Key: "automation.waitingSince", Value: 1}}, "automationWaiting", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "updatedAt", Value: -1}, {Key: "_id", Value: -1}}, "projectAllUpdated", false),
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "publicUpdatedAt", Value: -1}, {Key: "_id", Value: -1}}, "projectAllPublicUpdated", false),
			},
		},
		{
			name: colUserMFA,
			validator: schema([]string{"_id", "workspaceId", "updatedAt"}, bson.M{
				"_id":               tObjectID,
				"workspaceId":       tObjectID,
				"updatedAt":         tDate,
				"enabledAt":         tOptDate,
				"lastStep":          tInt,
				"pendingCiphertext": tStr(0, 1024),
				"pendingExpiresAt":  tOptDate,
				"recoveryHashes":    bson.M{"bsonType": "array", "maxItems": 20, "items": tStr(64, 64)},
				"secretCiphertext":  tStr(0, 1024),
			}),
		},
		{
			name: colUsers,
			validator: schema([]string{"workspaceId", "email", "emailNormalized", "username", "displayName", "status", "roles", "createdAt", "updatedAt", "version", "givenName"}, bson.M{
				"workspaceId":       tObjectID,
				"email":             tStr(3, 254),
				"emailNormalized":   tStr(3, 254),
				"username":          bson.M{"bsonType": "string", "pattern": "^[a-z][a-z0-9._-]{2,31}$"},
				"displayName":       tStr(1, 152),
				"status":            tEnum("active", "disabled", "invited"),
				"roles":             tStringArray,
				"createdAt":         tDate,
				"updatedAt":         tDate,
				"version":           tPositiveInt,
				"givenName":         tStr(1, 50),
				"emailPrefs":        tObject,
				"signature":         tStr(0, 4000),
				"emailVerifiedAt":   tOptDate,
				"familyName":        tStr(0, 50),
				"mfaEnabledAt":      tOptDate,
				"middleName":        tStr(0, 50),
				"passwordChangedAt": tOptDate,
				"passwordHash":      tString,
				"theme":             tEnum("system", "light", "dark"),
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "workspaceId", Value: 1}, {Key: "emailNormalized", Value: 1}}, "workspaceEmailUnique", true),
				idx(bson.D{{Key: "workspaceId", Value: 1}, {Key: "roles", Value: 1}, {Key: "status", Value: 1}}, "workspaceRolesStatus", false),
				{Keys: bson.D{{Key: "workspaceId", Value: 1}, {Key: "username", Value: 1}}, Options: options.Index().SetName("workspaceUsernameUnique").SetUnique(true).SetPartialFilterExpression(bson.D{{Key: "username", Value: bson.D{{Key: "$type", Value: "string"}}}})},
			},
		},
		{
			name: colWebhookEndpoints,
			validator: schema([]string{"workspaceId", "projectId", "principalId", "name", "url", "eventTypes", "active", "includeAgentNotes", "headers", "auth", "createdAt", "updatedAt", "version"}, bson.M{
				"workspaceId":       tObjectID,
				"projectId":         tObjectID,
				"principalId":       tObjectID,
				"name":              tStr(1, 80),
				"url":               tStr(8, 2048),
				"eventTypes":        tStringArray,
				"active":            tBool,
				"includeAgentNotes": tBool,
				"pausedAt":          tOptDate,
				"headers": bson.M{"bsonType": "array", "items": object([]string{"name", "valueCiphertext"}, bson.M{
					"name":            tStr(1, 100),
					"valueCiphertext": tStr(1, 16384),
				}), "maxItems": 20},
				"auth": object([]string{"type"}, bson.M{
					"type":               tEnum("none", "basic", "token"),
					"passwordCiphertext": tStr(0, 4096),
					"tokenCiphertext":    tStr(0, 16384),
					"username":           tStr(0, 256),
				}),
				"createdAt": tDate,
				"updatedAt": tDate,
				"version":   tPositiveInt,
				"createdBy": tObjectID,
				"tls":       bson.M{"bsonType": "object", "properties": bson.M{"caCertificate": tStr(0, 65536), "clientCertificate": tStr(0, 32768), "clientKeyCiphertext": tStr(0, 65536)}},
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "projectId", Value: 1}, {Key: "active", Value: 1}, {Key: "_id", Value: 1}}, "projectActive", false),
			},
		},
		{
			name: colWorkspaces,
			validator: schema([]string{"_id", "installKey", "name", "keyCheck", "createdAt", "updatedAt"}, bson.M{
				"installKey":       tEnum("primary"),
				"name":             tStr(1, 100),
				"keyCheck":         tStr(1, 512),
				"createdAt":        tDate,
				"updatedAt":        tDate,
				"bootstrapAdminId": tOptObjectID,
			}),
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "installKey", Value: 1}}, "installKeyUnique", true),
			},
		},
		{
			name: attachmentBucket + ".files",
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "filename", Value: 1}, {Key: "uploadDate", Value: 1}}, "filename1UploadDate1", false),
			},
		},
		{
			name: attachmentBucket + ".chunks",
			indexes: []mongo.IndexModel{
				idx(bson.D{{Key: "files_id", Value: 1}, {Key: "n", Value: 1}}, "files_id_1_n_1", true),
			},
		},
	}
}
