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
	"strings"

	"github.com/xmapst/tusx/types"
)

var defaultFilePerm = os.FileMode(0664)
var defaultDirectoryPerm = os.FileMode(0754)

type SFileStore struct {
	Dir    string
	locker types.ILocker
}

func New(dir string, locker types.ILocker) (*SFileStore, error) {
	_ = os.MkdirAll(dir, defaultDirectoryPerm)
	return &SFileStore{
		Dir:    dir,
		locker: locker,
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
	infoLock, err := store.locker.NewLock(strings.ReplaceAll(strings.TrimSpace(upload.infoPath), "/", ":"))
	if err != nil {
		return nil, err
	}
	upload.infoLock = infoLock
	upload.binLock, err = store.locker.NewLock(strings.ReplaceAll(strings.TrimSpace(upload.binPath), "/", ":"))
	if err != nil {
		return nil, err
	}
	if err = upload.binLock.Lock(ctx); err != nil {
		return nil, err
	}
	defer upload.binLock.Unlock()
	if err = upload.createFile(upload.binPath, nil); err != nil {
		return nil, err
	}
	if err = upload.writeInfo(ctx); err != nil {
		return nil, err
	}
	return upload, nil
}

func (store *SFileStore) GetUpload(ctx context.Context, id string) (types.IUpload, error) {
	upload := &sFileUpload{
		infoPath: store.infoPath(id),
		binPath:  store.binPath(id),
	}
	infoLock, err := store.locker.NewLock(upload.infoPath)
	if err != nil {
		return nil, err
	}
	upload.infoLock = infoLock
	upload.binLock, err = store.locker.NewLock(upload.binPath)
	if err != nil {
		return nil, err
	}

	if err = upload.readInfo(ctx); err != nil {
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
	infoLock types.ILock
	binLock  types.ILock
	info     types.FileInfo
	infoPath string
	binPath  string
}

func (upload *sFileUpload) writeInfo(ctx context.Context) error {
	if err := upload.infoLock.Lock(ctx); err != nil {
		return err
	}
	defer upload.infoLock.Unlock()
	data, err := json.Marshal(upload.info)
	if err != nil {
		return err
	}
	return upload.createFile(upload.infoPath, data)
}

func (upload *sFileUpload) readInfo(ctx context.Context) error {
	if err := upload.infoLock.Lock(ctx); err != nil {
		return err
	}
	defer upload.infoLock.Unlock()
	data, err := os.ReadFile(upload.infoPath)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &upload.info); err != nil {
		return err
	}
	return nil
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
	return upload.writeInfo(ctx)
}

func (upload *sFileUpload) GetInfo(ctx context.Context) (types.FileInfo, error) {
	if err := upload.readInfo(ctx); err != nil {
		return types.FileInfo{}, err
	}
	return upload.info, nil
}

func (upload *sFileUpload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	return os.Open(upload.binPath)
}

func (upload *sFileUpload) WriteChunk(ctx context.Context, src io.Reader) (int64, error) {
	if err := upload.binLock.Lock(ctx); err != nil {
		return 0, err
	}
	defer upload.binLock.Unlock()
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
	return n, upload.writeInfo(ctx)
}

func (upload *sFileUpload) ConcatUploads(ctx context.Context, uploads []types.IUpload) (err error) {
	if err = upload.binLock.Lock(ctx); err != nil {
		return err
	}
	defer upload.binLock.Unlock()
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
		if err = _partialUpload.appendTo(ctx, file); err != nil {
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
		if err = upload.writeInfo(ctx); err != nil {
			return err
		}
	}
	return
}

func (upload *sFileUpload) appendTo(ctx context.Context, file *os.File) error {
	if err := upload.binLock.Lock(ctx); err != nil {
		return err
	}
	defer upload.binLock.Unlock()
	src, err := os.Open(upload.binPath)
	if err != nil {
		return err
	}
	defer func() {
		cerr := src.Close()
		if err == nil {
			err = cerr
		}
	}()

	n, err := io.Copy(file, src)
	if err != nil {
		_ = src.Close()
		return err
	}
	upload.info.Offset += n

	return upload.writeInfo(ctx)
}

func (upload *sFileUpload) ServeContent(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if err := upload.binLock.Lock(ctx); err != nil {
		return err
	}
	defer upload.binLock.Unlock()
	http.ServeFile(w, r, upload.binPath)
	return nil
}

func (upload *sFileUpload) Terminate(ctx context.Context) error {
	if err := upload.binLock.Lock(ctx); err != nil {
		return err
	}
	defer upload.binLock.Unlock()
	if err := upload.infoLock.Lock(ctx); err != nil {
		return err
	}
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
