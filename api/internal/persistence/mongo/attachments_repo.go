package mongo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

type uploaderDoc struct {
	Kind string        `bson:"kind"`
	Id   bson.ObjectID `bson:"id"`
}

type attachmentDoc struct {
	Id          bson.ObjectID  `bson:"_id"`
	WorkspaceId bson.ObjectID  `bson:"workspaceId"`
	ProjectId   bson.ObjectID  `bson:"projectId"`
	Uploader    uploaderDoc    `bson:"uploader"`
	Filename    string         `bson:"filename"`
	ContentType string         `bson:"contentType"`
	Size        int64          `bson:"size"`
	SHA256      string         `bson:"sha256"`
	State       string         `bson:"state"`
	FileId      bson.ObjectID  `bson:"fileId"`
	TicketId    *bson.ObjectID `bson:"ticketId"`
	MessageId   *bson.ObjectID `bson:"messageId"`
	Visibility  *string        `bson:"visibility"`
	CreatedAt   time.Time      `bson:"createdAt"`
	ExpiresAt   *time.Time     `bson:"expiresAt"`
	AttachedAt  *time.Time     `bson:"attachedAt"`
	// ScheduledID reserves a staged upload for a scheduled message.
	ScheduledId *bson.ObjectID `bson:"scheduledMessageId,omitempty"`
	// Removed records the redaction that removed the file (state removed).
	Removed *attachmentRemovalDoc `bson:"removed,omitempty"`
}

// attachmentRemovalDoc records who removed an attachment, when and why
// (the redaction's reason), and when its bytes were deleted.
type attachmentRemovalDoc struct {
	At     time.Time `bson:"at"`
	By     actorDoc  `bson:"by"`
	Reason string    `bson:"reason"`
	// BytesDeletedAt is set by the job that deleted the GridFS file.
	BytesDeletedAt *time.Time `bson:"bytesDeletedAt"`
}

func (d attachmentDoc) toDomain() tickets.Attachment {
	a := tickets.Attachment{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(),
		UploaderKind: d.Uploader.Kind, UploaderId: d.Uploader.Id.Hex(), Filename: d.Filename, ContentType: d.ContentType,
		Size: d.Size, SHA256: d.SHA256, State: d.State, TicketId: hexOrEmpty(d.TicketId), MessageId: hexOrEmpty(d.MessageId),
		CreatedAt: d.CreatedAt, ScheduledId: hexOrEmpty(d.ScheduledId)}
	if d.Visibility != nil {
		a.Visibility = *d.Visibility
	}
	if d.ExpiresAt != nil {
		a.ExpiresAt = *d.ExpiresAt
	}
	if a.Removed() {
		// The record keeps none of this; never pass it on even if it did.
		a.Filename, a.ContentType, a.Size, a.SHA256 = "", "", 0, ""
	}
	return a
}

// AttachmentsRepository implements tickets.FileStore on GridFS. Bytes are
// written outside transactions; the metadata record's state machine
// (uploading → staged → attached, or → deleting) protects cleanup races.
type AttachmentsRepository struct {
	store  *Store
	bucket *mongo.GridFSBucket
}

// NewAttachmentsRepository returns the GridFS attachment store.
func NewAttachmentsRepository(s *Store) *AttachmentsRepository {
	return &AttachmentsRepository{store: s, bucket: s.db.GridFSBucket(options.GridFSBucket().SetName(attachmentBucket))}
}

func (r *AttachmentsRepository) fileID(ctx context.Context, a tickets.Attachment) (bson.ObjectID, error) {
	var d attachmentDoc
	if err := r.store.col(colAttachments).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(a.Id)}}).Decode(&d); err != nil {
		return bson.ObjectID{}, err
	}
	return d.FileId, nil
}

