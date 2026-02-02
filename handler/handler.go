package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/foae/marstek-energy-trading/service"
)

// Handler holds HTTP handler dependencies.
type Handler struct {
	svc *service.Service
}

// New creates a new HTTP handler.
func New(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// NewRouter creates and configures the HTTP router.
func (h *Handler) NewRouter() *chi.Mux {
	r := chi.NewRouter()

	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)

	r.Get("/health", h.healthHandler)
	r.Get("/metrics", h.metricsHandler())
	r.Get("/status", h.statusHandler)

	return r
}

// healthHandler returns a simple health check response.
func (h *Handler) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// StatusResponse contains current state and history.
type StatusResponse struct {
	Current service.CurrentStatus `json:"current"`
	History service.History       `json:"history"`
}

// statusHandler returns everything: current state + history.
func (h *Handler) statusHandler(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		h.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "service not ready"})
		return
	}

	resp := StatusResponse{
		Current: h.svc.GetCurrentStatus(),
		History: h.svc.GetRecorder().GetHistory(),
	}

	h.writeJSON(w, http.StatusOK, resp)
}

// writeJSON writes a JSON response.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// Prometheus metrics
var (
	batterySOC = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_battery_soc",
		Help: "Current battery state of charge (percentage)",
	})

	traderState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "energy_trader_state",
		Help: "Current trader state (1 = active)",
	}, []string{"state"})

	traderPnL = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_pnl_eur_total",
		Help: "Total profit and loss in EUR",
	})
)

func init() {
	prometheus.MustRegister(batterySOC)
	prometheus.MustRegister(traderState)
	prometheus.MustRegister(traderPnL)
}

// metricsHandler returns the Prometheus metrics handler.
func (h *Handler) metricsHandler() http.HandlerFunc {
	promHandler := promhttp.Handler()

	return func(w http.ResponseWriter, r *http.Request) {
		h.updateMetrics()
		promHandler.ServeHTTP(w, r)
	}
}

// updateMetrics updates the Prometheus metrics from current state.
func (h *Handler) updateMetrics() {
	if h.svc == nil {
		return
	}

	state := h.svc.State()
	traderState.Reset()
	traderState.WithLabelValues(string(state)).Set(1)

	recorder := h.svc.GetRecorder()
	if recorder != nil {
		pnl, _ := recorder.GetTotalPnL().Float64()
		traderPnL.Set(pnl)
	}

	// Update battery SOC from current status
	status := h.svc.GetCurrentStatus()
	batterySOC.Set(float64(status.BatterySOC))
}
