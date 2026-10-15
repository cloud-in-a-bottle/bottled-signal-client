// Package account is the settings for the Signal account itself: its profile name, its PIN, and the devices linked
// to it.
package account

import (
	"bytes"
	"context"
	"errors"
	"image"
	"io"
	"net/http"
	"strconv"
	"strings"

	// Formats a QR code screenshot may come in.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
	_ "golang.org/x/image/webp"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/web"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/connector"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
)

const linkPrefix = "sgnl://linkdevice"

// Routes adds the account settings API to the web server.
func Routes(srv *web.Server, svc *bridge.Service, rpc *signalcli.Client) {
	h := &handlers{srv: srv, svc: svc, rpc: rpc}
	srv.Handle("GET /api/signal-cli", h.daemon)
	srv.Handle("GET /api/account", h.get)
	srv.Handle("POST /api/account/profile", h.profile)
	srv.Handle("POST /api/account/pin", h.pin)
	srv.Handle("GET /api/account/devices", h.devices)
	srv.Handle("POST /api/account/devices", h.link)
	srv.Handle("DELETE /api/account/devices/{id}", h.unlink)
	srv.Handle("POST /api/account/devices/decode-qr", h.decodeQR)
}

type handlers struct {
	srv *web.Server
	svc *bridge.Service
	rpc *signalcli.Client
}

type daemonResponse struct {
	Up bool `json:"up"`
}

// daemon says whether signal-cli's daemon is answering.
func (h *handlers) daemon(w http.ResponseWriter, r *http.Request) {
	web.WriteJSON(w, daemonResponse{Up: h.rpc.Up(r.Context())})
}

// client is the logged-in account's connection, or an error to show.
func (h *handlers) client(ctx context.Context) (*connector.Client, error) {
	login := h.svc.CurrentLogin(ctx)
	if login == nil {
		return nil, web.BadRequest("not registered")
	}
	return login.Client.(*connector.Client), nil
}

// fail shows signal-cli's refusals to the user as they are.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	if signalcli.IsRPCError(err) {
		err = web.BadRequest(err.Error())
	}
	h.srv.Fail(w, r, err)
}

type accountResponse struct {
	Number string `json:"number"`
	ACI    string `json:"aci"`
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request) {
	cl, err := h.client(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	web.WriteJSON(w, accountResponse{Number: cl.Account().Number, ACI: cl.ACI()})
}

func (h *handlers) profile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GivenName  string `json:"given_name"`
		FamilyName string `json:"family_name"`
	}
	if err := web.ReadJSON(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if strings.TrimSpace(req.GivenName) == "" {
		h.fail(w, r, web.BadRequest("a first name is required"))
		return
	}
	cl, err := h.client(r.Context())
	if err == nil {
		err = cl.SetProfileName(r.Context(), strings.TrimSpace(req.GivenName), strings.TrimSpace(req.FamilyName))
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) pin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PIN string `json:"pin"`
	}
	if err := web.ReadJSON(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if len(strings.TrimSpace(req.PIN)) < 4 {
		h.fail(w, r, web.BadRequest("the PIN needs at least 4 characters"))
		return
	}
	cl, err := h.client(r.Context())
	if err == nil {
		err = cl.Account().SetPIN(r.Context(), strings.TrimSpace(req.PIN))
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) devices(w http.ResponseWriter, r *http.Request) {
	cl, err := h.client(r.Context())
	var devices []signalcli.Device
	if err == nil {
		devices, err = cl.Account().Devices(r.Context())
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The first is this server itself.
	linked := []signalcli.Device{}
	for _, d := range devices {
		if d.ID != 1 {
			linked = append(linked, d)
		}
	}
	web.WriteJSON(w, linked)
}

func (h *handlers) link(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URI string `json:"uri"`
	}
	if err := web.ReadJSON(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	uri := strings.TrimSpace(req.URI)
	if !strings.HasPrefix(uri, linkPrefix) {
		h.fail(w, r, web.BadRequest("that isn't a Signal device link (sgnl://linkdevice?…)"))
		return
	}
	cl, err := h.client(r.Context())
	if err == nil {
		err = cl.Account().AddDevice(r.Context(), uri)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) unlink(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 1 {
		h.fail(w, r, web.BadRequest("invalid device"))
		return
	}
	cl, err := h.client(r.Context())
	if err == nil {
		err = cl.Account().RemoveDevice(r.Context(), id)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type decodedQR struct {
	URI string `json:"uri"`
}

// decodeQR reads a device link from a picture of its QR code: an uploaded screenshot, or a camera frame from a browser
// that can't decode QR codes itself.
func (h *handlers) decodeQR(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 20<<20))
	if err != nil {
		h.fail(w, r, web.BadRequest("couldn't read that image"))
		return
	}
	// A small file can claim enormous dimensions; decoding it would take all the memory there is.
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil || cfg.Width*cfg.Height > 40_000_000 {
		h.fail(w, r, web.BadRequest("couldn't read that image, or it's too large"))
		return
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		h.fail(w, r, web.BadRequest("couldn't read that image"))
		return
	}
	uri, err := decodeLink(img)
	if err != nil {
		h.fail(w, r, web.BadRequest(err.Error()))
		return
	}
	web.WriteJSON(w, decodedQR{URI: uri})
}

func decodeLink(img image.Image) (string, error) {
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return "", err
	}
	hints := map[gozxing.DecodeHintType]any{gozxing.DecodeHintType_TRY_HARDER: true}
	result, err := qrcode.NewQRCodeReader().Decode(bmp, hints)
	if err != nil || !strings.HasPrefix(result.GetText(), linkPrefix) {
		return "", errors.New("no Signal device link QR code found in that image")
	}
	return result.GetText(), nil
}