// Begin inserts the metadata record in state "uploading". Its expiry lets
// cleanup remove bytes left behind by a crashed upload.
func (r *AttachmentsRepository) Begin(ctx context.Context, a tickets.Attachment) (tickets.Attachment, error) {
	exp := a.ExpiresAt
	d := attachmentDoc{Id: bson.NewObjectID(), WorkspaceId: mustOID(a.WorkspaceId), ProjectId: mustOID(a.ProjectId),
		Uploader: uploaderDoc{Kind: a.UploaderKind, Id: mustOID(a.UploaderId)}, Filename: a.Filename, ContentType: a.ContentType,
		State: tickets.AttachmentUploading, FileId: bson.NewObjectID(), CreatedAt: a.CreatedAt, ExpiresAt: &exp}
	if _, err := r.store.col(colAttachments).InsertOne(ctx, d); err != nil {
		return tickets.Attachment{}, err
	}
	return d.toDomain(), nil
}

type countingWriter struct {
	n int64
	w io.Writer
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Write streams at most limit bytes into GridFS, hashing them on the way.
func (r *AttachmentsRepository) Write(ctx context.Context, a tickets.Attachment, src io.Reader, limit int64) (int64, string, error) {
	fid, err := r.fileID(ctx, a)
	if err != nil {
		return 0, "", err
	}
	up, err := r.bucket.OpenUploadStreamWithID(ctx, fid, a.Filename)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(up, h)}
	if _, err := io.Copy(cw, io.LimitReader(src, limit+1)); err != nil {
		_ = up.Abort()
		return 0, "", err
	}
	if cw.n > limit {
		_ = up.Abort()
		return 0, "", tickets.ErrAttachmentTooBig
	}
	if err := up.Close(); err != nil {
		return 0, "", err
	}
	return cw.n, hex.EncodeToString(h.Sum(nil)), nil
}

// Stage marks a completed upload as attachable until its expiry.
func (r *AttachmentsRepository) Stage(ctx context.Context, a tickets.Attachment, ev audit.Event) (tickets.Attachment, error) {
	var d attachmentDoc
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		err := r.store.col(colAttachments).FindOneAndUpdate(ctx,
			bson.D{{Key: "_id", Value: mustOID(a.Id)}, {Key: "state", Value: tickets.AttachmentUploading}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "state", Value: tickets.AttachmentStaged}, {Key: "size", Value: a.Size},
				{Key: "sha256", Value: a.SHA256}, {Key: "expiresAt", Value: a.ExpiresAt}}}},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&d)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return tickets.ErrNotFound // cleaned up while uploading
		}
		if err != nil {
			return err
		}
		ev.Target = "attachment/" + a.Id
		return r.store.insertAudit(ctx, d.WorkspaceId, ev)
	})
	if err != nil {
		return tickets.Attachment{}, err
	}
	return d.toDomain(), nil
}

// Discard deletes a failed upload's bytes and record.
func (r *AttachmentsRepository) Discard(ctx context.Context, a tickets.Attachment) error {
	fid, err := r.fileID(ctx, a)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := r.bucket.Delete(ctx, fid); err != nil && !errors.Is(err, mongo.ErrFileNotFound) {
		return err
	}
	_, err = r.store.col(colAttachments).DeleteOne(ctx, bson.D{{Key: "_id", Value: mustOID(a.Id)}, {Key: "state", Value: bson.D{{Key: "$ne", Value: tickets.AttachmentAttached}}}})
	return err
}

// Get returns attachment metadata within a project.
func (r *AttachmentsRepository) Get(ctx context.Context, projectID, id string) (tickets.Attachment, error) {
	var d attachmentDoc
	err := r.store.col(colAttachments).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(id)}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return tickets.Attachment{}, tickets.ErrNotFound
	}
	if err != nil {
		return tickets.Attachment{}, err
	}
	return d.toDomain(), nil
}

// Open streams an attachment's bytes. The GridFS file is looked up from
// the metadata record, never from a caller-supplied file ID.
func (r *AttachmentsRepository) Open(ctx context.Context, a tickets.Attachment) (io.ReadCloser, error) {
	fid, err := r.fileID(ctx, a)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, tickets.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ds, err := r.bucket.OpenDownloadStream(ctx, fid)
	if errors.Is(err, mongo.ErrFileNotFound) {
		return nil, tickets.ErrNotFound
	}
	return ds, err
}

