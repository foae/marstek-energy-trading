package esphome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/foae/marstek-energy-trading/clients/marstek"
	"strings"
	"sync"
	"testing"
	"time"
)

func newControlTestServer(t *testing.T, failOption string, initialRSMode ...string) (*httptest.Server, *[]string) {
	t.Helper()
	calledPaths := make([]string, 0)
	rs485Mode := "disable"
	if len(initialRSMode) > 0 {
		rs485Mode = initialRSMode[0]
	}
	values := map[string]string{
		"/select/RS485 Control Mode":        rs485Mode,
		"/select/Forcible Charge⁄Discharge": "stop",
		"/number/Forcible Charge Power":     "0",
		"/number/Forcible Discharge Power":  "0",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"value": values[r.URL.Path], "state": values[r.URL.Path]})
			return
		}

		calledPaths = append(calledPaths, r.URL.String())
		if failOption != "" && r.URL.Query().Get("option") == failOption {
			http.Error(w, failOption+" failed", http.StatusInternalServerError)
			return
		}
		entityPath := strings.TrimSuffix(r.URL.Path, "/set")
		if option := r.URL.Query().Get("option"); option != "" {
			values[entityPath] = option
		}
		if value := r.URL.Query().Get("value"); value != "" {
			values[entityPath] = value
		}
		w.WriteHeader(http.StatusOK)
	}))
	return server, &calledPaths
}

func TestConnect_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/text_sensor/Device%20Name" || r.URL.Path == "/text_sensor/Device Name" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"text_sensor-device_name","state":"Marstek Venus E"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	err := client.Connect()
	if err != nil {
		t.Errorf("Connect() error = %v, want nil", err)
	}
}

func TestConnect_Failure(t *testing.T) {
	// Connect to a non-existent server
	client := New("http://127.0.0.1:1", 11)
	err := client.Connect()
	if err == nil {
		t.Error("Connect() error = nil, want error for unreachable device")
	}
}

