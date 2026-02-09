package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Runner struct {
	DB      *pgxpool.Pool
	mu      sync.RWMutex
	lastRun time.Time
}

type tailStatus struct {
	Self tailNode            `json:"Self"`
	Peer map[string]tailNode `json:"Peer"`
}

type tailNode struct {
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
	OS           string   `json:"OS"`
	AllowedIPs   []string `json:"AllowedIPs"`
	PrimaryRoutes []string `json:"PrimaryRoutes"`
	Addrs        []string `json:"Addrs"`
}

type ollamaModel struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Size    int64  `json:"size"`
	Digest  string `json:"digest"`
	Details struct {
		Family            string `json:"family"`
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
	} `json:"details"`
}

func New(db *pgxpool.Pool) *Runner {
	return &Runner{DB: db}
}

func (r *Runner) LastRun() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastRun
}

func (r *Runner) setLastRun(t time.Time) {
	r.mu.Lock()
	r.lastRun = t
	r.mu.Unlock()
}

// tailscaleAvailable проверяет доступность tailscale binary
func tailscaleAvailable() bool {
	_, err := exec.LookPath("tailscale")
	return err == nil
}

func (r *Runner) Run(ctx context.Context) error {
	start := time.Now()
	log.Printf("discovery: start")

	hasTailscale := tailscaleAvailable()
	var status tailStatus
	count := 0

	if hasTailscale {
		out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
		if err != nil {
			log.Printf("discovery: tailscale status error (continuing without): %v", err)
		} else if err := json.Unmarshal(out, &status); err != nil {
			log.Printf("discovery: tailscale json parse error (continuing without): %v", err)
		} else {
			processNode := func(node tailNode) {
				id := node.DNSName
				if id == "" {
					id = node.HostName
				}
				if id == "" {
					return
				}
				ip := ""
				if len(node.TailscaleIPs) > 0 {
					ip = node.TailscaleIPs[0]
				}
				addrs := buildAddrs(node)
				hostHeaders := buildHostHeaders(node)
				if err := r.upsertDevice(ctx, id, node, ip, addrs, hostHeaders); err != nil {
					log.Printf("discovery: upsert error id=%s err=%v", id, err)
					return
				}
				if node.Online {
					r.probeOllama(ctx, id, addrs, hostHeaders)
				}
				count++
			}

			processNode(status.Self)
			for _, node := range status.Peer {
				processNode(node)
			}
		}
	} else {
		log.Printf("discovery: tailscale not found, skipping mesh discovery")
	}

	// Subnet scan работает независимо от Tailscale
	if os.Getenv("DISCOVERY_SCAN_SUBNETS") == "1" {
		if hasTailscale && status.Self.HostName != "" {
			r.scanSubnets(ctx, status.Self)
		} else {
			// Subnet scan по DISCOVERY_SUBNETS env без Tailscale self-node
			r.scanSubnets(ctx, tailNode{})
		}
	} else {
		r.clearLanDevices(ctx)
	}

	// Обработка offline устройств (только если Tailscale доступен)
	offlineIDs := []string{}
	if hasTailscale && status.Self.HostName != "" {
		offlineIDs = r.collectOfflineDeviceIDs(ctx, status)
		if len(offlineIDs) > 0 {
			if err := HandleOfflineDevices(ctx, r.DB, offlineIDs); err != nil {
				log.Printf("discovery: offline handler error: %v", err)
			}
		}
	}

	r.setLastRun(time.Now())
	log.Printf("discovery: done peers=%d offline=%d tailscale=%v elapsed=%s", count, len(offlineIDs), hasTailscale, time.Since(start))
	return nil
}

// collectOfflineDeviceIDs собирает ID всех offline устройств из Tailscale status
func (r *Runner) collectOfflineDeviceIDs(ctx context.Context, status tailStatus) []string {
	offlineIDs := []string{}

	check := func(node tailNode) {
		if !node.Online {
			id := node.DNSName
			if id == "" {
				id = node.HostName
			}
			if id != "" {
				offlineIDs = append(offlineIDs, id)
			}
		}
	}

	check(status.Self)
	for _, node := range status.Peer {
		check(node)
	}

	return offlineIDs
}

