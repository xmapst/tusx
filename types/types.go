package types

import (
	"context"
	"io"
	"net/http"
	"strconv"
)

const (
	Version              = "1.0.0"
	HeaderUploadOffset   = "Upload-Offset"
	HeaderUploadLength   = "Upload-Length"
	HeaderUploadExpires  = "Upload-Expires"
	HeaderUploadMetadata = "Upload-Metadata"
	HeaderUploadConcat   = "Upload-Concat"
	HeaderContent        = "Content-Type"
	HeaderLocation       = "Location"
	HeaderVersion        = "Tus-Version"
	HeaderResumable      = "Tus-Resumable"
	HeaderMaxSize        = "Tus-Max-Size"
	HeaderExpiration     = "Tus-Expiration"
)

type IStorage interface {
	NewUpload(ctx context.Context, info FileInfo) (upload IUpload, err error)
	GetUpload(ctx context.Context, id string) (upload IUpload, err error)
}

type IUpload interface {
	UpdateOffset(ctx context.Context, offset int64) error
	GetInfo(ctx context.Context) (FileInfo, error)
	GetReader(ctx context.Context) (io.ReadCloser, error)
	WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error)
	ConcatUploads(ctx context.Context, partialUploads []IUpload) error
	ServeContent(ctx context.Context, w http.ResponseWriter, r *http.Request) error
	Terminate(ctx context.Context) error
}

type FileInfoChanges struct {
	ID       string
	MetaData map[string]string
}

type FileInfo struct {
	ID             string            `json:"id"`
	Size           int64             `json:"size"`
	Offset         int64             `json:"offset"`
	MetaData       map[string]string `json:"metaData"`
	IsPartial      bool              `json:"is_partial"`
	IsFinal        bool              `json:"is_final"`
	PartialUploads []string          `json:"partial_uploads"`
}

type HookEvent struct {
	Context     context.Context
	Upload      FileInfo
	HTTPRequest *http.Request
}

type HTTPResponse struct {
	StatusCode int
	Body       string
	Headers    map[string]string
}

func (resp HTTPResponse) WriteTo(w http.ResponseWriter) {
	headers := w.Header()
	for key, value := range resp.Headers {
		headers.Set(key, value)
	}

	if len(resp.Body) > 0 {
		headers.Set("Content-Length", strconv.Itoa(len(resp.Body)))
	}

	w.WriteHeader(resp.StatusCode)

	if len(resp.Body) > 0 {
		_, _ = w.Write([]byte(resp.Body))
	}
}

func (resp HTTPResponse) MergeWith(resp2 HTTPResponse) HTTPResponse {
	// Clone the response 1 and use it as a basis
	newResp := resp

	if resp2.StatusCode != 0 {
		newResp.StatusCode = resp2.StatusCode
	}

	if len(resp2.Body) > 0 {
		newResp.Body = resp2.Body
	}

	newResp.Headers = make(map[string]string, len(resp.Headers)+len(resp2.Headers))
	for key, value := range resp.Headers {
		newResp.Headers[key] = value
	}

	for key, value := range resp2.Headers {
		newResp.Headers[key] = value
	}
	return newResp
}
