package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Config holds all configuration from environment variables
type Config struct {
	TraefikAPIURL  string
	PiHoleURL      string
	PiHolePassword string
	TraefikHostIP  string
	SyncInterval   string
	LogLevel       string
	DryRun         bool
}

// TraefikRouter represents a Traefik HTTP router from the API
type TraefikRouter struct {
	EntryPoints []string               `json:"entryPoints"`
	Service     string                 `json:"service"`
	Rule        string                 `json:"rule"`
	Status      string                 `json:"status"`
	Using       []string               `json:"using"`
	Name        string                 `json:"name"`
	Provider    string                 `json:"provider"`
	TLS         map[string]interface{} `json:"tls,omitempty"`
}

// PiHoleV6ConfigResponse represents the response from Pi-hole v6's /api/config/dns endpoint
type PiHoleV6ConfigResponse struct {
	Config struct {
		DNS struct {
			Hosts []string `json:"hosts"`
		} `json:"dns"`
	} `json:"config"`
}

func main() {
	// Parse command-line flags
	once := flag.Bool("once", false, "Run sync once and exit")
	dryRun := flag.Bool("dry-run", false, "Show what would be synced without making changes")
	flag.Parse()

	// Load configuration
	config := loadConfig()
	config.DryRun = *dryRun

	if config.DryRun {
		log.Println("🔍 Running in DRY-RUN mode - no changes will be made")
	}

	// Run sync immediately
	log.Println("Starting Traefik to Pi-hole DNS sync...")
	if err := syncDNS(config); err != nil {
		log.Printf("❌ Sync failed: %v", err)
	}

	// If --once flag is set, exit after first sync
	if *once {
		log.Println("✅ One-time sync completed")
		return
	}

	// Otherwise, set up cron job
	c := cron.New()
	_, err := c.AddFunc(config.SyncInterval, func() {
		log.Println("Running scheduled sync...")
		if err := syncDNS(config); err != nil {
			log.Printf("❌ Sync failed: %v", err)
		}
	})
	if err != nil {
		log.Fatalf("Failed to schedule cron job: %v", err)
	}

	c.Start()
	log.Printf("📅 Scheduled sync with interval: %s", config.SyncInterval)

	// Keep the program running
	select {}
}

func loadConfig() Config {
	config := Config{
		TraefikAPIURL:  getEnv("TRAEFIK_API_URL", "http://traefik:8080/api/http/routers"),
		PiHoleURL:      os.Getenv("PIHOLE_URL"),
		PiHolePassword: os.Getenv("PIHOLE_PASSWORD"),
		TraefikHostIP:  os.Getenv("TRAEFIK_HOST_IP"),
		SyncInterval:   getEnv("SYNC_INTERVAL", "@every 5m"),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
	}

	// Validate required configuration
	if config.PiHoleURL == "" {
		log.Fatal("❌ PIHOLE_URL environment variable is required")
	}
	if config.PiHolePassword == "" {
		log.Fatal("❌ PIHOLE_PASSWORD environment variable is required")
	}
	if config.TraefikHostIP == "" {
		log.Fatal("❌ TRAEFIK_HOST_IP environment variable is required")
	}

	return config
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func syncDNS(config Config) error {
	// Authenticate once for this entire sync iteration
	sid, err := authenticatePiHoleV6(config)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// 1. Get all routers from Traefik
	routers, err := getTraefikRouters(config.TraefikAPIURL)
	if err != nil {
		return fmt.Errorf("failed to get Traefik routers: %w", err)
	}

	log.Printf("📡 Found %d routers in Traefik", len(routers))

	// 2. Extract hostnames from routers
	hostnames := extractHostnames(routers)
	log.Printf("🌐 Extracted %d unique hostnames", len(hostnames))

	if len(hostnames) == 0 {
		log.Println("⚠️  No hostnames found to sync")
		return nil
	}

	// 3. Get existing DNS records from Pi-hole (reuse SID)
	existingRecords, err := getPiHoleDNSRecords(config, sid)
	if err != nil {
		return fmt.Errorf("failed to get Pi-hole DNS records: %w", err)
	}

	log.Printf("📋 Found %d existing DNS records in Pi-hole", len(existingRecords))

	// 4. Sync: Add missing records (reuse SID)
	added := 0
	for _, hostname := range hostnames {
		if _, exists := existingRecords[hostname]; !exists {
			if config.DryRun {
				log.Printf("  [DRY-RUN] Would add: %s -> %s", hostname, config.TraefikHostIP)
			} else {
				if err := addPiHoleDNSRecord(config, sid, hostname, config.TraefikHostIP); err != nil {
					log.Printf("  ⚠️  Failed to add %s: %v", hostname, err)
				} else {
					log.Printf("  ✅ Added: %s -> %s", hostname, config.TraefikHostIP)
					added++
				}
			}
		} else {
			if config.LogLevel == "debug" {
				log.Printf("  ✓ Already exists: %s", hostname)
			}
		}
	}

	if config.DryRun {
		log.Printf("🔍 DRY-RUN: Would have added %d new DNS records", countMissing(hostnames, existingRecords))
	} else {
		log.Printf("✅ Sync completed: %d records added", added)
	}

	return nil
}

func getTraefikRouters(apiURL string) (map[string]TraefikRouter, error) {
	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("traefik API returned %d: %s", resp.StatusCode, string(body))
	}

	var routers map[string]TraefikRouter
	if err := json.NewDecoder(resp.Body).Decode(&routers); err != nil {
		return nil, err
	}

	return routers, nil
}

