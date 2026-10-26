package exporter

import (
	"errors"
	"net/http"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/web"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
)

// Routes adds the export API to the web server.
func (m *Manager) Routes(srv *web.Server) {
	h := &handlers{m: m, srv: srv}
	srv.Handle("GET /api/export", h.status)
	srv.Handle("POST /api/export", h.start)
	srv.Handle("DELETE /api/export", h.delete)
	srv.Handle("GET /api/export/{id}/SignalBackups.zip", h.download)
}

type handlers struct {
	m   *Manager
	srv *web.Server
}

func (h *handlers) status(w http.ResponseWriter, r *http.Request) {
	web.WriteJSON(w, h.m.Status())
}

func (h *handlers) start(w http.ResponseWriter, r *http.Request) {
	if err := h.m.Start(r.Context()); err != nil {
		h.fail(w, r, err)
		return
	}
	web.WriteJSON(w, h.m.Status())
}

func (h *handlers) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.m.Delete(); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) download(w http.ResponseWriter, r *http.Request) {
	backupDir, done, err := h.m.download(r.PathValue("id"))
	if err != nil {
		h.srv.Fail(w, r, err)
		return
	}
	defer done()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="SignalBackups.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	if err = writeZip(backupDir, w); err != nil {
		// Once the zip has started, the response can only be cut short.
		h.m.log.Err(err).Msg("Export download failed")
	}
}

func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrBusy) || errors.Is(err, ErrDownloading) || errors.Is(err, ErrNotLoggedIn) || errors.Is(err, signalcli.ErrSecretsUnavailable) {
		err = web.BadRequest(err.Error())
	}
	h.srv.Fail(w, r, err)
}