func TestDiscover(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "Device"):
			w.Write([]byte(`{"id":"text_sensor-device_name","state":"Marstek Venus E"}`))
		case strings.Contains(r.URL.Path, "ip"):
			w.Write([]byte(`{"id":"text_sensor-esp_ip","state":"192.168.1.50"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.URL, 11)
	info, err := client.Discover()
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if info.Device != "Marstek Venus E" {
		t.Errorf("Device = %q, want %q", info.Device, "Marstek Venus E")
	}
	if info.IP != "192.168.1.50" {
		t.Errorf("IP = %q, want %q", info.IP, "192.168.1.50")
	}
}

func TestGetBatteryStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "State%20Of%20Charge") || strings.Contains(r.URL.Path, "State Of Charge"):
			w.Write([]byte(`{"id":"sensor-soc","value":75,"state":"75 %"}`))
		case strings.Contains(r.URL.Path, "Temperature"):
			w.Write([]byte(`{"id":"sensor-temp","value":25.5,"state":"25.5 °C"}`))
		case strings.Contains(r.URL.Path, "Remaining%20Capacity") || strings.Contains(r.URL.Path, "Remaining Capacity"):
			w.Write([]byte(`{"id":"sensor-cap","value":3.84,"state":"3.84 kWh"}`))
		case strings.Contains(r.URL.Path, "Total%20Energy") || strings.Contains(r.URL.Path, "Total Energy"):
			w.Write([]byte(`{"id":"sensor-total","value":5.12,"state":"5.12 kWh"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.URL, 11)
	status, err := client.GetBatteryStatus()
	if err != nil {
		t.Fatalf("GetBatteryStatus() error = %v", err)
	}
	if status.SOC != 75 {
		t.Errorf("SOC = %d, want 75", status.SOC)
	}
	if status.Temperature != 25.5 {
		t.Errorf("Temperature = %v, want 25.5", status.Temperature)
	}
	if status.Capacity != 3840 { // kWh * 1000 = Wh
		t.Errorf("Capacity = %v, want 3840", status.Capacity)
	}
	if !status.ChargingFlag {
		t.Error("ChargingFlag = false, want true (SOC < 100)")
	}
	if !status.DischargFlag {
		t.Error("DischargFlag = false, want true (SOC > 11)")
	}
}

func TestGetBatteryStatus_PartialFailure(t *testing.T) {
	// Only SOC available, other sensors fail
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "State%20Of%20Charge") || strings.Contains(r.URL.Path, "State Of Charge") {
			w.Write([]byte(`{"id":"sensor-soc","value":50,"state":"50 %"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	status, err := client.GetBatteryStatus()
	if err != nil {
		t.Fatalf("GetBatteryStatus() error = %v", err)
	}
	if status.SOC != 50 {
		t.Errorf("SOC = %d, want 50", status.SOC)
	}
	// Other fields should be zero but not cause failure
	if status.Temperature != 0 {
		t.Errorf("Temperature = %v, want 0 (unavailable)", status.Temperature)
	}
}

func TestGetBatteryStatus_ChargingFlags(t *testing.T) {
	tests := []struct {
		name         string
		soc          int
		wantCharging bool
		wantDischarg bool
	}{
		{"SOC 100 - full", 100, false, true},
		{"SOC 11 - min", 11, true, false},
		{"SOC 10 - below min", 10, true, false},
		{"SOC 50 - normal", 50, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			soc := tt.soc // capture for closure
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "State%20Of%20Charge") || strings.Contains(r.URL.Path, "State Of Charge") {
					fmt.Fprintf(w, `{"id":"sensor-soc","value":%d,"state":"%d %%"}`, soc, soc)
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()

			client := New(server.URL, 11)
			status, err := client.GetBatteryStatus()
			if err != nil {
				t.Fatalf("GetBatteryStatus() error = %v", err)
			}
			if status.ChargingFlag != tt.wantCharging {
				t.Errorf("ChargingFlag = %v, want %v", status.ChargingFlag, tt.wantCharging)
			}
			if status.DischargFlag != tt.wantDischarg {
				t.Errorf("DischargFlag = %v, want %v", status.DischargFlag, tt.wantDischarg)
			}
		})
	}
}

func TestCharge(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	err := client.Charge(2500, 300)
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("Charge() error = %v", err)
	}

	if len(calledPaths) != 3 {
		t.Fatalf("expected 3 calls, got %d: %v", len(calledPaths), calledPaths)
	}

	// First call should enable RS485 control mode
	if !strings.Contains(calledPaths[0], "RS485%20Control%20Mode") || !strings.Contains(calledPaths[0], "option=enable") {
		t.Errorf("first call should enable RS485 control mode, got: %s", calledPaths[0])
	}

	// Second call should set charge power
	if !strings.Contains(calledPaths[1], "Charge%20Power") || !strings.Contains(calledPaths[1], "value=2500") {
		t.Errorf("second call should set charge power, got: %s", calledPaths[1])
	}

	// Third call should force mode to charge
	if !strings.Contains(calledPaths[2], "option=charge") {
		t.Errorf("third call should force mode to charge, got: %s", calledPaths[2])
	}
}

func TestChargeDoesNotRewriteEnabledRS485Mode(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "", "enable")
	defer server.Close()

	if err := New(server.URL, 11).Charge(500, 300); err != nil {
		t.Fatalf("Charge() error = %v", err)
	}
	calledPaths := *calledPathsPtr
	if len(calledPaths) != 2 {
		t.Fatalf("calls = %d, want power + charge mode: %v", len(calledPaths), calledPaths)
	}
	for _, path := range calledPaths {
		if strings.Contains(path, "RS485%20Control%20Mode") {
			t.Fatalf("already-enabled RS485 mode was rewritten: %v", calledPaths)
		}
	}
}

func TestControlConfirmationTimeoutCoversESPHomePublicationCycle(t *testing.T) {
	if controlConfirmationTimeout <= espHomeControlPublicationInterval {
		t.Fatalf(
			"control confirmation timeout %s must exceed ESPHome publication interval %s",
			controlConfirmationTimeout,
			espHomeControlPublicationInterval,
		)
	}
}

func TestChargeWaitsForControlConfirmation(t *testing.T) {
	values := map[string]string{
		"/select/RS485 Control Mode":        "disable",
		"/select/Forcible Charge⁄Discharge": "stop",
		"/number/Forcible Charge Power":     "0",
	}
	pending := make(map[string]string)
	getCounts := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entityPath := strings.TrimSuffix(r.URL.Path, "/set")
		if r.Method == http.MethodPost {
			pending[entityPath] = r.URL.Query().Get("option")
			if pending[entityPath] == "" {
				pending[entityPath] = r.URL.Query().Get("value")
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		if next, ok := pending[entityPath]; ok {
			getCounts[entityPath]++
			if getCounts[entityPath] >= 2 {
				if entityPath == "/number/Forcible Charge Power" {
					next = "500.4"
				}
				values[entityPath] = next
				delete(pending, entityPath)
			}
		}
		if strings.HasPrefix(entityPath, "/number/") {
			_, _ = fmt.Fprintf(w, `{"value":%s}`, values[entityPath])
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value": values[entityPath]})
	}))
	defer server.Close()

	if err := New(server.URL, 11).Charge(500, 300); err != nil {
		t.Fatalf("Charge() error = %v", err)
	}
	for _, entityPath := range []string{
		"/select/RS485 Control Mode",
		"/number/Forcible Charge Power",
		"/select/Forcible Charge⁄Discharge",
	} {
		if getCounts[entityPath] < 2 {
			t.Errorf("%s GET count = %d, want at least 2", entityPath, getCounts[entityPath])
		}
	}
}

func TestChargeRetriesUnconfirmedSelectWrite(t *testing.T) {
	values := map[string]string{
		"/select/RS485 Control Mode":        "disable",
		"/select/Forcible Charge⁄Discharge": "stop",
		"/number/Forcible Charge Power":     "0",
	}
	rs485Writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entityPath := strings.TrimSuffix(r.URL.Path, "/set")
		if r.Method == http.MethodPost {
			option := r.URL.Query().Get("option")
			if entityPath == "/select/RS485 Control Mode" {
				rs485Writes++
				if rs485Writes == 1 {
					w.WriteHeader(http.StatusOK)
					return
				}
			}
			if option != "" {
				values[entityPath] = option
			}
			if value := r.URL.Query().Get("value"); value != "" {
				values[entityPath] = value
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"value": values[entityPath]})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := New(server.URL, 11)
	// Production spaces retries 5 s apart; keep the test fast without weakening it.
	client.writeRetryDelay = 20 * time.Millisecond
	if err := client.ChargeContext(ctx, 500, 300); err != nil {
		t.Fatalf("ChargeContext() error = %v", err)
	}
	if rs485Writes != 2 {
		t.Fatalf("RS485 writes = %d, want 2", rs485Writes)
	}
}

func TestChargeContext_CancelsControlConfirmation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value": "disable"})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := New(server.URL, 11).ChargeContext(ctx, 500, 300)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ChargeContext() error = %v, want context deadline exceeded", err)
	}
}

func TestDischarge(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	err := client.Discharge(800, 300)
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("Discharge() error = %v", err)
	}

	if len(calledPaths) != 3 {
		t.Fatalf("expected 3 calls, got %d: %v", len(calledPaths), calledPaths)
	}

	// First call should enable RS485 control mode
	if !strings.Contains(calledPaths[0], "RS485%20Control%20Mode") || !strings.Contains(calledPaths[0], "option=enable") {
		t.Errorf("first call should enable RS485 control mode, got: %s", calledPaths[0])
	}

	// Second call should set discharge power
	if !strings.Contains(calledPaths[1], "Discharge%20Power") || !strings.Contains(calledPaths[1], "value=800") {
		t.Errorf("second call should set discharge power, got: %s", calledPaths[1])
	}

	// Third call should force mode to discharge
	if !strings.Contains(calledPaths[2], "option=discharge") {
		t.Errorf("third call should force mode to discharge, got: %s", calledPaths[2])
	}
}

func TestIdle(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	err := client.Idle()
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("Idle() error = %v", err)
	}

	if len(calledPaths) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(calledPaths), calledPaths)
	}

	if !strings.Contains(calledPaths[0], "option=enable") {
		t.Errorf("first call should enable RS485 control mode, got: %s", calledPaths[0])
	}
	if !strings.Contains(calledPaths[1], "option=stop") {
		t.Errorf("second call should force mode to stop, got: %s", calledPaths[1])
	}
	for _, path := range calledPaths {
		if strings.Contains(path, "option=disable") {
			t.Errorf("idle must leave RS485 control enabled: %v", calledPaths)
		}
	}
}

func TestIdle_StopFailureKeepsRS485Enabled(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "stop")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	err := client.Idle()
	calledPaths = *calledPathsPtr
	if err == nil {
		t.Fatal("Idle() error = nil, want error when stop fails")
	}

	if len(calledPaths) != 2 {
		t.Fatalf("expected 2 calls (enable + stop), got %d: %v", len(calledPaths), calledPaths)
	}

	if !strings.Contains(calledPaths[0], "option=enable") {
		t.Errorf("first call should be enable, got: %s", calledPaths[0])
	}
	if !strings.Contains(calledPaths[1], "option=stop") {
		t.Errorf("second call should be stop, got: %s", calledPaths[1])
	}
	for _, path := range calledPaths {
		if strings.Contains(path, "option=disable") {
			t.Errorf("RS485 must remain enabled when stop is unconfirmed: %v", calledPaths)
		}
	}

	// Error should identify the stop failure
	if !strings.Contains(err.Error(), "stop") {
		t.Errorf("error should mention stop, got: %v", err)
	}
}

func TestSetPassiveMode_Charge(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	// Negative power = charge
	err := client.SetPassiveMode(-2500, 300)
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("SetPassiveMode() error = %v", err)
	}

	if len(calledPaths) != 3 {
		t.Fatalf("expected 3 calls for charge, got %d", len(calledPaths))
	}
	if !strings.Contains(calledPaths[2], "option=charge") {
		t.Errorf("negative power should trigger charge mode, got: %v", calledPaths)
	}
}

func TestSetPassiveMode_Discharge(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	// Positive power = discharge
	err := client.SetPassiveMode(800, 300)
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("SetPassiveMode() error = %v", err)
	}

	if len(calledPaths) != 3 {
		t.Fatalf("expected 3 calls for discharge, got %d", len(calledPaths))
	}
	if !strings.Contains(calledPaths[2], "option=discharge") {
		t.Errorf("positive power should trigger discharge mode, got: %v", calledPaths)
	}
}

func TestSetPassiveMode_Idle(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	// Zero power = idle
	err := client.SetPassiveMode(0, 300)
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("SetPassiveMode() error = %v", err)
	}

	if len(calledPaths) != 2 {
		t.Fatalf("expected 2 calls for idle, got %d", len(calledPaths))
	}
	if !strings.Contains(calledPaths[0], "option=enable") {
		t.Errorf("zero power should enable RS485 control mode first, got: %s", calledPaths[0])
	}
	if !strings.Contains(calledPaths[1], "option=stop") {
		t.Errorf("zero power should trigger idle/stop mode, got: %s", calledPaths[1])
	}
	for _, path := range calledPaths {
		if strings.Contains(path, "option=disable") {
			t.Errorf("idle must leave RS485 control enabled: %v", calledPaths)
		}
	}
}

func TestGetESStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "State%20Of%20Charge") || strings.Contains(r.URL.Path, "State Of Charge"):
			w.Write([]byte(`{"id":"sensor-soc","value":80,"state":"80 %"}`))
		case strings.Contains(r.URL.Path, "Battery%20Power") || strings.Contains(r.URL.Path, "Battery Power"):
			w.Write([]byte(`{"id":"sensor-power","value":1500,"state":"1500 W"}`)) // positive = charging
		case strings.Contains(r.URL.Path, "Remaining%20Capacity") || strings.Contains(r.URL.Path, "Remaining Capacity"):
			w.Write([]byte(`{"id":"sensor-cap","value":4.1,"state":"4.1 kWh"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.URL, 11)
	status, err := client.GetESStatus(context.Background())
	if err != nil {
		t.Fatalf("GetESStatus() error = %v", err)
	}
	if status.BatterySOC != 80 {
		t.Errorf("BatterySOC = %d, want 80", status.BatterySOC)
	}
	if status.BatteryPower != 1500 {
		t.Errorf("BatteryPower = %v, want 1500", status.BatteryPower)
	}
	if status.BatteryCapacity != 0 {
		t.Errorf("BatteryCapacity = %v, want 0", status.BatteryCapacity)
	}
}

func TestGetESStatus_RequiresBatteryPower(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "State Of Charge") {
			w.Write([]byte(`{"value":80}`))
			return
		}
		http.Error(w, "power unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	_, err := client.GetESStatus(context.Background())
	if err == nil || !strings.Contains(err.Error(), "get battery power") {
		t.Fatalf("GetESStatus() error = %v, want battery power error", err)
	}
}

func TestGetESStatus_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	client := New(server.URL, 11)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.GetESStatus(ctx)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("GetESStatus() error = %v, want context cancellation", err)
	}
}

func TestGetBatteryStatusContext_CancellationDuringOptionalSensors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "State Of Charge"):
			_, _ = w.Write([]byte(`{"value":80}`))
		case strings.Contains(r.URL.Path, "Battery Power"):
			_, _ = w.Write([]byte(`{"value":500}`))
		default:
			<-r.Context().Done()
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := New(server.URL, 11).GetBatteryStatusContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetESStatus() error = %v, want context deadline exceeded", err)
	}
}

