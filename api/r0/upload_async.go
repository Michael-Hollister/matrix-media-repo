package r0

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"

	"github.com/getsentry/sentry-go"
	"github.com/sirupsen/logrus"
	"github.com/t2bot/matrix-media-repo/api/_apimeta"
	"github.com/t2bot/matrix-media-repo/api/_responses"
	"github.com/t2bot/matrix-media-repo/api/_routers"
	"github.com/t2bot/matrix-media-repo/common"
	"github.com/t2bot/matrix-media-repo/common/config"
	"github.com/t2bot/matrix-media-repo/common/rcontext"
	"github.com/t2bot/matrix-media-repo/database"
	"github.com/t2bot/matrix-media-repo/datastores"
	"github.com/t2bot/matrix-media-repo/notifier"
	"github.com/t2bot/matrix-media-repo/pipelines/_steps/meta"
	"github.com/t2bot/matrix-media-repo/pipelines/_steps/upload"
	"github.com/t2bot/matrix-media-repo/pipelines/pipeline_upload"
	"github.com/t2bot/matrix-media-repo/restrictions"
	"github.com/t2bot/matrix-media-repo/util"
	"github.com/t2bot/matrix-media-repo/util/readers"
)

func UploadMediaAsync(r *http.Request, rctx rcontext.RequestContext, user _apimeta.UserInfo) interface{} {
	server := _routers.GetParam("server", r)
	mediaId := _routers.GetParam("mediaId", r)
	filename := filepath.Base(r.URL.Query().Get("filename"))

	rctx = rctx.LogWithFields(logrus.Fields{
		"mediaId":  mediaId,
		"server":   server,
		"filename": filename,
	})

	if r.Host != server {
		return &_responses.ErrorResponse{
			Code:         common.ErrCodeNotFound,
			Message:      "Upload request is for another domain.",
			InternalCode: common.ErrCodeForbidden,
		}
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream" // binary
	}

	// Early sizing constraints (reject requests which claim to be too large/small)
	if sizeRes := uploadRequestSizeCheck(rctx, r); sizeRes != nil {
		return sizeRes
	}

	// Actually upload
	_, err := pipeline_upload.ExecutePut(rctx, server, mediaId, r.Body, contentType, filename, user.UserId)
	if err != nil {
		if errors.Is(err, common.ErrQuotaExceeded) {
			return _responses.QuotaExceeded()
		} else if errors.Is(err, common.ErrAlreadyUploaded) {
			return &_responses.ErrorResponse{
				Code:         common.ErrCodeCannotOverwrite,
				Message:      "This media has already been uploaded.",
				InternalCode: common.ErrCodeCannotOverwrite,
			}
		} else if errors.Is(err, common.ErrWrongUser) {
			return &_responses.ErrorResponse{
				Code:         common.ErrCodeForbidden,
				Message:      "You do not have permission to upload this media.",
				InternalCode: common.ErrCodeForbidden,
			}
		} else if errors.Is(err, common.ErrExpired) {
			return &_responses.ErrorResponse{
				Code:         common.ErrCodeNotFound,
				Message:      "Media expired or not found.",
				InternalCode: common.ErrCodeNotFound,
			}
		}
		rctx.Log.Error("Unexpected error uploading media: ", err)
		sentry.CaptureException(err)
		return _responses.InternalServerError("Unexpected Error")
	}

	return &MediaUploadedResponse{
		//ContentUri: util.MxcUri(media.Origin, media.MediaId), // This endpoint doesn't return a URI
	}
}

