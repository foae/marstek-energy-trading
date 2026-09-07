package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunReturnsFailureForInvalidConfig(t *testing.T) {
	t.Setenv("ESPHOME_URL", "")
	if got := run(); got != 1 {
		t.Fatalf("run() exit code = %d, want 1", got)
	}
}

func TestEmergencyESPHomeURLExtractsSafeEndpointFromMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "BATTERY_MIN_SOC=0.30\nESPHOME_URL=http://battery-bridge.local\nBROKEN=\"unterminated\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write dotenv fixture: %v", err)
	}
	if got := emergencyESPHomeURL(path); got != "http://battery-bridge.local" {
		t.Fatalf("emergencyESPHomeURL() = %q, want safe battery endpoint", got)
	}
}

func TestEmergencyESPHomeURLRejectsCredentialBearingEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "ESPHOME_URL=http://user:password@battery-bridge.local\nBROKEN=\"unterminated\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write dotenv fixture: %v", err)
	}
	if got := emergencyESPHomeURL(path); got != "" {
		t.Fatalf("emergencyESPHomeURL() = %q, want rejected endpoint", got)
	}
}

func TestEmergencyESPHomeURLSupportsQuotedValueWithComment(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "ESPHOME_URL=\"http://battery-bridge.local\" # bridge\nBROKEN=\"unterminated\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write dotenv fixture: %v", err)
	}
	if got := emergencyESPHomeURL(path); got != "http://battery-bridge.local" {
		t.Fatalf("emergencyESPHomeURL() = %q, want quoted battery endpoint", got)
	}
}

func TestEmergencyESPHomeURLDoesNotRetainSupersededEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "ESPHOME_URL=http://old-device.local\nESPHOME_URL=\nBROKEN=\"unterminated\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write dotenv fixture: %v", err)
	}
	if got := emergencyESPHomeURL(path); got != "" {
		t.Fatalf("emergencyESPHomeURL() = %q, want superseded endpoint cleared", got)
	}
}