func TestClose(t *testing.T) {
	client := New("http://localhost", 11)
	err := client.Close()
	if err != nil {
		t.Errorf("Close() error = %v, want nil (no-op)", err)
	}
}

func TestHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := New(server.URL, 11)

	_, err := client.GetBatteryStatus()
	if err == nil {
		t.Error("GetBatteryStatus() error = nil, want error for 500 response")
	}

	err = client.Charge(2500, 300)
	if err == nil {
		t.Error("Charge() error = nil, want error for 500 response")
	}
}

// frozenLinkServer serves a bridge whose RS485 link is dead: control POSTs are
// acknowledged, the force-mode select never leaves "stop", and battery-sourced
// sensors are frozen at constants. sensors is mutated by the test to simulate a
// live link. postCount counts control writes.
func newFrozenLinkServer(t *testing.T, sensors map[string]float64, postCount *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	values := map[string]string{
		"/select/RS485 Control Mode":        "enable",
		"/select/Forcible Charge⁄Discharge": "stop",
		"/number/Forcible Charge Power":     "0",
		"/number/Forcible Discharge Power":  "0",
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			*postCount++
			entityPath := strings.TrimSuffix(r.URL.Path, "/set")
			// Numbers echo optimistically; the force-mode select never moves.
			if value := r.URL.Query().Get("value"); value != "" {
				values[entityPath] = value
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if value, ok := sensors[r.URL.Path]; ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"value": value})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value": values[r.URL.Path], "state": values[r.URL.Path]})
	}))
}

