package tusx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/xmapst/tusx/types"
)

func (tusx *STusx) PatchFile(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(types.HeaderContent) != "application/offset+octet-stream" {
		tusx.logger.Error("invalid content type header")
		http.Error(w, "invalid content type header", http.StatusBadRequest)
		return
	}
	offset, err := strconv.ParseInt(r.Header.Get(types.HeaderUploadOffset), 10, 64)
	if err != nil {
		tusx.logger.Error("invalid offset header", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := tusx.extractIDFromPath(r.URL.Path)
	if err != nil {
		tusx.logger.Error("failed to extract id from path", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	upload, err := tusx.config.Store.GetUpload(r.Context(), id)
	if err != nil {
		tusx.logger.Error("failed to get upload", "error", err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	info, err := upload.GetInfo(r.Context())
	if err != nil {
		tusx.logger.Error("failed to get upload info", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if info.IsFinal {
		tusx.logger.Error("upload is final")
		http.Error(w, "modifying a final upload is not allowed", http.StatusBadRequest)
	}

	if offset != info.Offset {
		tusx.logger.Error("invalid offset", "expected", info.Offset, "got", offset)
		http.Error(w, "invalid offset", http.StatusBadRequest)
		return
	}

	resp := types.HTTPResponse{
		StatusCode: http.StatusNoContent,
		Headers: map[string]string{
			"Cache-Control": "no-store",
		},
	}

	if r.Header.Get(types.HeaderUploadLength) != "" {
		uploadLength, err := strconv.ParseInt(r.Header.Get(types.HeaderUploadLength), 10, 64)
		if err != nil || uploadLength < 0 || uploadLength < info.Offset || (tusx.config.MaxSize > 0 && uploadLength > tusx.config.MaxSize) {
			return
		}

		info.Size = uploadLength
	}
	maxSize := info.Size - offset
	body := newBodyReader(w, r, maxSize)
	body.onReadDone = func() {
		if err = body.resC.SetReadDeadline(time.Now().Add(120 * time.Second)); err != nil {
			tusx.logger.WarnContext(r.Context(), "NetworkTimeoutError", "error", err)
		}
		if err = body.resC.SetWriteDeadline(time.Now().Add(120 * time.Second)); err != nil {
			tusx.logger.WarnContext(r.Context(), "NetworkTimeoutError", "error", err)
		}
	}
	tusx.sendProgressMessages(r, upload, info, body)
	info.Offset, err = upload.WriteChunk(r.Context(), body)
	if err != nil {
		tusx.logger.Error("failed to write chunk", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp.WriteTo(w)
}
