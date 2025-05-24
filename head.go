package tusx

import (
	"net/http"
	"strconv"

	"github.com/xmapst/tusx/types"
)

func (tusx *STusx) HeadFile(w http.ResponseWriter, r *http.Request) {
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
	}
	info, err := upload.GetInfo(r.Context())
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set(types.HeaderUploadOffset, "0")
		w.WriteHeader(http.StatusOK)
		return
	}

	resp := types.HTTPResponse{
		Headers: map[string]string{
			"Cache-Control":          "no-store",
			types.HeaderUploadOffset: strconv.FormatInt(info.Offset, 10),
		},
	}
	if info.IsPartial {
		resp.Headers[types.HeaderUploadConcat] = "partial"
	}
	if info.IsFinal {
		v := "final;"
		for _, uploadID := range info.PartialUploads {
			v += tusx.absFileURL(r, uploadID) + " "
		}
		// Remove trailing space
		v = v[:len(v)-1]

		resp.Headers[types.HeaderUploadConcat] = v
	}

	if len(info.MetaData) != 0 {
		resp.Headers["Upload-Metadata"] = tusx.serializeMetadataHeader(info.MetaData)
	}

	resp.Headers["Upload-Length"] = strconv.FormatInt(info.Size, 10)
	// TODO: Shouldn't this rather be offset? Basically, whatever GET would return.
	// But this then also depends on the storage backend if that's even supported.
	resp.Headers["Content-Length"] = strconv.FormatInt(info.Size, 10)
	resp.StatusCode = http.StatusOK
	resp.WriteTo(w)
}