func frozenSensors() map[string]float64 {
	return map[string]float64{
		"/sensor/AC Voltage":                232.5,
		"/sensor/Battery Power":             -10,
		"/sensor/Internal Temperature":      32.9,
		"/sensor/Battery State Of Charge":   50,
		"/sensor/Battery Voltage (Average)": 51.2,
		"/sensor/Battery Current (Average)": -0.2,
	}
}

func TestControlFailureWithFrozenTelemetryReportsLinkDown(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	posts := 0
	server := newFrozenLinkServer(t, frozenSensors(), &posts, &mu)
	defer server.Close()

	client := New(server.URL, 11)
	client.probeWindow = 60 * time.Millisecond
	client.probeInterval = 20 * time.Millisecond

	err := client.ChargeContext(context.Background(), 500, 300)
	if !errors.Is(err, marstek.ErrLinkDown) {
		t.Fatalf("ChargeContext() error = %v, want ErrLinkDown", err)
	}
}

func TestControlFailureWithLiveTelemetryIsNotLinkDown(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	posts := 0
	sensors := frozenSensors()
	server := newFrozenLinkServer(t, sensors, &posts, &mu)
	defer server.Close()

	client := New(server.URL, 11)
	client.probeWindow = 60 * time.Millisecond
	client.probeInterval = 20 * time.Millisecond

	// Move a probed sensor while the probe is sampling.
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		value := 232.5
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				value += 0.1
				mu.Lock()
				sensors["/sensor/AC Voltage"] = value
				mu.Unlock()
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()

	err := client.ChargeContext(context.Background(), 500, 300)
	if err == nil {
		t.Fatal("ChargeContext() error = nil, want control failure")
	}
	if errors.Is(err, marstek.ErrLinkDown) {
		t.Fatalf("ChargeContext() error = %v, want plain control failure", err)
	}
}