func (r *Runner) upsertDevice(ctx context.Context, id string, node tailNode, ip string, addrs []string, hostHeaders []string) error {
	status := "offline"
	lastSeen := time.Time{}
	if node.Online {
		status = "online"
		lastSeen = time.Now().UTC()
	}
	meta := map[string]any{
		"dns":      node.DNSName,
		"hostname": node.HostName,
		"ip":       ip,
		"ips":      node.TailscaleIPs,
		"addrs":    addrs,
		"host_headers": hostHeaders,
		"os":       node.OS,
		"online":   node.Online,
	}
	metaJSON, _ := json.Marshal(meta)

	if node.Online {
		_, err := r.DB.Exec(ctx, `
			INSERT INTO devices (id, name, platform, arch, host, tags, status, last_seen, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, now())
			ON CONFLICT (id) DO UPDATE SET
			  name = excluded.name,
			  platform = excluded.platform,
			  host = excluded.host,
			  tags = excluded.tags,
			  status = excluded.status,
			  last_seen = excluded.last_seen,
			  updated_at = now()
		`, id, node.HostName, node.OS, "", node.DNSName, metaJSON, status, lastSeen)
		return err
	}
	_, err := r.DB.Exec(ctx, `
		INSERT INTO devices (id, name, platform, arch, host, tags, status, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, now())
		ON CONFLICT (id) DO UPDATE SET
		  name = excluded.name,
		  platform = excluded.platform,
		  host = excluded.host,
		  tags = excluded.tags,
		  status = excluded.status,
		  updated_at = now()
	`, id, node.HostName, node.OS, "", node.DNSName, metaJSON, status)
	return err
}

func (r *Runner) probeOllama(ctx context.Context, deviceID string, addrs []string, hostHeaders []string) {
	client := http.Client{Timeout: 2 * time.Second}
	seen := map[string]bool{}
	results := make([]map[string]any, 0, len(addrs))
	bestAddr := ""
	bestLatency := int64(0)
	bestModels := []string{}
	bestHost := ""

	for _, addr := range addrs {
		addr = strings.TrimSpace(addr)
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		url := "http://" + formatAddr(addr) + ":11434/api/tags"
		attemptHosts := []string{""}
		if net.ParseIP(addr) != nil {
			attemptHosts = append(attemptHosts, hostHeaders...)
		}
		for _, host := range attemptHosts {
			start := time.Now()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				continue
			}
			if host != "" {
				req.Host = host
			}
			resp, err := client.Do(req)
			latency := time.Since(start).Milliseconds()
			if err != nil {
				log.Printf("discovery: ollama probe fail device=%s addr=%s host=%s err=%v", deviceID, addr, host, err)
				results = append(results, map[string]any{"addr": addr, "host": host, "ok": false, "latency_ms": latency, "error": err.Error()})
				continue
			}
			if resp.StatusCode != http.StatusOK {
				log.Printf("discovery: ollama probe bad status device=%s addr=%s host=%s status=%d", deviceID, addr, host, resp.StatusCode)
				_ = resp.Body.Close()
				results = append(results, map[string]any{"addr": addr, "host": host, "ok": false, "latency_ms": latency, "status": resp.StatusCode})
				continue
			}
		var data struct {
			Models []ollamaModel `json:"models"`
		}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				_ = resp.Body.Close()
				log.Printf("discovery: ollama decode error device=%s addr=%s host=%s err=%v", deviceID, addr, host, err)
				results = append(results, map[string]any{"addr": addr, "host": host, "ok": false, "latency_ms": latency, "error": err.Error()})
				continue
			}
			_ = resp.Body.Close()
		models := []string{}
		for _, m := range data.Models {
			name := strings.TrimSpace(m.Name)
			if name != "" {
				models = append(models, name)
			}
		}
		if err := r.syncDeviceModels(ctx, deviceID, data.Models); err != nil {
			log.Printf("discovery: model sync error device=%s err=%v", deviceID, err)
		}
			results = append(results, map[string]any{"addr": addr, "host": host, "ok": true, "latency_ms": latency, "models_count": len(models)})
			if bestAddr == "" || latency < bestLatency {
				bestAddr = addr
				bestLatency = latency
				bestModels = models
				bestHost = host
			}
		}
	}

	if bestAddr == "" {
		return
	}
	meta := map[string]any{
		"ollama":           true,
		"models":           bestModels,
		"ollama_addr":      bestAddr,
		"ollama_latency_ms": bestLatency,
		"ollama_addrs":     results,
		"ollama_host":      bestHost,
	}
	metaJSON, _ := json.Marshal(meta)
	_, err := r.DB.Exec(ctx, `
		UPDATE devices
		SET tags = COALESCE(tags, '{}'::jsonb) || $1::jsonb,
		    updated_at = now()
		WHERE id = $2
	`, metaJSON, deviceID)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("discovery: ollama update error device=%s err=%v", deviceID, err)
		return
	}
	log.Printf("discovery: ollama ok device=%s addr=%s host=%s latency=%dms models=%d", deviceID, bestAddr, bestHost, bestLatency, len(bestModels))
}

