package importer

import (
	"errors"
	"net/http"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/web"
)

// Routes adds the import API to the web server.
func (m *Manager) Routes(srv *web.Server) {
	h := &handlers{m: m, srv: srv}
	srv.Handle("GET /api/import", h.status)
	srv.Handle("POST /api/import", h.start)
	srv.Handle("PUT /api/import/chunk", h.chunk)
	srv.Handle("POST /api/import/run", h.run)
	srv.Handle("DELETE /api/import", h.discard)
}

type handlers struct {
	m   *Manager
	srv *web.Server
}

func (h *handlers) status(w http.ResponseWriter, r *http.Request) {
	web.WriteJSON(w, h.m.Status())
}

func (h *handlers) start(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Size int64 `json:"size"`
	}
	if err := web.ReadJSON(r, &req); err != nil {
		h.srv.Fail(w, r, err)
		return
	}
	if err := h.m.Start(req.Size); err != nil {
		h.fail(w, r, err)
		return
	}
	web.WriteJSON(w, h.m.Status())
}

// chunk appends one chunk. On a wrong offset (eg a retry of a chunk that did arrive), it answers 409 with the
// status, whose `received` says where to continue.
func (h *handlers) chunk(w http.ResponseWriter, r *http.Request) {
	offset, err := web.QueryInt(r, "offset", -1)
	if err != nil {
		h.srv.Fail(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxChunkBytes)
	if err = h.m.Append(offset, r.Body); errors.Is(err, ErrWrongOffset) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		web.WriteJSON(w, h.m.Status())
		return
	} else if err != nil {
		h.fail(w, r, err)
		return
	}
	web.WriteJSON(w, h.m.Status())
}

func (h *handlers) run(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RecoveryKey string `json:"recovery_key"`
	}
	if err := web.ReadJSON(r, &req); err != nil {
		h.srv.Fail(w, r, err)
		return
	}
	if err := h.m.Run(r.Context(), req.RecoveryKey); err != nil {
		h.fail(w, r, err)
		return
	}
	web.WriteJSON(w, h.m.Status())
}

func (h *handlers) discard(w http.ResponseWriter, r *http.Request) {
	if err := h.m.Discard(); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fail shows problems with the upload or what was asked to the user, rather than as server errors.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	if IsUserError(err) {
		err = web.BadRequest(err.Error())
	}
	h.srv.Fail(w, r, err)
}