func UploadMediaAsyncComplete(r *http.Request, rctx rcontext.RequestContext, user _apimeta.UserInfo) interface{} {
	server := _routers.GetParam("server", r)
	mediaId := _routers.GetParam("mediaId", r)

	rctx = rctx.LogWithFields(logrus.Fields{
		"mediaId": mediaId,
		"server":  server,
	})

	if r.Host != server {
		return &_responses.ErrorResponse{
			Code:         common.ErrCodeNotFound,
			Message:      "Upload request is for another domain.",
			InternalCode: common.ErrCodeForbidden,
		}
	}

	// MMR TBD?
	// contentType := r.Header.Get("Content-Type")
	// if contentType == "" {
	// 	contentType = "application/octet-stream" // binary
	// }

	// // Early sizing constraints (reject requests which claim to be too large/small)
	// if sizeRes := uploadRequestSizeCheck(rctx, r); sizeRes != nil {
	// 	return sizeRes
	// }

	// // Actually upload
	// _, err := pipeline_upload.ExecutePut(rctx, server, mediaId, r.Body, contentType, filename, user.UserId)

	// MMR EXEC PUT
	// Step 1: Do we already have a media record for this?
	mediaDb := database.GetInstance().Media.Prepare(rctx)
	mediaRecord, err := mediaDb.GetById(server, mediaId)
	if err != nil {
		rctx.Log.Error("Unexpected error uploading media: ", err)
		sentry.CaptureException(err)
		return _responses.InternalServerError("Unexpected Error")
	}
	if mediaRecord != nil {
		return &_responses.ErrorResponse{
			Code:         common.ErrCodeCannotOverwrite,
			Message:      "This media has already been uploaded.",
			InternalCode: common.ErrCodeCannotOverwrite,
		}
	}

	// Step 2: Try to find the holding record
	expiringDb := database.GetInstance().ExpiringMedia.Prepare(rctx)
	record, err := expiringDb.Get(server, mediaId)
	if err != nil {
		rctx.Log.Error("Unexpected error uploading media: ", err)
		sentry.CaptureException(err)
		return _responses.InternalServerError("Unexpected Error")
	}

	// Step 3: Is the record expired?
	if record == nil || record.IsExpired() {
		return &_responses.ErrorResponse{
			Code:         common.ErrCodeNotFound,
			Message:      "Media expired or not found.",
			InternalCode: common.ErrCodeNotFound,
		}
	}

	// Step 4: Is the correct user uploading this media?
	if record.UserId != user.UserId {
		return &_responses.ErrorResponse{
			Code:         common.ErrCodeForbidden,
			Message:      "You do not have permission to upload this media.",
			InternalCode: common.ErrCodeForbidden,
		}
	}

	// Step 5: Do the upload
	// newRecord, err := Execute(ctx, origin, mediaId, r, contentType, fileName, userId, datastores.LocalMediaKind)
	// if err != nil {
	// 	return nil, err
	// }

	// MMR EXEC

	uploadDone := func(record *database.DbMedia) {
		meta.FlagAccess(rctx, record.Sha256Hash, 0) // upload time is zero here to skip metrics gathering
		if err := notifier.UploadDone(rctx, record); err != nil {
			rctx.Log.Warn("Non-fatal error notifying about completed upload: ", err)
			sentry.CaptureException(err)
		}
	}

	// Step 1: Limit the stream's length
	r = upload.LimitStream(rctx, r)

	// Step 2: Create a media ID (if needed), skipped for /complete

	// Step 3: Pick a datastore
	dsConf, err := datastores.Pick(rctx, datastores.LocalMediaKind)
	if err != nil {
		return nil, err
	}

	// Step 4: Buffer to the datastore's temporary path, and check for spam
	spamR, spamW := io.Pipe()
	spamTee := io.TeeReader(r, spamW)
	spamChan := upload.CheckSpamAsync(ctx, spamR, upload.FileMetadata{
		Name:        fileName,
		ContentType: contentType,
		UserId:      userId,
		Origin:      origin,
		MediaId:     mediaId,
	})
	sha256hash, sizeBytes, reader, err := datastores.BufferTemp(dsConf, readers.NewCancelCloser(io.NopCloser(spamTee), func() {
		r.Close()
	}))
	if err != nil {
		return nil, err
	}
	if err = spamW.Close(); err != nil {
		ctx.Log.Warn("Failed to close writer for spam checker: ", err)
		spamChan <- upload.SpamResponse{Err: errors.New("failed to close")}
	}
	defer reader.Close()
	spam := <-spamChan
	if spam.Err != nil {
		return nil, err
	}
	if spam.IsSpam {
		return nil, common.ErrMediaQuarantined
	}

	// Step 5: Split the buffer to populate cache later
	cacheR, cacheW := io.Pipe()
	allWriters := io.MultiWriter(cacheW)
	tee := io.TeeReader(reader, allWriters)

	defer func(cacheW *io.PipeWriter, err error) {
		_ = cacheW.CloseWithError(err)
	}(cacheW, errors.New("failed to finish write"))

	// Step 6: Check quarantine
	if err = upload.CheckQuarantineStatus(ctx, sha256hash); err != nil {
		return nil, err
	}

	// Step 7: Ensure user can upload within quota
	if userId != "" && !config.Runtime.IsImportProcess {
		err = quota.CanUpload(ctx, userId, sizeBytes)
		if err != nil {
			return nil, err
		}
	}

	// Step 8: Acquire a lock on the media hash for uploading
	unlockFn, err := upload.LockForUpload(ctx, sha256hash)
	if err != nil {
		return nil, err
	}
	//goland:noinspection GoUnhandledErrorResult
	defer unlockFn()

	// Step 9: Pull all upload records (to check if an upload has already happened)
	newRecord := &database.DbMedia{
		Origin:      origin,
		MediaId:     mediaId,
		UploadName:  fileName,
		ContentType: contentType,
		UserId:      userId,
		SizeBytes:   sizeBytes,
		CreationTs:  util.NowMillis(),
		Quarantined: false,
		Locatable: &database.Locatable{
			Sha256Hash:  sha256hash,
			DatastoreId: "", // Populated later
			Location:    "", // Populated later
		},
	}
	record, perfect, err := upload.FindRecord(ctx, sha256hash, userId, contentType, fileName)
	if err != nil {
		return nil, err
	}
	if record != nil {
		// We already had this record in some capacity
		if perfect && !mustUseMediaId {
			// Exact match - deduplicate, skip upload to datastore
			return record, nil
		} else {
			// We already uploaded it somewhere else - use the datastore ID and location
			newRecord.Quarantined = record.Quarantined // just in case (shouldn't be a different value by here)
			newRecord.DatastoreId = record.DatastoreId
			newRecord.Location = record.Location
			if err = database.GetInstance().Media.Prepare(ctx).Insert(newRecord); err != nil {
				return nil, err
			}
			if config.Get().General.FreezeUnauthenticatedMedia {
				if err = restrictions.SetMediaRequiresAuth(ctx, newRecord.Origin, newRecord.MediaId); err != nil {
					return nil, err
				}
			}
			uploadDone(newRecord)
			return newRecord, nil
		}
	}

	// Step 11: Asynchronously upload to cache
	cacheChan := upload.PopulateCacheAsync(ctx, cacheR, sizeBytes, sha256hash)

	// Step 12: Since we didn't find a duplicate, upload it to the datastore
	dsLocation, err := datastores.Upload(ctx, dsConf, io.NopCloser(tee), sizeBytes, contentType, sha256hash)
	if err != nil {
		return nil, err
	}
	if err = cacheW.Close(); err != nil {
		ctx.Log.Warn("Failed to close writer for cache layer: ", err)
		close(cacheChan)
	}

	// Step 13: Wait for channels
	<-cacheChan

	// Step 14: Everything finally looks good - return some stuff
	newRecord.DatastoreId = dsConf.Id
	newRecord.Location = dsLocation
	if err = database.GetInstance().Media.Prepare(ctx).Insert(newRecord); err != nil {
		if err2 := datastores.Remove(ctx, dsConf, dsLocation); err2 != nil {
			sentry.CaptureException(err2)
			ctx.Log.Warn("Error deleting upload (delete attempted due to persistence error): ", err2)
		}
		return nil, err
	}
	if config.Get().General.FreezeUnauthenticatedMedia {
		if err = restrictions.SetMediaRequiresAuth(ctx, newRecord.Origin, newRecord.MediaId); err != nil {
			return nil, err
		}
	}
	uploadDone(newRecord)
	return newRecord, nil

	// MMR EXEC

	// Step 6: Delete the holding record
	if err2 := expiringDb.Delete(origin, mediaId); err2 != nil {
		ctx.Log.Warn("Non-fatal error while deleting expiring media record: " + err2.Error())
		sentry.CaptureException(err2)
	}

	return newRecord, err
	// MMR EXEC PUT

	if err != nil {
		if errors.Is(err, common.ErrQuotaExceeded) {
			return _responses.QuotaExceeded()
		}
		rctx.Log.Error("Unexpected error uploading media: ", err)
		sentry.CaptureException(err)
		return _responses.InternalServerError("Unexpected Error")
	}

	return &MediaUploadedResponse{
		ContentUri: util.MxcUri(media.Origin, media.MediaId),
	}
}

