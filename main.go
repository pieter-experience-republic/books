package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"

	"ebooks/internal/ircclient"

	_ "ebooks/pb_migrations" // registers the schema migration via init()
)

func main() {
	app := pocketbase.New()

	// Single IRC manager shared across all HTTP handlers and goroutines.
	// Callbacks are wired after `app` is constructed so they can update
	// PocketBase records.
	var (
		mgr     *ircclient.Manager
		mgrOnce sync.Once
	)

	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		mgrOnce.Do(func() {
			cfg := ircclient.Config{
				Logger:  log.New(os.Stdout, "", log.LstdFlags),
				WorkDir: filepath.Join(app.DataDir(), "dcc_inbox"),
			}
			cfg.Callbacks = ircclient.Callbacks{
				OnConnected: func(nick string) {
					log.Printf("ebooks: irc connected as %s", nick)
				},
				OnDisconnected: func() {
					log.Printf("ebooks: irc disconnected")
				},
				OnSearchAccepted: func(searchID string) {
					setSearchStatus(app, searchID, "searching", 0, "")
				},
				OnSearchMatches: func(searchID string, n int) {
					setSearchResultCount(app, searchID, n)
				},
				OnSearchResults: func(searchID string, books []ircclient.BookResult) {
					ingestSearchResults(app, searchID, books)
				},
				OnSearchFailed: func(searchID, errMsg string) {
					setSearchStatus(app, searchID, "failed", 0, truncate(errMsg, 500))
				},
				OnDownloadStart: func(downloadID, filename string, size int64) {
					updateDownloadStart(app, downloadID, filename, size)
				},
				OnDownloadDone: func(downloadID, filePath, filename string, size int64) {
					attachDownloadFile(app, downloadID, filePath, filename, size)
				},
				OnDownloadFailed: func(downloadID, errMsg string) {
					updateDownloadFailed(app, downloadID, truncate(errMsg, 500))
				},
			}
			mgr = ircclient.New(cfg)
			ctx, cancel := context.WithCancel(context.Background())
			go mgr.Start(ctx)
			// Stop the IRC client when the HTTP server shuts down.
			app.OnTerminate().BindFunc(func(_ *core.TerminateEvent) error {
				cancel()
				return nil
			})
			log.Printf("ebooks: irc manager started, nick=%s", mgr.Nick())
		})

		// All custom routes live under /api/ebooks/* and require an
		// authenticated user from the default `users` collection.
		api := se.Router.Group("/api/ebooks").Bind(apis.RequireAuth("users"))

		api.POST("/search", func(e *core.RequestEvent) error {
			return handleSearch(e, mgr)
		})
		api.POST("/download", func(e *core.RequestEvent) error {
			return handleDownload(e, mgr)
		})
		api.GET("/status", func(e *core.RequestEvent) error {
			return e.JSON(http.StatusOK, mgr.Status())
		})

		// Static SPA. Falls back to index.html so client-side routes work
		// (we don't have any yet, but it's free).
		se.Router.GET("/{path...}", apis.Static(os.DirFS("./pb_public"), true))

		return se.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// ---- HTTP handlers ----

func handleSearch(e *core.RequestEvent, mgr *ircclient.Manager) error {
	var body struct {
		Query string `json:"query"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	body.Query = strings.TrimSpace(body.Query)
	if body.Query == "" {
		return e.BadRequestError("query is required", nil)
	}
	if len(body.Query) > 200 {
		return e.BadRequestError("query too long (max 200 chars)", nil)
	}

	coll, err := e.App.FindCollectionByNameOrId("searches")
	if err != nil {
		return e.InternalServerError("missing searches collection", err)
	}
	rec := core.NewRecord(coll)
	rec.Set("owner", e.Auth.Id)
	rec.Set("query", body.Query)
	rec.Set("status", "queued")
	if err := e.App.Save(rec); err != nil {
		return e.InternalServerError("save search", err)
	}

	mgr.QueueSearch(rec.Id, body.Query)
	return e.JSON(http.StatusOK, map[string]any{
		"id":     rec.Id,
		"status": "queued",
	})
}

func handleDownload(e *core.RequestEvent, mgr *ircclient.Manager) error {
	var body struct {
		Command  string `json:"command"`
		ResultID string `json:"result_id"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	body.Command = strings.TrimSpace(body.Command)
	if body.Command == "" {
		return e.BadRequestError("command is required", nil)
	}
	if !strings.HasPrefix(body.Command, "!") {
		return e.BadRequestError("command must start with '!'", nil)
	}
	if len(body.Command) > 2000 {
		return e.BadRequestError("command too long", nil)
	}

	// If a result_id is supplied, verify the caller actually owns the
	// parent search before we let them link to it.
	if body.ResultID != "" {
		res, err := e.App.FindRecordById("search_results", body.ResultID)
		if err != nil {
			return e.NotFoundError("search result not found", err)
		}
		searchID := res.GetString("search")
		searchRec, err := e.App.FindRecordById("searches", searchID)
		if err != nil || searchRec.GetString("owner") != e.Auth.Id {
			return e.ForbiddenError("not your result", nil)
		}
	}

	coll, err := e.App.FindCollectionByNameOrId("downloads")
	if err != nil {
		return e.InternalServerError("missing downloads collection", err)
	}
	rec := core.NewRecord(coll)
	rec.Set("owner", e.Auth.Id)
	rec.Set("command", body.Command)
	rec.Set("status", "queued")
	if body.ResultID != "" {
		rec.Set("result", body.ResultID)
	}
	if err := e.App.Save(rec); err != nil {
		return e.InternalServerError("save download", err)
	}

	if err := mgr.QueueDownload(rec.Id, body.Command); err != nil {
		rec.Set("status", "failed")
		rec.Set("error", err.Error())
		_ = e.App.Save(rec)
		return e.InternalServerError("queue download", err)
	}
	return e.JSON(http.StatusOK, map[string]any{
		"id":     rec.Id,
		"status": "queued",
	})
}

// ---- Callback helpers (run from IRC reader goroutine) ----

func setSearchStatus(app core.App, searchID, status string, resultCount int, errMsg string) {
	rec, err := app.FindRecordById("searches", searchID)
	if err != nil {
		log.Printf("ebooks: setSearchStatus find: %v", err)
		return
	}
	rec.Set("status", status)
	if resultCount > 0 {
		rec.Set("result_count", resultCount)
	}
	if errMsg != "" {
		rec.Set("error", errMsg)
	}
	if err := app.Save(rec); err != nil {
		log.Printf("ebooks: setSearchStatus save: %v", err)
	}
}

func setSearchResultCount(app core.App, searchID string, n int) {
	rec, err := app.FindRecordById("searches", searchID)
	if err != nil {
		return
	}
	rec.Set("result_count", n)
	if err := app.Save(rec); err != nil {
		log.Printf("ebooks: setSearchResultCount save: %v", err)
	}
}

func ingestSearchResults(app core.App, searchID string, books []ircclient.BookResult) {
	resColl, err := app.FindCollectionByNameOrId("search_results")
	if err != nil {
		log.Printf("ebooks: ingestSearchResults missing collection: %v", err)
		return
	}
	kept := 0
	for _, b := range books {
		if b.Format != "epub" {
			continue
		}
		r := core.NewRecord(resColl)
		r.Set("search", searchID)
		r.Set("server", b.Server)
		r.Set("author", b.Author)
		r.Set("title", b.Title)
		r.Set("format", b.Format)
		r.Set("size", b.Size)
		r.Set("full", b.Full)
		if err := app.Save(r); err != nil {
			log.Printf("ebooks: insert search_result: %v", err)
			continue
		}
		kept++
	}
	if rec, err := app.FindRecordById("searches", searchID); err == nil {
		rec.Set("status", "complete")
		rec.Set("result_count", kept)
		if err := app.Save(rec); err != nil {
			log.Printf("ebooks: finalize search: %v", err)
		}
	}
}

func updateDownloadStart(app core.App, downloadID, filename string, size int64) {
	rec, err := app.FindRecordById("downloads", downloadID)
	if err != nil {
		return
	}
	rec.Set("status", "downloading")
	rec.Set("filename", filename)
	rec.Set("size_bytes", size)
	if err := app.Save(rec); err != nil {
		log.Printf("ebooks: updateDownloadStart save: %v", err)
	}
}

func attachDownloadFile(app core.App, downloadID, filePath, filename string, size int64) {
	rec, err := app.FindRecordById("downloads", downloadID)
	if err != nil {
		log.Printf("ebooks: attachDownloadFile find: %v", err)
		return
	}
	f, err := filesystem.NewFileFromPath(filePath)
	if err != nil {
		log.Printf("ebooks: attachDownloadFile NewFile: %v", err)
		updateDownloadFailed(app, downloadID, err.Error())
		return
	}
	if filename != "" {
		f.OriginalName = filename
	}
	rec.Set("file", f)
	rec.Set("filename", filename)
	rec.Set("size_bytes", size)
	rec.Set("status", "complete")
	if err := app.Save(rec); err != nil {
		log.Printf("ebooks: attachDownloadFile save: %v", err)
		return
	}
	// PocketBase has copied the file into its storage dir; the temp can go.
	_ = os.Remove(filePath)
}

func updateDownloadFailed(app core.App, downloadID, errMsg string) {
	rec, err := app.FindRecordById("downloads", downloadID)
	if err != nil {
		return
	}
	rec.Set("status", "failed")
	rec.Set("error", errMsg)
	if err := app.Save(rec); err != nil {
		log.Printf("ebooks: updateDownloadFailed save: %v", err)
	}
}

// ---- helpers ----

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