func formatAddr(addr string) string {
	if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
		return "[" + addr + "]"
	}
	return addr
}

func buildAddrs(node tailNode) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		v = strings.TrimSuffix(v, ".")
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, ip := range node.TailscaleIPs {
		add(ip)
	}
	if node.DNSName != "" {
		add(node.DNSName)
	}
	if node.HostName != "" {
		if !strings.ContainsAny(node.HostName, " \t") {
			add(node.HostName)
		}
	}
	return out
}

func buildHostHeaders(node tailNode) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		v = strings.TrimSuffix(v, ".")
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	if node.DNSName != "" {
		add(node.DNSName)
	}
	if node.HostName != "" && !strings.ContainsAny(node.HostName, " \t") {
		add(node.HostName)
	}
	return out
}

func (r *Runner) syncDeviceModels(ctx context.Context, deviceID string, models []ollamaModel) error {
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			continue
		}
		modelIDs = append(modelIDs, name)
		paramsB := parseParamsB(m.Details.ParameterSize)
		sizeGB := float64(0)
		if m.Size > 0 {
			sizeGB = float64(m.Size) / (1024 * 1024 * 1024)
		}
		meta := map[string]any{
			"digest":     m.Digest,
			"size_bytes": m.Size,
			"details": map[string]any{
				"family":             m.Details.Family,
				"parameter_size":     m.Details.ParameterSize,
				"quantization_level": m.Details.QuantizationLevel,
			},
		}
		metaJSON, _ := json.Marshal(meta)
		_, _ = r.DB.Exec(ctx, `
			INSERT INTO models (id, provider, family, kind, params_b, size_gb, quant, meta, updated_at)
			VALUES ($1, 'ollama', $2, 'chat', $3, $4, $5, $6::jsonb, now())
			ON CONFLICT (id) DO UPDATE SET
			  provider = excluded.provider,
			  family = excluded.family,
			  params_b = excluded.params_b,
			  size_gb = excluded.size_gb,
			  quant = excluded.quant,
			  meta = excluded.meta,
			  updated_at = now()
		`, name, m.Details.Family, paramsB, nullableFloat(sizeGB), m.Details.QuantizationLevel, metaJSON)

		_, _ = r.DB.Exec(ctx, `
			INSERT INTO device_models (device_id, model_id, available, meta, updated_at)
			VALUES ($1, $2, TRUE, $3::jsonb, now())
			ON CONFLICT (device_id, model_id) DO UPDATE SET
			  available = TRUE,
			  meta = excluded.meta,
			  updated_at = now()
		`, deviceID, name, metaJSON)
	}

	if len(modelIDs) > 0 {
		_, _ = r.DB.Exec(ctx, `
			UPDATE device_models
			SET available = FALSE, updated_at = now()
			WHERE device_id = $1 AND NOT (model_id = ANY($2))
		`, deviceID, modelIDs)
	}
	return nil
}

func parseParamsB(raw string) *float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	last := raw[len(raw)-1]
	mult := 1.0
	if last == 'B' || last == 'b' {
		raw = raw[:len(raw)-1]
		mult = 1.0
	} else if last == 'M' || last == 'm' {
		raw = raw[:len(raw)-1]
		mult = 0.001
	} else if last == 'K' || last == 'k' {
		raw = raw[:len(raw)-1]
		mult = 0.000001
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil
	}
	val := v * mult
	return &val
}

func nullableFloat(v float64) *float64 {
	if v == 0 {
		return nil
	}
	return &v
}

func (r *Runner) clearLanDevices(ctx context.Context) {
	tag, err := r.DB.Exec(ctx, `DELETE FROM devices WHERE id LIKE 'lan-%'`)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("discovery: clear lan devices error: %v", err)
		return
	}
	if tag.RowsAffected() > 0 {
		log.Printf("discovery: cleared lan devices count=%d", tag.RowsAffected())
	}
}

