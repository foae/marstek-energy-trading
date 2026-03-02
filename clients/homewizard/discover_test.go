package homewizard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
)

func TestParseTXTRecords(t *testing.T) {
	tests := []struct {
		name    string
		records []string
		want    map[string]string
	}{
		{
			name:    "typical P1 records",
			records: []string{"product_type=HWE-P1", "api_enabled=1", "serial=abc123"},
			want:    map[string]string{"product_type": "HWE-P1", "api_enabled": "1", "serial": "abc123"},
		},
		{
			name:    "empty value",
			records: []string{"key="},
			want:    map[string]string{"key": ""},
		},
		{
			name:    "no equals sign skipped",
			records: []string{"noequals"},
			want:    map[string]string{},
		},
		{
			name:    "value with equals sign",
			records: []string{"path=/api/v1?foo=bar"},
			want:    map[string]string{"path": "/api/v1?foo=bar"},
		},
		{
			name:    "nil input",
			records: nil,
			want:    map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTXTRecords(tt.records)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d entries, want %d: %v", len(got), len(tt.want), got)
			}
			for k, wantV := range tt.want {
				if gotV, ok := got[k]; !ok {
					t.Errorf("missing key %q", k)
				} else if gotV != wantV {
					t.Errorf("key %q: got %q, want %q", k, gotV, wantV)
				}
			}
		})
	}
}

// --- mDNS tests: call discoverMDNS directly to avoid HTTP scan fallback ---

func TestDiscoverMDNS_FindsP1Meter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mDNS integration test in short mode")
	}

	server, err := zeroconf.Register(
		"test-p1",
		"_hwenergy._tcp",
		"local.",
		8080,
		[]string{"product_type=HWE-P1", "api_enabled=1", "serial=fakep1serial"},
		nil,
	)
	if err != nil {
		t.Fatalf("register fake mDNS service: %v", err)
	}
	defer server.Shutdown()

	time.Sleep(200 * time.Millisecond)

	result, err := discoverMDNS(context.Background())
	if err != nil {
		t.Fatalf("discoverMDNS() error: %v", err)
	}

	if result.Serial != "fakep1serial" {
		t.Errorf("serial: got %q, want %q", result.Serial, "fakep1serial")
	}
	if result.URL == "" {
		t.Error("URL is empty")
	}
	if result.Method != "mdns" {
		t.Errorf("method: got %q, want %q", result.Method, "mdns")
	}
}

func TestDiscoverMDNS_SkipsNonP1Device(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mDNS integration test in short mode")
	}

	server, err := zeroconf.Register(
		"test-kwh",
		"_hwenergy._tcp",
		"local.",
		8080,
		[]string{"product_type=HWE-KWH1", "api_enabled=1", "serial=kwhserial"},
		nil,
	)
	if err != nil {
		t.Fatalf("register fake mDNS service: %v", err)
	}
	defer server.Shutdown()

	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = discoverMDNS(ctx)
	if err == nil {
		t.Fatal("expected error when no P1 device is present, got nil")
	}
}

func TestDiscoverMDNS_SkipsAPIDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mDNS integration test in short mode")
	}

	server, err := zeroconf.Register(
		"test-p1-noapi",
		"_hwenergy._tcp",
		"local.",
		8080,
		[]string{"product_type=HWE-P1", "api_enabled=0", "serial=noapiserial"},
		nil,
	)
	if err != nil {
		t.Fatalf("register fake mDNS service: %v", err)
	}
	defer server.Shutdown()

	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = discoverMDNS(ctx)
	if err == nil {
		t.Fatal("expected error when P1 has API disabled, got nil")
	}
}

func TestDiscoverMDNS_TimeoutWhenNoServices(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mDNS integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := discoverMDNS(ctx)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

// --- probeP1 tests: use httptest to simulate HomeWizard responses ---

func TestProbeP1_FindsP1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(DeviceInfo{
			ProductName: "P1 Meter",
			ProductType: "HWE-P1",
			Serial:      "testp1serial",
			Firmware:    "4.19",
			APIVersion:  "v1",
		})
	}))
	defer srv.Close()

	addr := srv.URL[len("http://"):]
	result := probeP1(context.Background(), srv.Client(), addr)
	if result == nil {
		t.Fatal("expected non-nil result for P1 device")
	}
	if result.Serial != "testp1serial" {
		t.Errorf("serial: got %q, want %q", result.Serial, "testp1serial")
	}
	if result.Hostname != "P1 Meter" {
		t.Errorf("hostname: got %q, want %q", result.Hostname, "P1 Meter")
	}
	if result.Method != "http_scan" {
		t.Errorf("method: got %q, want %q", result.Method, "http_scan")
	}
	if want := "http://" + addr; result.URL != want {
		t.Errorf("url: got %q, want %q", result.URL, want)
	}
}

func TestProbeP1_SkipsNonP1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(DeviceInfo{
			ProductType: "HWE-KWH1",
			Serial:      "kwhserial",
		})
	}))
	defer srv.Close()

	addr := srv.URL[len("http://"):]
	result := probeP1(context.Background(), srv.Client(), addr)
	if result != nil {
		t.Fatalf("expected nil for non-P1 device, got %+v", result)
	}
}

func TestProbeP1_HandlesNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	addr := srv.URL[len("http://"):]
	result := probeP1(context.Background(), srv.Client(), addr)
	if result != nil {
		t.Fatalf("expected nil for 404 response, got %+v", result)
	}
}

func TestProbeP1_HandlesInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	addr := srv.URL[len("http://"):]
	result := probeP1(context.Background(), srv.Client(), addr)
	if result != nil {
		t.Fatalf("expected nil for invalid JSON, got %+v", result)
	}
}

func TestProbeP1_HandlesConnectionRefused(t *testing.T) {
	// Create and immediately close a listener to get a port that refuses connections.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL[len("http://"):]
	srv.Close()

	client := &http.Client{Timeout: 500 * time.Millisecond}
	result := probeP1(context.Background(), client, addr)
	if result != nil {
		t.Fatalf("expected nil for connection refused, got %+v", result)
	}
}

func TestProbeP1_RespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	addr := srv.URL[len("http://"):]
	result := probeP1(ctx, srv.Client(), addr)
	if result != nil {
		t.Fatalf("expected nil for cancelled context, got %+v", result)
	}
}

// --- Discover integration: verify mDNS is preferred over HTTP scan ---

func TestDiscover_PrefersMDNS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mDNS integration test in short mode")
	}

	server, err := zeroconf.Register(
		"test-p1-prefer",
		"_hwenergy._tcp",
		"local.",
		8080,
		[]string{"product_type=HWE-P1", "api_enabled=1", "serial=mdnsp1serial"},
		nil,
	)
	if err != nil {
		t.Fatalf("register fake mDNS service: %v", err)
	}
	defer server.Shutdown()

	time.Sleep(200 * time.Millisecond)

	result, err := Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}

	if result.Method != "mdns" {
		t.Errorf("method: got %q, want %q (mDNS should be preferred)", result.Method, "mdns")
	}
	if result.Serial != "mdnsp1serial" {
		t.Errorf("serial: got %q, want %q", result.Serial, "mdnsp1serial")
	}
}
