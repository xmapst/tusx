package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/xmapst/tusx/types"
)

var defaultFilePerm = os.FileMode(0664)
var defaultDirectoryPerm = os.FileMode(0754)

type SFileStore struct {
	Dir string
}

func New(dir string) (*SFileStore, error) {
	_ = os.MkdirAll(dir, defaultDirectoryPerm)
	return &SFileStore{
		Dir: dir,
	}, nil
}

func (store *SFileStore) infoPath(id string) string {
	return filepath.Join(store.Dir, id+".json")
}

func (store *SFileStore) binPath(id string) string {
	return filepath.Join(store.Dir, id)
}

func (store *SFileStore) NewUpload(ctx context.Context, info types.FileInfo) (types.IUpload, error) {
	if info.ID == "" {
		info.ID = types.Uid()
	}
	upload := &sFileUpload{
		info:     info,
		infoPath: store.infoPath(info.ID),
		binPath:  store.binPath(info.ID),
	}
	if err := upload.createFile(upload.binPath, nil); err != nil {
		return nil, err
	}
	if err := upload.writeInfo(); err != nil {
		return nil, err
	}
	return upload, nil
}

func (store *SFileStore) GetUpload(ctx context.Context, id string) (types.IUpload, error) {
	upload := &sFileUpload{
		infoPath: store.infoPath(id),
		binPath:  store.binPath(id),
	}
	data, err := os.ReadFile(upload.infoPath)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &upload.info); err != nil {
		return nil, err
	}

	stat, err := os.Stat(upload.binPath)
	if err != nil {
		return nil, err
	}
	upload.info.Offset = stat.Size()
	return upload, nil
}

type sFileUpload struct {
	info     types.FileInfo
	infoPath string
	binPath  string
}

func (upload *sFileUpload) writeInfo() error {
	data, err := json.Marshal(upload.info)
	if err != nil {
		return err
	}
	return upload.createFile(upload.infoPath, data)
}

func (upload *sFileUpload) createFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), defaultDirectoryPerm); err != nil {
		return fmt.Errorf("failed to create directory for %s: %s", path, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, defaultFilePerm)
	if err != nil {
		return err
	}
	if content != nil {
		if _, err = file.Write(content); err != nil {
			return err
		}
	}
	return file.Close()
}

func (upload *sFileUpload) UpdateOffset(ctx context.Context, offset int64) error {
	upload.info.Offset = offset
	return upload.writeInfo()
}

func (upload *sFileUpload) GetInfo(ctx context.Context) (types.FileInfo, error) {
	return upload.info, nil
}

func (upload *sFileUpload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	return os.Open(upload.binPath)
}

func (upload *sFileUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	file, err := os.OpenFile(upload.binPath, os.O_WRONLY|os.O_APPEND, defaultFilePerm)
	if err != nil {
		return 0, err
	}
	defer func() {
		cerr := file.Close()
		if err == nil {
			err = cerr
		}
	}()
	n, err := io.Copy(file, src)
	if err != nil {
		return n, err
	}
	upload.info.Offset += n
	return n, upload.writeInfo()
}

func (upload *sFileUpload) ConcatUploads(ctx context.Context, uploads []types.IUpload) (err error) {
	file, err := os.OpenFile(upload.binPath, os.O_WRONLY|os.O_APPEND, defaultFilePerm)
	if err != nil {
		return err
	}
	defer func() {
		cerr := file.Close()
		if err == nil {
			err = cerr
		}
	}()
	for _, partialUpload := range uploads {
		_partialUpload := partialUpload.(*sFileUpload)
		if err = _partialUpload.appendTo(file); err != nil {
			return err
		}
		// clear partial upload
		if err = _partialUpload.Terminate(ctx); err != nil {
			return err
		}
	}

	if upload.info.PartialUploads != nil {
		// update upload info
		upload.info.PartialUploads = nil
		if err = upload.writeInfo(); err != nil {
			return err
		}
	}
	return
}

func (upload *sFileUpload) appendTo(file *os.File) error {
	src, err := os.Open(upload.binPath)
	if err != nil {
		return err
	}

	if _, err = io.Copy(file, src); err != nil {
		_ = src.Close()
		return err
	}

	return src.Close()
}

func (upload *sFileUpload) ServeContent(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	http.ServeFile(w, r, upload.binPath)
	return nil
}

func (upload *sFileUpload) Terminate(ctx context.Context) error {
	err := os.Remove(upload.binPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	err = os.Remove(upload.infoPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}
