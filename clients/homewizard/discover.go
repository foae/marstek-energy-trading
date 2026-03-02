package homewizard

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

const (
	mdnsService = "_hwenergy._tcp"
	mdnsDomain  = "local."
	mdnsTimeout = 3 * time.Second

	// HTTP scan settings
	scanConnectTimeout = 500 * time.Millisecond
	scanReadTimeout    = 2 * time.Second
	scanWorkers        = 64
)

// subnets to scan when mDNS fails (typical home networks).
var scanSubnets = []string{"192.168.1", "192.168.0"}

// DiscoveryResult contains the details of a discovered HomeWizard device.
type DiscoveryResult struct {
	URL      string
	Serial   string
	Hostname string
	Method   string // "mdns" or "http_scan"
}

// Discover attempts to find a HomeWizard P1 meter on the local network.
// It tries mDNS first, then falls back to an HTTP scan of common subnets.
func Discover(ctx context.Context) (*DiscoveryResult, error) {
	// Try mDNS first
	result, err := discoverMDNS(ctx)
	if err == nil {
		return result, nil
	}
	slog.Info("mDNS discovery failed, falling back to HTTP scan", "mdns_error", err)

	// Fall back to HTTP scan
	result, err = discoverHTTPScan(ctx)
	if err != nil {
		return nil, fmt.Errorf("all discovery methods failed: %w", err)
	}
	return result, nil
}

// discoverMDNS browses the local network for a HomeWizard P1 meter via mDNS.
func discoverMDNS(ctx context.Context) (*DiscoveryResult, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("create mDNS resolver: %w", err)
	}

	entries := make(chan *zeroconf.ServiceEntry)
	result := make(chan *DiscoveryResult, 1)

	ctx, cancel := context.WithTimeout(ctx, mdnsTimeout)
	defer cancel()

	go func() {
		for entry := range entries {
			txt := parseTXTRecords(entry.Text)
			l := slog.With(
				"hostname", entry.HostName,
				"addr", entry.AddrIPv4,
				"port", entry.Port,
				"product_type", txt["product_type"],
				"api_enabled", txt["api_enabled"],
				"serial", txt["serial"],
			)

			if txt["product_type"] != "HWE-P1" {
				l.Debug("skipping non-P1 HomeWizard device")
				continue
			}
			if txt["api_enabled"] != "1" {
				l.Debug("skipping P1 device with API disabled")
				continue
			}

			var host string
			if len(entry.AddrIPv4) > 0 {
				host = entry.AddrIPv4[0].String()
			} else if len(entry.AddrIPv6) > 0 {
				host = "[" + entry.AddrIPv6[0].String() + "]"
			} else {
				l.Debug("skipping P1 device with no addresses")
				continue
			}

			select {
			case result <- &DiscoveryResult{
				URL:      fmt.Sprintf("http://%s:%d", host, entry.Port),
				Serial:   txt["serial"],
				Hostname: entry.HostName,
				Method:   "mdns",
			}:
				cancel()
			default:
			}
			return
		}
	}()

	if err := resolver.Browse(ctx, mdnsService, mdnsDomain, entries); err != nil {
		return nil, fmt.Errorf("mDNS browse: %w", err)
	}

	<-ctx.Done()

	select {
	case r := <-result:
		return r, nil
	default:
		return nil, fmt.Errorf("no HomeWizard P1 meter found via mDNS (timeout %s)", mdnsTimeout)
	}
}

// discoverHTTPScan scans common LAN subnets for a HomeWizard P1 meter by
// probing GET /api on each IP. It uses a worker pool for concurrency.
func discoverHTTPScan(ctx context.Context) (*DiscoveryResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: scanConnectTimeout,
			}).DialContext,
			DisableKeepAlives: true,
		},
		Timeout: scanReadTimeout,
	}

	// Collect all IPs to scan
	var ips []string
	for _, subnet := range scanSubnets {
		for i := 1; i < 255; i++ {
			ips = append(ips, fmt.Sprintf("%s.%d", subnet, i))
		}
	}

	slog.Info("starting HTTP scan for HomeWizard P1", "subnets", scanSubnets, "total_ips", len(ips))

	// Fan out to workers
	ipCh := make(chan string, len(ips))
	for _, ip := range ips {
		ipCh <- ip
	}
	close(ipCh)

	resultCh := make(chan *DiscoveryResult, 1)
	var wg sync.WaitGroup

	for range scanWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range ipCh {
				select {
				case <-ctx.Done():
					return
				default:
				}

				if r := probeP1(ctx, client, ip); r != nil {
					select {
					case resultCh <- r:
						cancel() // stop other workers
					default:
					}
					return
				}
			}
		}()
	}

	// Wait for all workers in the background, then close resultCh
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	if r, ok := <-resultCh; ok {
		return r, nil
	}
	return nil, fmt.Errorf("no HomeWizard P1 meter found via HTTP scan on %v", scanSubnets)
}

// probeP1 checks a single IP for a HomeWizard P1 device by calling GET /api.
func probeP1(ctx context.Context, client *http.Client, ip string) *DiscoveryResult {
	url := "http://" + ip + "/api"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var info DeviceInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil
	}

	if info.ProductType != "HWE-P1" {
		slog.Debug("found non-P1 HomeWizard device", "ip", ip, "product_type", info.ProductType)
		return nil
	}

	slog.Info("found HomeWizard P1 via HTTP scan", "ip", ip, "serial", info.Serial, "product", info.ProductName)
	return &DiscoveryResult{
		URL:      "http://" + ip,
		Serial:   info.Serial,
		Hostname: info.ProductName,
		Method:   "http_scan",
	}
}

// parseTXTRecords converts mDNS TXT record entries (key=value) into a map.
func parseTXTRecords(records []string) map[string]string {
	m := make(map[string]string, len(records))
	for _, r := range records {
		k, v, ok := strings.Cut(r, "=")
		if ok {
			m[k] = v
		}
	}
	return m
}
