package homewizard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetActivePowerW_Import(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/data" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"active_power_w": 450.0}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := New(server.URL)
	power, err := client.GetActivePowerW()
	if err != nil {
		t.Fatalf("GetActivePowerW() error = %v", err)
	}
	if power != 450.0 {
		t.Errorf("GetActivePowerW() = %v, want 450.0 (importing)", power)
	}
}

func TestGetActivePowerW_Export(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/data" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"active_power_w": -800.5}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := New(server.URL)
	power, err := client.GetActivePowerW()
	if err != nil {
		t.Fatalf("GetActivePowerW() error = %v", err)
	}
	if power != -800.5 {
		t.Errorf("GetActivePowerW() = %v, want -800.5 (exporting)", power)
	}
}

func TestGetActivePowerW_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := New(server.URL)
	_, err := client.GetActivePowerW()
	if err == nil {
		t.Error("GetActivePowerW() error = nil, want error for 500 response")
	}
}

func TestGetActivePowerW_Disabled(t *testing.T) {
	client := New("")
	if client.Enabled() {
		t.Error("Enabled() = true, want false for empty URL")
	}
	_, err := client.GetActivePowerW()
	if err == nil {
		t.Error("GetActivePowerW() error = nil, want error when disabled")
	}
}

func TestGetDeviceInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"product_name":"P1 Meter","product_type":"HWE-P1","serial":"abc123","firmware_version":"4.19","api_version":"v1"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := New(server.URL)
	info, err := client.GetDeviceInfo()
	if err != nil {
		t.Fatalf("GetDeviceInfo() error = %v", err)
	}
	if info.ProductName != "P1 Meter" {
		t.Errorf("ProductName = %q, want %q", info.ProductName, "P1 Meter")
	}
	if info.Serial != "abc123" {
		t.Errorf("Serial = %q, want %q", info.Serial, "abc123")
	}
}

func TestEnabled(t *testing.T) {
	c1 := New("http://192.168.1.100")
	if !c1.Enabled() {
		t.Error("Enabled() = false, want true for non-empty URL")
	}

	c2 := New("")
	if c2.Enabled() {
		t.Error("Enabled() = true, want false for empty URL")
	}
}

func TestGetDeviceInfo_Disabled(t *testing.T) {
	client := New("")
	_, err := client.GetDeviceInfo()
	if err == nil {
		t.Error("GetDeviceInfo() error = nil, want error when disabled")
	}
}

func TestGetDeviceInfo_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := New(server.URL)
	_, err := client.GetDeviceInfo()
	if err == nil {
		t.Error("GetDeviceInfo() error = nil, want error for 500 response")
	}
}

func TestGetActivePowerW_MalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid json`))
	}))
	defer server.Close()

	client := New(server.URL)
	_, err := client.GetActivePowerW()
	if err == nil {
		t.Error("GetActivePowerW() error = nil, want error for malformed JSON")
	}
}

func TestGetDeviceInfo_MalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not json`))
	}))
	defer server.Close()

	client := New(server.URL)
	_, err := client.GetDeviceInfo()
	if err == nil {
		t.Error("GetDeviceInfo() error = nil, want error for malformed JSON")
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	c := New("http://192.168.1.100/")
	if c.baseURL != "http://192.168.1.100" {
		t.Errorf("baseURL = %q, want trailing slash trimmed", c.baseURL)
	}

	c2 := New("http://192.168.1.100///")
	if c2.baseURL != "http://192.168.1.100" {
		t.Errorf("baseURL = %q, want multiple trailing slashes trimmed", c2.baseURL)
	}
}

func TestGetActivePowerW_ZeroPower(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"active_power_w": 0}`))
	}))
	defer server.Close()

	client := New(server.URL)
	power, err := client.GetActivePowerW()
	if err != nil {
		t.Fatalf("GetActivePowerW() error = %v", err)
	}
	if power != 0 {
		t.Errorf("GetActivePowerW() = %v, want 0", power)
	}
}

func TestGetActivePowerW_ConnectionRefused(t *testing.T) {
	client := New("http://127.0.0.1:1")
	_, err := client.GetActivePowerW()
	if err == nil {
		t.Error("GetActivePowerW() error = nil, want error for connection refused")
	}
}