// db := storage.GetDatabase().GetMediaStore(rctx)

// media, err := db.Get(server, mediaId)
// if err != nil {
// 	rctx.Log.Debug("got upload_complete for media that isn't in db, 404ing")
// 	return api.NotFoundError()
// }

// if media.Location == "" {
// 	rctx.Log.Warn("received upload_complete for a media with no location")
// 	return api.NotFoundError()
// }

// if media.SizeBytes > 0 {
// 	rctx.Log.Info("received upload_complete for a media which already has a size set, not re-scanning")
// 	return struct{}{}
// }

// ds, err := datastore.LocateDatastore(rctx, media.DatastoreId)
// if err != nil {
// 	rctx.Log.Warn("error getting datasource for upload_complete: ", err)
// 	return api.InternalServerError("unexpected error processing upload")
// }

// info, err := ds.ObjectInfo(rctx, media.Location)
// if err != nil {
// 	rctx.Log.Error("error getting info about object for upload_complete: ", err)
// 	return api.InternalServerError("unexpected error processing upload")
// }

// media.ContentType = info.ContentType
// media.SizeBytes = info.Size

// go func() {
// 	// Download the file to get the hash
// 	f, err := ds.DownloadFile(media.Location)
// 	if err != nil {
// 		rctx.Log.Error("error getting uploaded file for upload_complete: ", err)
// 		return
// 	}
// 	defer f.Close()

// 	hash, err := util.GetSha256HashOfStream(f)
// 	if err != nil {
// 		rctx.Log.Error("error hashing uploaded file: ", err)
// 		return
// 	}

// 	media.Sha256Hash = hash

// 	// db variable used in parent function will have a cancelled context by the time we get here
// 	outOfContextDB := storage.GetDatabase().GetMediaStore(rcontext.Initial())
// 	if err := outOfContextDB.Update(media); err != nil {
// 		rctx.Log.Error("error updating media entry in db: ", err)
// 		return
// 	}
// }()

// util.NotifyUpload(server, mediaId)

// if err := internal_cache.Get().NotifyUpload(server, mediaId, rctx); err != nil {
// 	rctx.Log.Warn("Unexpected error trying to notify cache about media: " + err.Error())
// }