func TestLinkDownVerdictFailsFastAndClearsOnTelemetryChange(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	posts := 0
	sensors := frozenSensors()
	server := newFrozenLinkServer(t, sensors, &posts, &mu)
	defer server.Close()

	client := New(server.URL, 11)
	client.probeWindow = 60 * time.Millisecond
	client.probeInterval = 20 * time.Millisecond

	// Seed telemetry so a later change is observable.
	if _, err := client.GetBatteryStatusContext(context.Background()); err != nil {
		t.Fatalf("GetBatteryStatusContext() error = %v", err)
	}

	client.markLinkDown()

	mu.Lock()
	posts = 0
	mu.Unlock()

	err := client.ChargeContext(context.Background(), 500, 300)
	if !errors.Is(err, marstek.ErrLinkDown) {
		t.Fatalf("ChargeContext() error = %v, want ErrLinkDown", err)
	}
	mu.Lock()
	postsAfter := posts
	mu.Unlock()
	if postsAfter != 0 {
		t.Fatalf("control writes while link down = %d, want 0", postsAfter)
	}

	// Telemetry moves: the verdict must clear and commands must be attempted again.
	mu.Lock()
	sensors["/sensor/Battery State Of Charge"] = 60
	mu.Unlock()
	if _, err := client.GetBatteryStatusContext(context.Background()); err != nil {
		t.Fatalf("GetBatteryStatusContext() error = %v", err)
	}
	if _, down := client.linkDown(); down {
		t.Fatal("link-down verdict still set after telemetry change")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = client.ChargeContext(ctx, 500, 300)
	mu.Lock()
	postsAfter = posts
	mu.Unlock()
	if postsAfter == 0 {
		t.Fatal("no control writes attempted after link recovered")
	}
}

func TestControlWriteRetryDelayFitsConfirmationWindow(t *testing.T) {
	if got := New("http://example.invalid", 11).writeRetryDelay; got != controlWriteRetryDelay {
		t.Fatalf("New() writeRetryDelay = %s, want %s", got, controlWriteRetryDelay)
	}
	if controlWriteRetryDelay != 4*time.Second {
		t.Fatalf("controlWriteRetryDelay = %s, want 4s", controlWriteRetryDelay)
	}
	// Retries land at 4 s and 12 s: both must fit inside the confirmation window.
	if controlWriteRetryDelay*3 >= controlConfirmationTimeout {
		t.Fatalf(
			"retry cadence %s does not fit confirmation timeout %s",
			controlWriteRetryDelay*3,
			controlConfirmationTimeout,
		)
	}
	// The last retry must leave ESPHome enough time to poll the value back.
	lastRetryAt := controlWriteRetryDelay + 2*controlWriteRetryDelay
	if controlConfirmationTimeout-lastRetryAt < 5*time.Second {
		t.Fatalf(
			"last retry at %s leaves only %s of the %s confirmation window, want >= 5s",
			lastRetryAt,
			controlConfirmationTimeout-lastRetryAt,
			controlConfirmationTimeout,
		)
	}
}

func TestCheckLink_TelemetryMovingIsHealthy(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	posts := 0
	sensors := frozenSensors()
	server := newFrozenLinkServer(t, sensors, &posts, &mu)
	defer server.Close()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	client := New(server.URL, 11)
	client.now = func() time.Time { return now }

	if err := client.CheckLink(context.Background()); err != nil {
		t.Fatalf("CheckLink() first call error = %v, want nil", err)
	}

	// Telemetry moves while the clock advances past the staleness threshold.
	now = now.Add(5 * time.Minute)
	mu.Lock()
	sensors["/sensor/Battery Power"] = -1200
	mu.Unlock()

	if err := client.CheckLink(context.Background()); err != nil {
		t.Fatalf("CheckLink() error = %v, want nil", err)
	}
}

func TestCheckLink_FrozenTelemetryReportsLinkDown(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	posts := 0
	server := newFrozenLinkServer(t, frozenSensors(), &posts, &mu)
	defer server.Close()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	client := New(server.URL, 11)
	client.now = func() time.Time { return now }

	// Seed the baseline, then let the clock run past the threshold with no change.
	if err := client.CheckLink(context.Background()); err != nil {
		t.Fatalf("CheckLink() seed error = %v, want nil", err)
	}
	now = now.Add(linkStaleThreshold)

	err := client.CheckLink(context.Background())
	if !errors.Is(err, marstek.ErrLinkDown) {
		t.Fatalf("CheckLink() error = %v, want ErrLinkDown", err)
	}

	// The verdict must be recorded, so control writes fail fast.
	mu.Lock()
	posts = 0
	mu.Unlock()
	if err := client.ensureRS485ControlMode(context.Background()); !errors.Is(err, marstek.ErrLinkDown) {
		t.Fatalf("ensureRS485ControlMode() error = %v, want ErrLinkDown", err)
	}
	mu.Lock()
	postsAfter := posts
	mu.Unlock()
	if postsAfter != 0 {
		t.Fatalf("control writes while link down = %d, want 0", postsAfter)
	}
}

func TestCheckLink_ReadFailureIsNotLinkDown(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	err := New(server.URL, 11).CheckLink(context.Background())
	if err == nil {
		t.Fatal("CheckLink() error = nil, want read error")
	}
	if errors.Is(err, marstek.ErrLinkDown) {
		t.Fatalf("CheckLink() error = %v, want plain read error", err)
	}
}

func TestCheckLink_NoHistoryIsHealthy(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	posts := 0
	server := newFrozenLinkServer(t, frozenSensors(), &posts, &mu)
	defer server.Close()

	if err := New(server.URL, 11).CheckLink(context.Background()); err != nil {
		t.Fatalf("CheckLink() error = %v, want nil", err)
	}
}

// newRefreshTestServer serves the control entities from the given values and
// records every POST so tests can assert that no Modbus write was issued.
func newRefreshTestServer(t *testing.T, values map[string]string) (*httptest.Server, *[]string) {
	t.Helper()
	posts := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts = append(posts, r.URL.String())
			entityPath := strings.TrimSuffix(r.URL.Path, "/set")
			if option := r.URL.Query().Get("option"); option != "" {
				values[entityPath] = option
			}
			if value := r.URL.Query().Get("value"); value != "" {
				values[entityPath] = value
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"value": values[r.URL.Path], "state": values[r.URL.Path]})
	}))
	return server, &posts
}