func extractHostnames(routers map[string]TraefikRouter) []string {
	// Regex to match Host(`hostname`) or Host(`hostname1`,`hostname2`)
	hostRegex := regexp.MustCompile(`Host\(\x60([^\x60]+)\x60\)`)

	hostnameSet := make(map[string]bool)

	for _, router := range routers {
		// Skip disabled routers
		if router.Status != "enabled" {
			continue
		}

		// Extract all Host() matches from the rule
		matches := hostRegex.FindAllStringSubmatch(router.Rule, -1)
		for _, match := range matches {
			if len(match) > 1 {
				// Handle multiple hostnames separated by commas
				hosts := strings.Split(match[1], ",")
				for _, host := range hosts {
					hostname := strings.TrimSpace(host)
					hostname = strings.Trim(hostname, "`")
					if hostname != "" {
						hostnameSet[hostname] = true
					}
				}
			}
		}
	}

	// Convert set to slice
	hostnames := make([]string, 0, len(hostnameSet))
	for hostname := range hostnameSet {
		hostnames = append(hostnames, hostname)
	}

	return hostnames
}

func getPiHoleDNSRecords(config Config, sid string) (map[string]string, error) {
	// Get DNS config using Pi-hole v6 API with provided session ID
	apiURL := fmt.Sprintf("%s/api/config/dns", config.PiHoleURL)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	// Add session ID header
	req.Header.Set("sid", sid)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pi-hole API returned %d: %s", resp.StatusCode, string(body))
	}

	var dnsResp PiHoleV6ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&dnsResp); err != nil {
		return nil, err
	}

	// Build map of hostname -> IP from hosts array
	// Each host entry is formatted as "IP HOSTNAME"
	records := make(map[string]string)
	for _, host := range dnsResp.Config.DNS.Hosts {
		parts := strings.Fields(host) // Split by whitespace
		if len(parts) >= 2 {
			ip := parts[0]
			hostname := parts[1]
			records[hostname] = ip
		}
	}

	return records, nil
}

// authenticatePiHoleV6 authenticates with Pi-hole v6 and returns a session ID
func authenticatePiHoleV6(config Config) (string, error) {
	// Pi-hole v6 uses /api/auth endpoint
	authURL := fmt.Sprintf("%s/api/auth", config.PiHoleURL)

	data := url.Values{}
	data.Set("password", config.PiHolePassword)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(authURL, data)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("authentication failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response to get session ID
	var authResp struct {
		Session struct {
			SID      string `json:"sid"`
			Validity int    `json:"validity"`
		} `json:"session"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return "", fmt.Errorf("failed to parse auth response: %w", err)
	}

	if authResp.Session.SID == "" {
		return "", fmt.Errorf("no session ID received from Pi-hole")
	}

	return authResp.Session.SID, nil
}

func addPiHoleDNSRecord(config Config, sid string, hostname, ip string) error {
	// Add DNS record using Pi-hole v6 API with provided session ID

	// First, get existing hosts (reuse SID)
	existingHosts, err := getExistingHosts(config, sid)
	if err != nil {
		return fmt.Errorf("failed to get existing hosts: %w", err)
	}

	// Add new host entry in "IP HOSTNAME" format
	newHost := fmt.Sprintf("%s %s", ip, hostname)
	existingHosts = append(existingHosts, newHost)

	// Update DNS config using Pi-hole v6 API
	apiURL := fmt.Sprintf("%s/api/config/dns", config.PiHoleURL)

	// Create JSON payload with updated hosts array
	payload := map[string]interface{}{
		"dns": map[string]interface{}{
			"hosts": existingHosts,
		},
	}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("PUT", apiURL, strings.NewReader(string(jsonData)))
	if err != nil {
		return err
	}

	// Add headers
	req.Header.Set("sid", sid)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pi-hole API returned %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// getExistingHosts retrieves the current hosts array from Pi-hole
func getExistingHosts(config Config, sid string) ([]string, error) {
	apiURL := fmt.Sprintf("%s/api/config/dns", config.PiHoleURL)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("sid", sid)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pi-hole API returned %d: %s", resp.StatusCode, string(body))
	}

	var dnsResp PiHoleV6ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&dnsResp); err != nil {
		return nil, err
	}

	return dnsResp.Config.DNS.Hosts, nil
}

func countMissing(hostnames []string, existing map[string]string) int {
	count := 0
	for _, hostname := range hostnames {
		if _, exists := existing[hostname]; !exists {
			count++
		}
	}
	return count
}
