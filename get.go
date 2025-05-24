package tusx

import "net/http"

func (tusx *STusx) GetFile(w http.ResponseWriter, r *http.Request) {
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
	contentType, contentDisposition := tusx.filterContentType(info)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", contentDisposition)
	err = upload.ServeContent(r.Context(), w, r)
	if err != nil {
		tusx.logger.Error("failed to serve content", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}
