package tusx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/xmapst/tusx/types"
)

func (tusx *STusx) PostFile(w http.ResponseWriter, r *http.Request) {
	containsChunk := r.Header.Get(types.HeaderContent) == "application/offset+octet-stream"
	concatHeader := r.Header.Get(types.HeaderUploadConcat)
	// Parse Upload-Concat header
	isPartial, isFinal, partialUploadIDs, err := tusx.parseConcat(concatHeader)
	if err != nil {
		tusx.logger.Error("parse concat header error", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var size int64
	var partialUploads []types.IUpload
	if isFinal {
		// A final upload must not contain a chunk within the creation request
		if containsChunk {
			tusx.logger.Error("modifying a final upload is not allowed")
			http.Error(w, "modifying a final upload is not allowed", http.StatusBadRequest)
			return
		}
		partialUploads, size, err = tusx.sizeOfUploads(r.Context(), partialUploadIDs)
		if err != nil {
			tusx.logger.Error("size of uploads error", "error", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		size, err = strconv.ParseInt(r.Header.Get(types.HeaderUploadLength), 10, 64)
		if err != nil || size < 0 {
			tusx.logger.Error("invalid upload-length header", "error", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if tusx.config.MaxSize > 0 && size > tusx.config.MaxSize {
		tusx.logger.Error("upload size exceeds maximum", "size", size, "max_size", tusx.config.MaxSize)
		http.Error(w, "upload size exceeds maximum", http.StatusRequestEntityTooLarge)
		return
	}

	meta := tusx.parseMetadataHeader(r.Header.Get(types.HeaderUploadMetadata))

	info := types.FileInfo{
		Size:           size,
		MetaData:       meta,
		IsPartial:      isPartial,
		IsFinal:        isFinal,
		PartialUploads: partialUploadIDs,
	}

	resp := types.HTTPResponse{
		StatusCode: http.StatusCreated,
		Headers:    make(map[string]string),
	}

	if tusx.config.PreUploadCreateCallback != nil {
		resp2, changes, err := tusx.config.PreUploadCreateCallback(types.HookEvent{
			Context:     r.Context(),
			HTTPRequest: r,
			Upload:      info,
		})
		if err != nil {
			tusx.logger.Error("failed to run PreUploadCreateCallback", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp = resp.MergeWith(resp2)

		// Apply changes returned from the pre-create hook.
		if changes.ID != "" {
			if err = tusx.validateUploadId(changes.ID); err != nil {
				tusx.logger.Error("failed to validate upload ID", "error", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			info.ID = changes.ID
		}
	}

	upload, err := tusx.config.Store.NewUpload(r.Context(), info)
	if err != nil {
		tusx.logger.Error("failed to create upload", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	info, err = upload.GetInfo(r.Context())
	if err != nil {
		tusx.logger.Error("failed to get upload info", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id := info.ID
	url := tusx.absFileURL(r, id)
	resp.Headers[types.HeaderLocation] = url

	tusx.events.PublishEvent("upload.created", types.HookEvent{
		Upload:      info,
		Context:     r.Context(),
		HTTPRequest: r,
	})

	if isFinal {
		// 完成后合并
		if err = upload.ConcatUploads(r.Context(), partialUploads); err != nil {
			tusx.logger.Error("failed to concat uploads", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		info.Offset = size

		resp, err = tusx.emitFinishEvents(r, resp, info)
		if err != nil {
			tusx.logger.Error("failed to emit finish events", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if containsChunk && r.Body != nil {
		if r.Body == nil {
			tusx.logger.Error("request body is nil")
			http.Error(w, "request body is nil", http.StatusInternalServerError)
			return
		}

		body := newBodyReader(w, r, info.Size)
		body.onReadDone = func() {
			if err = body.resC.SetReadDeadline(time.Now().Add(120 * time.Second)); err != nil {
				tusx.logger.WarnContext(r.Context(), "NetworkTimeoutError", "error", err)
			}
			if err = body.resC.SetWriteDeadline(time.Now().Add(120 * time.Second)); err != nil {
				tusx.logger.WarnContext(r.Context(), "NetworkTimeoutError", "error", err)
			}
		}
		tusx.sendProgressMessages(r, upload, info, body)
		size, err = upload.WriteChunk(r.Context(), body)
		if err != nil {
			tusx.logger.Error("failed to write chunk", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		info.Offset = size
		resp, err = tusx.emitFinishEvents(r, resp, info)
		if err != nil {
			tusx.logger.Error("failed to emit finish events", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	resp.Headers[types.HeaderUploadOffset] = strconv.FormatInt(info.Offset, 10)
	resp.WriteTo(w)
}
