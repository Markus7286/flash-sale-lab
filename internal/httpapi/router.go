// Package httpapi mounts every reservation backend under its own version prefix,
// so a load test drives v1 and v2 through the same handlers on the same machine.
//
// There is no authentication on purpose: the caller names itself with an
// X-User-Id header, and identity, carts and payment are out of scope.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"flash-sale/internal/flashsale"
)

const (
	headerUserID    = "X-User-Id"
	headerRequestID = "X-Request-Id"
)

// Backend is one reservation implementation mounted at /{Version}/....
type Backend struct {
	Version  string // "v1", "v2"
	Reserver flashsale.Reserver
}

type Server struct {
	backends map[string]flashsale.Reserver
	fallback string // version served by the unversioned /flash-sale alias
	logger   *slog.Logger
}

// NewServer makes the last backend given the target of the unversioned alias.
func NewServer(logger *slog.Logger, backends ...Backend) *Server {
	s := &Server{backends: make(map[string]flashsale.Reserver, len(backends)), logger: logger}
	for _, b := range backends {
		s.backends[b.Version] = b.Reserver
		s.fallback = b.Version
	}
	return s
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /backends", s.handleBackends)

	mux.HandleFunc("POST /{version}/flash-sale", s.handleFlashSale)
	mux.HandleFunc("GET /{version}/skus/{sku}/stock", s.handleStock)
	mux.HandleFunc("PUT /{version}/admin/skus/{sku}/stock", s.handleSeedStock)

	// Alias so curl and the smoke test need not know which backend is current.
	mux.HandleFunc("POST /flash-sale", s.handleFlashSale)
	return mux
}

// reserverFor writes the error response itself when there is no such backend.
func (s *Server) reserverFor(w http.ResponseWriter, r *http.Request) (flashsale.Reserver, bool) {
	version := r.PathValue("version")
	if version == "" {
		version = s.fallback
	}
	reserver, ok := s.backends[version]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown backend version "+version)
		return nil, false
	}
	return reserver, true
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleBackends(w http.ResponseWriter, _ *http.Request) {
	out := make(map[string]string, len(s.backends))
	for version, reserver := range s.backends {
		out[version] = reserver.Name()
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": out, "default": s.fallback})
}

type flashSaleRequest struct {
	SKU string `json:"sku"`
	Qty int64  `json:"qty"`
}

type flashSaleResponse struct {
	Status    string `json:"status"`
	Backend   string `json:"backend"`
	SKU       string `json:"sku"`
	RequestID string `json:"request_id"`
	Remaining int64  `json:"remaining"`
}

func (s *Server) handleFlashSale(w http.ResponseWriter, r *http.Request) {
	reserver, ok := s.reserverFor(w, r)
	if !ok {
		return
	}

	userID := r.Header.Get(headerUserID)
	requestID := r.Header.Get(headerRequestID)
	if userID == "" || requestID == "" {
		writeError(w, http.StatusBadRequest,
			"both "+headerUserID+" and "+headerRequestID+" headers are required")
		return
	}

	var body flashSaleRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.SKU == "" {
		writeError(w, http.StatusBadRequest, "sku is required")
		return
	}
	if body.Qty == 0 {
		body.Qty = 1
	}
	if body.Qty < 0 {
		writeError(w, http.StatusBadRequest, "qty must be positive")
		return
	}

	res, err := reserver.Reserve(r.Context(), flashsale.Request{
		SKU:       body.SKU,
		UserID:    userID,
		RequestID: requestID,
		Qty:       body.Qty,
	})
	if err != nil {
		s.logger.Error("reserve failed", "backend", reserver.Name(),
			"sku", body.SKU, "user_id", userID, "request_id", requestID, "err", err)
		writeError(w, http.StatusInternalServerError, "reservation failed")
		return
	}

	writeJSON(w, statusToCode(res.Status), flashSaleResponse{
		Status:    res.Status.String(),
		Backend:   reserver.Name(),
		SKU:       body.SKU,
		RequestID: requestID,
		Remaining: res.Remaining,
	})
}

// statusToCode returns 409 for sold-out and per-user-limit: the request was well
// formed, the current state refused it.
func statusToCode(status flashsale.Status) int {
	switch status {
	case flashsale.StatusReserved:
		return http.StatusCreated
	case flashsale.StatusDuplicate:
		return http.StatusOK
	case flashsale.StatusSoldOut, flashsale.StatusUserLimit:
		return http.StatusConflict
	case flashsale.StatusUnknownSKU:
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) handleStock(w http.ResponseWriter, r *http.Request) {
	reserver, ok := s.reserverFor(w, r)
	if !ok {
		return
	}

	sku := r.PathValue("sku")
	stats, err := reserver.Stats(r.Context(), sku)
	if errors.Is(err, flashsale.ErrUnknownSKU) {
		writeError(w, http.StatusNotFound, "unknown sku")
		return
	}
	if err != nil {
		s.logger.Error("read stats failed", "backend", reserver.Name(), "sku", sku, "err", err)
		writeError(w, http.StatusInternalServerError, "could not read stock")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sku":          sku,
		"backend":      reserver.Name(),
		"remaining":    stats.Remaining,
		"reservations": stats.Reservations,
		"buyers":       stats.Buyers,
		"units_sold":   stats.UnitsSold,
	})
}

type seedRequest struct {
	Stock int64 `json:"stock"`
}

func (s *Server) handleSeedStock(w http.ResponseWriter, r *http.Request) {
	reserver, ok := s.reserverFor(w, r)
	if !ok {
		return
	}

	sku := r.PathValue("sku")
	var body seedRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Stock < 0 {
		writeError(w, http.StatusBadRequest, "stock must not be negative")
		return
	}

	if err := reserver.SeedStock(r.Context(), sku, body.Stock); err != nil {
		s.logger.Error("seed stock failed", "backend", reserver.Name(), "sku", sku, "err", err)
		writeError(w, http.StatusInternalServerError, "could not seed stock")
		return
	}
	s.logger.Info("stock seeded", "backend", reserver.Name(), "sku", sku, "stock", body.Stock)
	writeJSON(w, http.StatusOK, map[string]any{
		"sku": sku, "backend": reserver.Name(), "stock": body.Stock,
	})
}

// maxBodyBytes is generous for the few-field bodies this API accepts and small
// enough that a client cannot pin a handler by streaming an endless one.
const maxBodyBytes = 64 << 10

// decodeJSON writes the error response itself and reports whether decoding succeeded.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
	} else {
		writeError(w, http.StatusBadRequest, "malformed json body")
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
