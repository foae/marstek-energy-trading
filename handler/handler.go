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

	opportunityAdjustedPnL = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_opportunity_adjusted_pnl_eur_total",
		Help: "Known cash flow less known signed forgone-export value; not a counterfactual savings metric",
	})

	unattributedChargeEnergy = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_unattributed_charge_energy_kwh",
		Help: "Scheduled charge energy before the first successful P1 source observation",
	})

	unpricedOpportunityEnergy = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_opportunity_unpriced_energy_kwh",
		Help: "Solar-attributed charged energy excluded from forgone-export valuation because its export tariff was unavailable",
	})

	measuredEfficiency = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_measured_efficiency_percent",
		Help: "Energy-weighted operational AC round-trip efficiency including standby (percent); NaN until a valid matched-SOC window completes",
	})
	measuredEfficiencyWindows = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_measured_efficiency_windows",
		Help: "Accepted complete matched-SOC measurement windows",
	})
	rejectedEfficiencyWindows = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_rejected_efficiency_windows",
		Help: "Incomplete or invalid efficiency measurement windows discarded; the sum of the three reason gauges below, except for aggregates recorded before they existed",
	})
	unqualifiedEfficiencyWindows = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_unqualified_efficiency_windows",
		Help: "Efficiency windows discarded because the SOC swing was too small to measure; expected churn, not a fault",
	})
	interruptedEfficiencyWindows = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_interrupted_efficiency_windows",
		Help: "Efficiency windows discarded because telemetry stopped or jumped, leaving a hole in the energy integral",
	})
	implausibleEfficiencyWindows = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "energy_trader_implausible_efficiency_windows",
		Help: "Efficiency windows discarded because their measured energies cannot describe a round trip",
	})
)

func init() {
	prometheus.MustRegister(batterySOC)
	prometheus.MustRegister(traderState)
	prometheus.MustRegister(traderPnL)
	prometheus.MustRegister(unpricedCashFlowEnergy, opportunityAdjustedPnL, unattributedChargeEnergy, unpricedOpportunityEnergy)
	prometheus.MustRegister(measuredEfficiency, measuredEfficiencyWindows, rejectedEfficiencyWindows)
	prometheus.MustRegister(unqualifiedEfficiencyWindows, interruptedEfficiencyWindows, implausibleEfficiencyWindows)
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
		pnl, unpricedKWh, opportunityAdjusted, unattributedKWh, opportunityUnpricedKWh := historyMetricValues(recorder.GetHistory())
		traderPnL.Set(pnl)
		unpricedCashFlowEnergy.Set(unpricedKWh)
		opportunityAdjustedPnL.Set(opportunityAdjusted)
		unattributedChargeEnergy.Set(unattributedKWh)
		unpricedOpportunityEnergy.Set(opportunityUnpricedKWh)
	}

	// Update battery SOC from current status
	status := h.svc.GetCurrentStatus(ctx)
	if status.MeasuredEfficiency.Percent == nil {
		measuredEfficiency.Set(math.NaN())
	} else {
		measuredEfficiency.Set(*status.MeasuredEfficiency.Percent)
	}
	measuredEfficiencyWindows.Set(float64(status.MeasuredEfficiency.Cycles))
	rejectedEfficiencyWindows.Set(float64(status.MeasuredEfficiency.RejectedWindows))
	unqualifiedEfficiencyWindows.Set(float64(status.MeasuredEfficiency.UnqualifiedWindows))
	interruptedEfficiencyWindows.Set(float64(status.MeasuredEfficiency.InterruptedWindows))
	implausibleEfficiencyWindows.Set(float64(status.MeasuredEfficiency.ImplausibleWindows))
	if status.BatteryAvailable {
		batterySOC.Set(float64(status.BatterySOC))
	} else {
		batterySOC.Set(math.NaN())
	}
}

func historyMetricValues(history service.History) (pnl, unpricedKWh, opportunityAdjustedPnL, unattributedKWh, opportunityUnpricedKWh float64) {
	pnl, _ = history.TotalPnL.Float64()
	opportunityAdjustedPnL, _ = history.TotalOpportunityAdjustedPnLEUR.Float64()
	unattributedKWh, _ = history.TotalUnattributedChargeKWh.Float64()
	for _, day := range history.Days {
		cashFlowUnpriced, _ := day.CashFlowUnpricedKWh.Float64()
		totalUnpriced, _ := day.UnpricedKWh.Float64()
		unpricedKWh += cashFlowUnpriced
		opportunityUnpricedKWh += totalUnpriced - cashFlowUnpriced
	}
	return pnl, unpricedKWh, opportunityAdjustedPnL, unattributedKWh, opportunityUnpricedKWh
}
