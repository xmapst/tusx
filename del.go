package tusx

import (
	"net/http"

	"github.com/xmapst/tusx/types"
)

func (tusx *STusx) DelFile(w http.ResponseWriter, r *http.Request) {
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

	resp := types.HTTPResponse{
		StatusCode: http.StatusNoContent,
	}

	if tusx.config.PreUploadTerminateCallback != nil {
		resp2, err := tusx.config.PreUploadTerminateCallback(types.HookEvent{
			Context:     r.Context(),
			HTTPRequest: r,
			Upload:      info,
		})
		if err != nil {
			tusx.logger.Error("failed to emit finish events", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp = resp.MergeWith(resp2)
	}

	err = upload.Terminate(r.Context())
	if err != nil {
		tusx.logger.Error("failed to terminate upload", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tusx.events.PublishEvent("upload.terminated", types.HookEvent{
		Context:     r.Context(),
		HTTPRequest: r,
		Upload:      info,
	})
	resp.WriteTo(w)
}