// CleanupExpired removes uploads that were never attached before expiry.
// Each record is claimed with a conditional state change (so a concurrent
// attach either wins first or fails), then its bytes and record are
// deleted. Interrupted cleanups are retried via the "deleting" state.
func (r *AttachmentsRepository) CleanupExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	col := r.store.col(colAttachments)
	docs, err := findAll[attachmentDoc](ctx, col, bson.D{
		{Key: "state", Value: bson.D{{Key: "$in", Value: bson.A{tickets.AttachmentUploading, tickets.AttachmentStaged, "deleting"}}}},
		{Key: "expiresAt", Value: bson.D{{Key: "$lt", Value: now}}},
	}, options.Find().SetLimit(int64(limit)))
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, d := range docs {
		res, err := col.UpdateOne(ctx, bson.D{{Key: "_id", Value: d.Id}, {Key: "state", Value: d.State},
			{Key: "expiresAt", Value: bson.D{{Key: "$lt", Value: now}}}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "state", Value: "deleting"}}}})
		if err != nil {
			return removed, err
		}
		if res.MatchedCount == 0 {
			continue // attached or claimed by another process meanwhile
		}
		if err := r.bucket.Delete(ctx, d.FileId); err != nil && !errors.Is(err, mongo.ErrFileNotFound) {
			return removed, err
		}
		if _, err := col.DeleteOne(ctx, bson.D{{Key: "_id", Value: d.Id}, {Key: "state", Value: "deleting"}}); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// DeleteRemovedFiles implements tickets.FileStore: the GridFS files of the
// message's removed attachments are deleted (files document, then chunks)
// and the record notes when. A retry after an interrupted delete finds the
// record unmarked again; GridFS then reports the file missing, and any
// chunks left behind are deleted explicitly.
func (r *AttachmentsRepository) DeleteRemovedFiles(ctx context.Context, projectID, messageID string, now time.Time) (int, error) {
	col := r.store.col(colAttachments)
	docs, err := findAll[attachmentDoc](ctx, col, bson.D{{Key: "messageId", Value: mustOID(messageID)},
		{Key: "projectId", Value: mustOID(projectID)}, {Key: "state", Value: tickets.AttachmentRemoved},
		{Key: "removed.bytesDeletedAt", Value: nil}}, options.Find().SetProjection(bson.D{{Key: "fileId", Value: 1}}))
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, d := range docs {
		switch err := r.bucket.Delete(ctx, d.FileId); {
		case err == nil:
			deleted++
		case errors.Is(err, mongo.ErrFileNotFound):
			if _, err := r.store.col(attachmentBucket+".chunks").DeleteMany(ctx, bson.D{{Key: "files_id", Value: d.FileId}}); err != nil {
				return deleted, err
			}
		default:
			return deleted, err
		}
		if _, err := col.UpdateOne(ctx, bson.D{{Key: "_id", Value: d.Id}, {Key: "state", Value: tickets.AttachmentRemoved}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "removed.bytesDeletedAt", Value: now}}}}); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// PendingUploads implements tickets.FileStore (index uploader_pending).
func (r *AttachmentsRepository) PendingUploads(ctx context.Context, projectID, kind, uploaderID string, now time.Time) (int64, int64, error) {
	cur, err := r.store.col(colAttachments).Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "uploader.id", Value: mustOID(uploaderID)}, {Key: "uploader.kind", Value: kind},
			{Key: "projectId", Value: mustOID(projectID)},
			{Key: "state", Value: bson.D{{Key: "$in", Value: bson.A{tickets.AttachmentUploading, tickets.AttachmentStaged}}}},
			{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: now}}}}}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil}, {Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "bytes", Value: bson.D{{Key: "$sum", Value: "$size"}}}}}},
	})
	if err != nil {
		return 0, 0, err
	}
	var out []struct {
		N     int64 `bson:"n"`
		Bytes int64 `bson:"bytes"`
	}
	if err := cur.All(ctx, &out); err != nil || len(out) == 0 {
		return 0, 0, err
	}
	return out[0].N, out[0].Bytes, nil
}
