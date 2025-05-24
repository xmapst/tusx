package tusx

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xmapst/tusx/types"
)

var (
	reForwardedHost  = regexp.MustCompile(`host="?([^;"]+)`)
	reForwardedProto = regexp.MustCompile(`proto=(https?)`)
	reValidUploadId  = regexp.MustCompile(`^[A-Za-z0-9\-._~%!$'()*+,;=/:@]*$`)
)

type STusx struct {
	config        *SConfig
	basePath      string
	isBasePathAbs bool
	logger        *slog.Logger
	events        *sMemoryBroker
	extensions    string
}

func New(config *SConfig) (*STusx, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &STusx{
		config:        config,
		basePath:      config.BasePath,
		isBasePathAbs: config.isAbs,
		logger:        config.Logger,
		events:        newMemoryBroker(config.Logger),
		extensions:    "creation,creation-with-upload,concatenation,termination",
	}, nil
}

func (tusx *STusx) Close(ctx context.Context) error {
	tusx.events.Shutdown(ctx)
	return nil
}

func (tusx *STusx) SubscribeCompleteUploads(ctx context.Context, callback func(hook types.HookEvent) error) {
	tusx.events.SubscribeEvent(ctx, "upload.finished", callback)
}

func (tusx *STusx) SubscribeTerminatedUploads(ctx context.Context, callback func(hook types.HookEvent) error) {
	tusx.events.SubscribeEvent(ctx, "upload.terminated", callback)
}

func (tusx *STusx) SubscribeUploadProgress(ctx context.Context, callback func(hook types.HookEvent) error) {
	tusx.events.SubscribeEvent(ctx, "upload.progress", callback)
}

func (tusx *STusx) SubscribeCreatedUploads(ctx context.Context, callback func(hook types.HookEvent) error) {
	tusx.events.SubscribeEvent(ctx, "upload.created", callback)
}

