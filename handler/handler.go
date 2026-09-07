package handler

import (
	"context"
	"encoding/json"
	"math"
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
		Current: h.svc.GetCurrentStatus(r.Context()),
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
		Help: "Known total cash flow in EUR; consult energy_trader_cash_flow_unpriced_energy_kwh for completeness",
	})

	unpricedCashFlowEnergy = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_cash_flow_unpriced_energy_kwh",
		Help: "Cumulative grid charge and discharge energy excluded from known cash flow because its tariff was unavailable",
	})
)

func init() {
	prometheus.MustRegister(batterySOC)
	prometheus.MustRegister(traderState)
	prometheus.MustRegister(traderPnL)
	prometheus.MustRegister(unpricedCashFlowEnergy)
}

// metricsHandler returns the Prometheus metrics handler.
func (h *Handler) metricsHandler() http.HandlerFunc {
	promHandler := promhttp.Handler()

	return func(w http.ResponseWriter, r *http.Request) {
		h.updateMetrics(r.Context())
		promHandler.ServeHTTP(w, r)
	}
}

// updateMetrics updates the Prometheus metrics from current state.
func (h *Handler) updateMetrics(ctx context.Context) {
	if h.svc == nil {
		return
	}

	state := h.svc.State()
	traderState.Reset()
	traderState.WithLabelValues(string(state)).Set(1)

	recorder := h.svc.GetRecorder()
	if recorder != nil {
		pnl, unpricedKWh := historyMetricValues(recorder.GetHistory())
		traderPnL.Set(pnl)
		unpricedCashFlowEnergy.Set(unpricedKWh)
	}

	// Update battery SOC from current status
	status := h.svc.GetCurrentStatus(ctx)
	if status.BatteryAvailable {
		batterySOC.Set(float64(status.BatterySOC))
	} else {
		batterySOC.Set(math.NaN())
	}
}

func historyMetricValues(history service.History) (float64, float64) {
	pnl, _ := history.TotalPnL.Float64()
	unpricedKWh := 0.0
	for _, day := range history.Days {
		value, _ := day.CashFlowUnpricedKWh.Float64()
		unpricedKWh += value
	}
	return pnl, unpricedKWh
}
