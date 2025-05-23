package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pires/go-proxyproto"

	"github.com/xmapst/tusx"
	filestore "github.com/xmapst/tusx/storage/file"
	"github.com/xmapst/tusx/types"
)

//go:embed index.html
var indexHtml []byte

var (
	host      string
	port      int
	uploadDir string
	redisURL  string
)

func init() {
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelDebug,
	})
	slog.SetDefault(slog.New(h))
}

func main() {
	flag.StringVar(&host, "host", "0.0.0.0", "listen host addr")
	flag.IntVar(&port, "port", 8080, "listen port")
	flag.StringVar(&uploadDir, "upload-dir", "./uploads", "upload dir")
	flag.StringVar(&redisURL, "redis-url", "redis://:123456@localhost:6379/0", "redis url")
	flag.Parse()

	serverCtx, cancelServerCtx := context.WithCancelCause(context.Background())
	//_ = os.MkdirAll(uploadDir, os.FileMode(0754))
	//slog.Info("starting...")
	//locker, err := redislocker.New(redisURL)
	//if err != nil {
	//	slog.Error("failed to create redis locker", "err", err)
	//	os.Exit(255)
	//}
	//
	store, err := filestore.New(filepath.Join(os.TempDir(), ".tusd"))
	if err != nil {
		slog.Error("failed to create file store", "err", err)
		os.Exit(255)
	}
	tusxHandler, err := tusx.New(&tusx.SConfig{
		BasePath: "/files",
		Store:    store,
		PreUploadCreateCallback: func(hook types.HookEvent) (types.HTTPResponse, types.FileInfoChanges, error) {
			id := types.Uid()
			// 从metadata中获取信息
			gameID, ok := hook.Upload.MetaData["game_id"]
			if !ok {
				return types.HTTPResponse{
					StatusCode: http.StatusBadRequest,
					Body:       "game_id is required",
				}, types.FileInfoChanges{}, errors.New("game_id is required")
			}
			regionID, ok := hook.Upload.MetaData["region_id"]
			if !ok {
				return types.HTTPResponse{
					StatusCode: http.StatusBadRequest,
					Body:       "region_id is required",
				}, types.FileInfoChanges{}, errors.New("region_id is required")
			}
			return types.HTTPResponse{}, types.FileInfoChanges{
				ID: filepath.Join(fmt.Sprintf("game-%s", gameID), fmt.Sprintf("region-%s", regionID), "temp", "autoops-api-upload-file", id),
			}, nil
		},
		PreFinishResponseCallback: func(hook types.HookEvent) (types.HTTPResponse, error) {
			//if hook.Upload.IsFinal {
			//	src := filepath.Join(os.TempDir(), ".tusd", hook.Upload.ID)
			//	dst := filepath.Join(uploadDir, hook.Upload.ID)
			//	slog.Info("copy file", "src", src, "dst", dst)
			//	if err = filestore.CopyFile(src, dst); err != nil {
			//		return types.HTTPResponse{
			//			StatusCode: http.StatusInternalServerError,
			//			Body:       "failed to copy file",
			//		}, err
			//	}
			//}
			return types.HTTPResponse{
				Headers: map[string]string{
					"ID":   filepath.Base(hook.Upload.ID),
					"Path": filepath.Dir(hook.Upload.ID),
				},
			}, nil
		},
	})
	if err != nil {
		slog.Error("failed to create tusx handler", "err", err)
		os.Exit(255)
	}
	tusxHandler.SubscribeCompleteUploads(serverCtx, func(event types.HookEvent) error {
		slog.Info("upload completed",
			"id", event.Upload.ID,
			"size", event.Upload.Size,
			"offset", event.Upload.Offset,
			"meta", event.Upload.MetaData,
		)
		return nil
	})

	handler := http.NewServeMux()
	handler.Handle("/files", http.StripPrefix("/files", tusxHandler))
	handler.Handle("/files/", http.StripPrefix("/files/", tusxHandler))
	handler.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(indexHtml)
	})

	ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
	if err != nil {
		slog.Error("failed to listen", "err", err)
		os.Exit(255)
	}
	slog.Info("listen on", "addr", ln.Addr().String())

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 60 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		MaxHeaderBytes:    3 << 20, // 3MB
		BaseContext: func(_ net.Listener) context.Context {
			return serverCtx
		},
	}
	shutdownComplete := setupSignalHandler(server, cancelServerCtx)
	err = server.Serve(&proxyproto.Listener{Listener: ln})
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownComplete
	} else if err != nil {
		slog.Error("failed to serve", "err", err)
		os.Exit(255)
	}
}

func setupSignalHandler(server *http.Server, cancelServerCtx context.CancelCauseFunc) <-chan struct{} {
	shutdownComplete := make(chan struct{})

	// We read up to two signals, so use a capacity of 2 here to not miss any signal
	c := make(chan os.Signal, 2)

	// os.Interrupt is mapped to SIGINT on Unix and to the termination instructions on Windows.
	// On Unix we also listen to SIGTERM.
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	// When closing the server, cancel its context so all open requests shut down as well.
	// See context.go for the logic.
	server.RegisterOnShutdown(func() {
		cancelServerCtx(http.ErrServerClosed)
	})

	go func() {
		// First interrupt signal
		<-c
		slog.Info("Received interrupt signal. Shutting down tusd...")

		// Wait for second interrupt signal, while also shutting down the existing server
		go func() {
			<-c
			slog.Info("Received second interrupt signal. Exiting immediately!")
			os.Exit(1)
		}()

		// Shutdown the server, but with a user-specified timeout
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := server.Shutdown(ctx)

		if err == nil {
			slog.Info("Shutdown completed. Goodbye!")
		} else if errors.Is(err, context.DeadlineExceeded) {
			slog.Info("Shutdown timeout exceeded. Exiting immediately!")
		} else {
			slog.Error("Failed to shutdown gracefully: ", "err", err)
		}

		close(shutdownComplete)
	}()

	return shutdownComplete
}