func (r *Runner) scanSubnets(ctx context.Context, self tailNode) {
	subnets := parseSubnets(self)
	if len(subnets) == 0 {
		return
	}

	skip := map[string]bool{}
	for _, addr := range self.Addrs {
		ip := extractHost(addr)
		if ip != "" {
			skip[ip] = true
			_, _ = r.DB.Exec(ctx, `DELETE FROM devices WHERE id = $1`, "lan-"+ip)
		}
	}

	log.Printf("discovery: subnet scan start subnets=%d", len(subnets))
	type job struct {
		ip string
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	workerCount := 24
	client := http.Client{Timeout: 300 * time.Millisecond}

	worker := func() {
		defer wg.Done()
		for j := range jobs {
			url := "http://" + j.ip + ":11434/api/tags"
			resp, err := client.Get(url)
			if err != nil {
				continue
			}
			if resp.StatusCode != http.StatusOK {
				_ = resp.Body.Close()
				continue
			}
			var data struct {
				Models []struct {
					Name string `json:"name"`
				} `json:"models"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				_ = resp.Body.Close()
				continue
			}
			_ = resp.Body.Close()
			models := []string{}
			for _, m := range data.Models {
				name := strings.TrimSpace(m.Name)
				if name != "" {
					models = append(models, name)
				}
			}
			r.upsertLanDevice(ctx, j.ip, models)
		}
	}

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go worker()
	}

	for _, cidr := range subnets {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		start, last := ipv4Range(prefix)
		// пропускаем network и broadcast
		for ip := start.Next(); ip.Compare(last) < 0; ip = ip.Next() {
			if ctx.Err() != nil {
				break
			}
			ipStr := ip.String()
			if skip[ipStr] {
				continue
			}
			jobs <- job{ip: ipStr}
		}
	}
	close(jobs)
	wg.Wait()
	log.Printf("discovery: subnet scan done")
}

func parseSubnets(self tailNode) []string {
	candidates := []string{}
	if env := strings.TrimSpace(getEnv("DISCOVERY_SUBNETS")); env != "" {
		for _, item := range strings.Split(env, ",") {
			item = strings.TrimSpace(item)
			if item != "" {
				candidates = append(candidates, item)
			}
		}
	}
	candidates = append(candidates, self.PrimaryRoutes...)
	candidates = append(candidates, self.AllowedIPs...)

	out := []string{}
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		prefix, err := netip.ParsePrefix(c)
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		if !isPrivateIPv4(prefix.Addr()) {
			continue
		}
		ones := prefix.Bits()
		bits := prefix.Addr().BitLen()
		if bits-ones > 9 { // > 512 адресов
			continue
		}
		out = append(out, prefix.String())
	}
	return out
}

func isPrivateIPv4(addr netip.Addr) bool {
	if !addr.Is4() {
		return false
	}
	b := addr.As4()
	switch {
	case b[0] == 10:
		return true
	case b[0] == 172 && b[1] >= 16 && b[1] <= 31:
		return true
	case b[0] == 192 && b[1] == 168:
		return true
	default:
		return false
	}
}

func (r *Runner) upsertLanDevice(ctx context.Context, ip string, models []string) {
	id := "lan-" + ip
	meta := map[string]any{
		"ip":          ip,
		"ollama":      true,
		"ollama_addr": ip,
		"models":      models,
		"source":      "subnet",
	}
	metaJSON, _ := json.Marshal(meta)
	_, err := r.DB.Exec(ctx, `
		INSERT INTO devices (id, name, platform, arch, host, tags, status, last_seen, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, 'online', now(), now())
		ON CONFLICT (id) DO UPDATE SET
		  name = excluded.name,
		  platform = excluded.platform,
		  host = excluded.host,
		  tags = excluded.tags,
		  status = 'online',
		  last_seen = now(),
		  updated_at = now()
	`, id, id, "lan", "", ip, metaJSON)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("discovery: lan upsert error ip=%s err=%v", ip, err)
		return
	}
	log.Printf("discovery: ollama ok device=%s addr=%s models=%d", id, ip, len(models))
}

func getEnv(key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return ""
}

func ipv4Range(prefix netip.Prefix) (netip.Addr, netip.Addr) {
	prefix = prefix.Masked()
	start := prefix.Addr()
	if !start.Is4() {
		return start, start
	}
	ones := prefix.Bits()
	hostBits := 32 - ones
	if hostBits <= 0 {
		return start, start
	}
	ip4 := start.As4()
	startUint := uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
	endUint := startUint | ((1 << hostBits) - 1)
	end := netip.AddrFrom4([4]byte{
		byte(endUint >> 24),
		byte(endUint >> 16),
		byte(endUint >> 8),
		byte(endUint),
	})
	return start, end
}

func extractHost(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	if idx := strings.LastIndex(addr, ":"); idx > 0 {
		return addr[:idx]
	}
	return addr
}