func (tusx *STusx) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(types.HeaderResumable) != types.Version && r.Method != http.MethodGet {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}

	w.Header().Set(types.HeaderResumable, types.Version)
	// Allow overriding the HTTP method. The reason for this is
	// that some libraries/environments do not support PATCH and
	// DELETE requests, e.g. Flash in a browser and parts of Java.
	if newMethod := r.Header.Get("X-HTTP-Method-Override"); r.Method == http.MethodPost && newMethod != "" {
		r.Method = newMethod
	}

	method := r.Method
	path := strings.Trim(r.URL.Path, "/")
	switch path {
	case "":
		switch method {
		case http.MethodPost:
			tusx.PostFile(w, r)
		default:
			w.Header().Add("Allow", "POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`method not allowed`))
		}
	default:
		// URL points to an upload resource
		switch {
		case method == http.MethodHead && r.URL.Path != "":
			tusx.HeadFile(w, r)
			return
		case method == http.MethodPatch && r.URL.Path != "":
			tusx.PatchFile(w, r)
			return
		case method == http.MethodGet && r.URL.Path != "":
			tusx.GetFile(w, r)
			return
		case method == http.MethodDelete && r.URL.Path != "":
			tusx.DelFile(w, r)
			return
		case method == http.MethodOptions && r.URL.Path != "":
			tusx.OptionsFile(w, r)
			return
		default:
			w.Header().Add("Allow", "HEAD, PATCH, GET, DELETE")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func (tusx *STusx) OptionsFile(w http.ResponseWriter, r *http.Request) {
	if tusx.config.MaxSize > 0 {
		maxSizeStr := strconv.FormatInt(tusx.config.MaxSize, 10)
		w.Header().Set(types.HeaderMaxSize, maxSizeStr)
	}
	w.Header().Set(types.HeaderExpiration, tusx.extensions)
	w.Header().Set(types.HeaderVersion, types.Version)
	w.WriteHeader(http.StatusNoContent)
}

func (tusx *STusx) parseConcat(header string) (isPartial bool, isFinal bool, partialUploads []string, err error) {
	if header == "" {
		return
	}
	if header == "partial" {
		isPartial = true
		return
	}
	l := len("final;")
	if strings.HasPrefix(header, "final;") && len(header) > l {
		isFinal = true

		list := strings.Split(header[l:], " ")
		for _, value := range list {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}

			id, extractErr := tusx.extractIDFromURL(value, tusx.basePath)
			if extractErr != nil {
				err = extractErr
				return
			}

			partialUploads = append(partialUploads, id)
		}
	}

	// If no valid partial upload ids are extracted this is not a final upload.
	if len(partialUploads) == 0 {
		isFinal = false
		err = errors.New("invalid upload-concat header")
	}

	return
}

func (tusx *STusx) extractIDFromURL(url string, basePath string) (string, error) {
	_, id, ok := strings.Cut(url, basePath)
	if !ok {
		return "", errors.New("invalid upload-concat header")
	}

	return tusx.extractIDFromPath(id)
}

func (tusx *STusx) extractIDFromPath(path string) (string, error) {
	return strings.Trim(path, "/"), nil
}

func (tusx *STusx) sizeOfUploads(ctx context.Context, ids []string) (partialUploads []types.IUpload, size int64, err error) {
	partialUploads = make([]types.IUpload, len(ids))

	for i, id := range ids {
		upload, err := tusx.config.Store.GetUpload(ctx, id)
		if err != nil {
			return nil, 0, err
		}

		info, err := upload.GetInfo(ctx)
		if err != nil {
			return nil, 0, err
		}

		if info.Offset != info.Size {
			return nil, 0, errors.New("one of the partial uploads is not finished")
		}

		size += info.Size
		partialUploads[i] = upload
	}

	return
}

func (tusx *STusx) parseMetadataHeader(header string) map[string]string {
	meta := make(map[string]string)

	for _, element := range strings.Split(header, ",") {
		element = strings.TrimSpace(element)
		parts := strings.Split(element, " ")
		if len(parts) > 2 {
			continue
		}

		key := parts[0]
		if key == "" {
			continue
		}

		value := ""
		if len(parts) == 2 {
			// Ignore current element if the value is no valid base64
			dec, err := base64.StdEncoding.DecodeString(parts[1])
			if err != nil {
				continue
			}

			value = string(dec)
		}

		meta[key] = value
	}

	return meta
}

func (tusx *STusx) serializeMetadataHeader(meta map[string]string) string {
	header := ""
	for key, value := range meta {
		valueBase64 := base64.StdEncoding.EncodeToString([]byte(value))
		header += key + " " + valueBase64 + ","
	}

	// Remove trailing comma
	if len(header) > 0 {
		header = header[:len(header)-1]
	}

	return header
}

func (tusx *STusx) validateUploadId(newId string) error {
	if newId == "" {
		// An empty ID from FileInfoChanges is allowed. The store will then
		// just pick an ID.
		return nil
	}

	if strings.HasPrefix(newId, "/") || strings.HasSuffix(newId, "/") {
		// Disallow leading and trailing slashes, as these would be
		// stripped away by extractIDFromPath, which can cause problems and confusion.
		return fmt.Errorf("validation error in FileInfoChanges: ID must not begin or end with a forward slash (got: %s)", newId)
	}

	if !reValidUploadId.MatchString(newId) {
		// Disallow some non-URL-safe characters in the upload ID to
		// prevent issues with URL parsing, which are though to debug for users.
		return fmt.Errorf("validation error in FileInfoChanges: ID must contain only URL-safe character: %s (got: %s)", reValidUploadId.String(), newId)
	}

	return nil
}

func (tusx *STusx) mergeMetadata(dst map[string]string, src ...map[string]string) {
	if dst == nil {
		panic("dst is nil")
	}
	for _, s := range src {
		for k, v := range s {
			dst[k] = v
		}
	}
}

func (tusx *STusx) absFileURL(r *http.Request, id string) string {
	if tusx.isBasePathAbs {
		return tusx.basePath + id
	}

	// Read origin and protocol from request
	host, proto := tusx.getHostAndProtocol(r)

	url := proto + "://" + host + tusx.basePath + id

	return url
}

func (tusx *STusx) getHostAndProtocol(r *http.Request) (host, proto string) {
	if r.TLS != nil {
		proto = "https"
	} else {
		proto = "http"
	}

	host = r.Host

	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}

	if h := r.Header.Get("X-Forwarded-Proto"); h == "http" || h == "https" {
		proto = h
	}

	if h := r.Header.Get("Forwarded"); h != "" {
		if _r := reForwardedHost.FindStringSubmatch(h); len(_r) == 2 {
			host = _r[1]
		}

		if _r := reForwardedProto.FindStringSubmatch(h); len(_r) == 2 {
			proto = _r[1]
		}
	}

	// Remove default ports
	if proto == "http" {
		host = strings.TrimSuffix(host, ":80")
	}
	if proto == "https" {
		host = strings.TrimSuffix(host, ":443")
	}

	return
}

func (tusx *STusx) emitFinishEvents(r *http.Request, resp types.HTTPResponse, info types.FileInfo) (types.HTTPResponse, error) {
	if tusx.config.PreFinishResponseCallback != nil {
		resp2, err := tusx.config.PreFinishResponseCallback(types.HookEvent{
			Context:     r.Context(),
			HTTPRequest: r,
			Upload:      info,
		})
		if err != nil {
			tusx.logger.Error("failed to run PreFinishResponseCallback", "error", err)
			return resp, err
		}
		resp = resp.MergeWith(resp2)
	}

	tusx.events.PublishEvent("upload.finished", types.HookEvent{
		Context:     r.Context(),
		HTTPRequest: r,
		Upload:      info,
	})

	return resp, nil
}

var mimeInlineBrowserWhitelist = map[string]struct{}{
	"text/plain": {},

	"image/png":  {},
	"image/jpeg": {},
	"image/gif":  {},
	"image/bmp":  {},
	"image/webp": {},

	"audio/wave":     {},
	"audio/wav":      {},
	"audio/x-wav":    {},
	"audio/x-pn-wav": {},
	"audio/webm":     {},
	"audio/ogg":      {},

	"video/mp4":  {},
	"video/webm": {},
	"video/ogg":  {},

	"application/ogg": {},
}

func (tusx *STusx) filterContentType(info types.FileInfo) (contentType string, contentDisposition string) {
	filetype := info.MetaData["filetype"]

	if ft, _, err := mime.ParseMediaType(filetype); err == nil {
		// If the filetype from metadata is well-formed, we forward use this for the Content-Type header.
		// However, only allowlisted mime types	will be allowed to be shown inline in the browser
		contentType = filetype
		if _, isWhitelisted := mimeInlineBrowserWhitelist[ft]; isWhitelisted {
			contentDisposition = "inline"
		} else {
			contentDisposition = "attachment"
		}
	} else {
		// If the filetype from the metadata is not well-formed, we use a
		// default type and force the browser to download the content.
		contentType = "application/octet-stream"
		contentDisposition = "attachment"
	}

	// Add a filename to Content-Disposition if one is available in the metadata
	if filename, ok := info.MetaData["filename"]; ok {
		contentDisposition += ";filename=" + strconv.Quote(filename)
	}

	return contentType, contentDisposition
}

func (tusx *STusx) sendProgressMessages(r *http.Request, upload types.IUpload, info types.FileInfo, body *bodyReader) {
	hook := types.HookEvent{
		Context:     r.Context(),
		HTTPRequest: r,
		Upload:      info,
	}

	previousOffset := int64(0)
	originalOffset := hook.Upload.Offset

	emitProgress := func() {
		hook.Upload.Offset = originalOffset + body.bytesRead()
		if hook.Upload.Offset != previousOffset {
			_ = upload.UpdateOffset(r.Context(), hook.Upload.Offset)
			tusx.events.PublishEvent("upload.progress", hook)
			previousOffset = hook.Upload.Offset
		}
	}

	go func() {
		for {
			select {
			case <-r.Context().Done():
				emitProgress()
				return
			case <-time.After(time.Second):
				emitProgress()
			}
		}
	}()
}