func TestRefreshPassiveModeContext(t *testing.T) {
	tests := []struct {
		name     string
		values   map[string]string
		power    int
		wantPost bool
	}{
		{
			name: "battery still reports discharge at the same power",
			values: map[string]string{
				"/select/RS485 Control Mode":        "enable",
				"/select/Forcible Charge⁄Discharge": "discharge",
				"/number/Forcible Discharge Power":  "2200",
				"/number/Forcible Charge Power":     "0",
			},
			power:    2200,
			wantPost: false,
		},
		{
			name: "battery dropped back to stop",
			values: map[string]string{
				"/select/RS485 Control Mode":        "enable",
				"/select/Forcible Charge⁄Discharge": "stop",
				"/number/Forcible Discharge Power":  "2200",
				"/number/Forcible Charge Power":     "0",
			},
			power:    2200,
			wantPost: true,
		},
		{
			name: "idle already stopped",
			values: map[string]string{
				"/select/RS485 Control Mode":        "enable",
				"/select/Forcible Charge⁄Discharge": "stop",
				"/number/Forcible Discharge Power":  "0",
				"/number/Forcible Charge Power":     "0",
			},
			power:    0,
			wantPost: false,
		},
		{
			name: "mode matches but power drifted",
			values: map[string]string{
				"/select/RS485 Control Mode":        "enable",
				"/select/Forcible Charge⁄Discharge": "discharge",
				"/number/Forcible Discharge Power":  "800",
				"/number/Forcible Charge Power":     "0",
			},
			power:    2200,
			wantPost: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, posts := newRefreshTestServer(t, tt.values)
			defer server.Close()

			client := New(server.URL, 11)
			client.writeRetryDelay = 20 * time.Millisecond
			if err := client.RefreshPassiveModeContext(context.Background(), tt.power, 300); err != nil {
				t.Fatalf("RefreshPassiveModeContext() error = %v", err)
			}
			if tt.wantPost && len(*posts) == 0 {
				t.Fatal("no control writes issued, want re-assert")
			}
			if !tt.wantPost && len(*posts) != 0 {
				t.Fatalf("control writes = %v, want none", *posts)
			}
			if !tt.wantPost {
				return
			}
			var sawSelect, sawNumber bool
			for _, post := range *posts {
				if strings.Contains(post, "Forcible%20Charge%E2%81%84Discharge") {
					sawSelect = true
				}
				if strings.Contains(post, "Forcible%20Discharge%20Power") {
					sawNumber = true
				}
			}
			if !sawSelect || !sawNumber {
				t.Fatalf("posts = %v, want both discharge power and force-mode writes", *posts)
			}
		})
	}
}

func TestRestartDevice(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		client := New("http://example.invalid", 11)
		if client.RestartAvailable() {
			t.Fatal("RestartAvailable() = true, want false")
		}
		if err := client.RestartDevice(context.Background()); err == nil {
			t.Fatal("RestartDevice() error = nil, want error")
		}
	})

	t.Run("presses the button", func(t *testing.T) {
		var got string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Method + " " + r.URL.Path
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := New(server.URL, 11)
		client.SetRestartButton("restart")
		if !client.RestartAvailable() {
			t.Fatal("RestartAvailable() = false, want true")
		}
		if err := client.RestartDevice(context.Background()); err != nil {
			t.Fatalf("RestartDevice() error = %v", err)
		}
		if want := "POST /button/restart/press"; got != want {
			t.Fatalf("request = %q, want %q", got, want)
		}
	})

	t.Run("non-2xx is an error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusInternalServerError)
		}))
		defer server.Close()

		client := New(server.URL, 11)
		client.SetRestartButton("restart")
		if err := client.RestartDevice(context.Background()); err == nil {
			t.Fatal("RestartDevice() error = nil, want error")
		}
	})
}

func TestCheckLink_SamplesFastMovingSensors(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	seen := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"value": 1.0})
	}))
	defer server.Close()

	if err := New(server.URL, 11).CheckLink(context.Background()); err != nil {
		t.Fatalf("CheckLink() error = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"/sensor/AC Voltage",
		"/sensor/Battery Power",
		"/sensor/Battery Voltage (Average)",
		"/sensor/Battery Current (Average)",
	}
	for _, path := range want {
		if seen[path] != 1 {
			t.Errorf("reads of %q = %d, want 1 (seen: %v)", path, seen[path], seen)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("sampled paths = %v, want exactly %v", seen, want)
	}
}

func TestRestartDevice_ReseedsLinkBaseline(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	posts := 0
	server := newFrozenLinkServer(t, frozenSensors(), &posts, &mu)
	defer server.Close()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	client := New(server.URL, 11)
	client.now = func() time.Time { return now }
	client.SetRestartButton("restart")

	if err := client.CheckLink(context.Background()); err != nil {
		t.Fatalf("CheckLink() seed error = %v, want nil", err)
	}
	now = now.Add(linkStaleThreshold)
	if err := client.CheckLink(context.Background()); !errors.Is(err, marstek.ErrLinkDown) {
		t.Fatalf("CheckLink() error = %v, want ErrLinkDown", err)
	}

	if err := client.RestartDevice(context.Background()); err != nil {
		t.Fatalf("RestartDevice() error = %v", err)
	}

	// Post-reboot telemetry is a fresh baseline and the verdict is dropped.
	if err := client.CheckLink(context.Background()); err != nil {
		t.Fatalf("CheckLink() after restart error = %v, want nil", err)
	}
	if err := client.ensureRS485ControlMode(context.Background()); err != nil {
		t.Fatalf("ensureRS485ControlMode() after restart error = %v, want nil", err)
	}
}

func TestRefreshPassiveModeContext_LinkDownFailsFast(t *testing.T) {
	t.Parallel()
	var requests int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	client.markLinkDown()

	err := client.RefreshPassiveModeContext(context.Background(), -2500, 300)
	if !errors.Is(err, marstek.ErrLinkDown) {
		t.Fatalf("RefreshPassiveModeContext() error = %v, want ErrLinkDown", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Fatalf("HTTP requests while link down = %d, want 0", requests)
	}
}

func TestRefreshPassiveModeContext_LostControlModeReasserts(t *testing.T) {
	t.Parallel()
	// Forcible mode still matches, but the battery dropped RS485 control mode:
	// everything must be re-asserted.
	values := map[string]string{
		"/select/RS485 Control Mode":        "disable",
		"/select/Forcible Charge⁄Discharge": "discharge",
		"/number/Forcible Discharge Power":  "2200",
		"/number/Forcible Charge Power":     "0",
	}
	server, posts := newRefreshTestServer(t, values)
	defer server.Close()

	client := New(server.URL, 11)
	client.writeRetryDelay = 20 * time.Millisecond
	if err := client.RefreshPassiveModeContext(context.Background(), 2200, 300); err != nil {
		t.Fatalf("RefreshPassiveModeContext() error = %v", err)
	}

	var sawControlMode, sawSelect, sawNumber bool
	for _, post := range *posts {
		if strings.Contains(post, "RS485%20Control%20Mode") {
			sawControlMode = true
		}
		if strings.Contains(post, "Forcible%20Charge%E2%81%84Discharge") {
			sawSelect = true
		}
		if strings.Contains(post, "Forcible%20Discharge%20Power") {
			sawNumber = true
		}
	}
	if !sawControlMode || !sawSelect || !sawNumber {
		t.Fatalf("posts = %v, want control mode, discharge power and force-mode writes", *posts)
	}
}
