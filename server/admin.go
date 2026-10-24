package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// AdminUsername is the fixed login name for the administration dashboard.
const AdminUsername = "administrator"

// Admin dashboard configuration. These values are populated from the command
// line flags during startup (see configure.go). The dashboard is enabled with
// the -admin flag. The password is not configured up front: it is defined by
// the administrator on the very first connection and then stored, bcrypt-hashed,
// in adminPasswordFile so it survives restarts.
var (
	adminEnabled      bool
	adminPasswordFile string
	adminPath         = DEFAULT_ADMIN_PATH
	adminDataFile     = DEFAULT_ADMIN_DATA_FILE
	geoIPAPIURL       = DEFAULT_GEOIP_API_URL
)

// adminMinPasswordLen is the minimum length accepted for the admin password.
const adminMinPasswordLen = 8

// adminPasswordHash holds the current bcrypt hash of the admin password, or an
// empty slice when no password has been defined yet (first-run state).
var (
	adminPasswordHash []byte
	adminPasswordMu   sync.RWMutex
)

const adminSessionCookie = "nvda_admin_session"
const adminSessionTTL = 2 * time.Hour

// AdminEnabled reports whether the administration dashboard is active.
func AdminEnabled() bool {
	return adminEnabled
}

// adminPasswordDefined reports whether a password has already been set.
func adminPasswordDefined() bool {
	adminPasswordMu.RLock()
	defer adminPasswordMu.RUnlock()
	return len(adminPasswordHash) > 0
}

// loadAdminPassword loads the stored bcrypt hash from adminPasswordFile, if any.
// A missing file simply means the password has not been defined yet.
func loadAdminPassword() {
	if adminPasswordFile == "" {
		return
	}
	data, err := os.ReadFile(adminPasswordFile)
	if err != nil {
		if !os.IsNotExist(err) {
			Log(LOG_INFO, "Warning: unable to read admin password file "+adminPasswordFile+": "+err.Error())
		}
		return
	}
	hash := strings.TrimSpace(string(data))
	if hash == "" {
		return
	}
	adminPasswordMu.Lock()
	adminPasswordHash = []byte(hash)
	adminPasswordMu.Unlock()
}

// setAdminPassword hashes and stores the given password, persisting it to disk
// when adminPasswordFile is configured.
func setAdminPassword(password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	adminPasswordMu.Lock()
	adminPasswordHash = hash
	adminPasswordMu.Unlock()
	if adminPasswordFile != "" {
		if err := os.WriteFile(adminPasswordFile, append(hash, '\n'), 0o600); err != nil {
			Log(LOG_INFO, "Warning: unable to write admin password file "+adminPasswordFile+": "+err.Error())
			return err
		}
	}
	return nil
}

// adminSessions stores active session tokens together with their expiry time.
var (
	adminSessions   = make(map[string]time.Time)
	adminSessionsMu sync.Mutex
)

func newAdminToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func createAdminSession() (string, error) {
	token, err := newAdminToken()
	if err != nil {
		return "", err
	}
	adminSessionsMu.Lock()
	adminSessions[token] = time.Now().Add(adminSessionTTL)
	adminSessionsMu.Unlock()
	return token, nil
}

func validAdminSession(token string) bool {
	if token == "" {
		return false
	}
	adminSessionsMu.Lock()
	defer adminSessionsMu.Unlock()
	expiry, ok := adminSessions[token]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(adminSessions, token)
		return false
	}
	return true
}

func deleteAdminSession(token string) {
	if token == "" {
		return
	}
	adminSessionsMu.Lock()
	delete(adminSessions, token)
	adminSessionsMu.Unlock()
}

func adminRequestAuthorized(r *http.Request) bool {
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil {
		return false
	}
	return validAdminSession(cookie.Value)
}

// checkAdminCredentials verifies the submitted username and password against the
// fixed administrator account and the stored bcrypt hash.
func checkAdminCredentials(user, password string) bool {
	if user != AdminUsername {
		return false
	}
	adminPasswordMu.RLock()
	hash := adminPasswordHash
	adminPasswordMu.RUnlock()
	if len(hash) == 0 {
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}

// adminClientInfo is the JSON representation of a single connected client.
type adminClientInfo struct {
	ID          int    `json:"id"`
	IP          string `json:"ip"`
	Location    string `json:"location"`
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	Role        string `json:"role"`
	Channel     string `json:"channel"`
	ConnectedAt string `json:"connected_at"`
	Uptime      string `json:"uptime"`
	// LastReceived says how long ago the client last sent anything, so that a
	// connection whose computer has vanished can be told from a living one.
	LastReceived string `json:"last_received"`
	// Duplicate is set when another connection shares this one's address, key
	// and role: most likely the same computer, reconnected while the relay has
	// not yet noticed that its previous connection is dead.
	Duplicate bool `json:"duplicate"`
	sortKey   time.Time
}

// adminBlockedInfo is the JSON representation of a blocked machine.
type adminBlockedInfo struct {
	IP        string `json:"ip"`
	Location  string `json:"location"`
	ClientID  int    `json:"client_id"`
	Channel   string `json:"channel"`
	BlockedAt string `json:"blocked_at"`
}

// adminSnapshot is the JSON payload sent to the dashboard.
type adminSnapshot struct {
	Connected  int                `json:"connected"`
	Channels   int                `json:"channels"`
	Goroutines int                `json:"goroutines"`
	MemAllocMB float64            `json:"mem_alloc_mb"`
	MemSysMB   float64            `json:"mem_sys_mb"`
	MemUsedPct float64            `json:"mem_used_pct"`
	MemUsedOK  bool               `json:"mem_used_ok"`
	CPUPct     float64            `json:"cpu_pct"`
	CPUOK      bool               `json:"cpu_ok"`
	Timestamp  string             `json:"timestamp"`
	Clients    []adminClientInfo  `json:"clients"`
	Blocked    []adminBlockedInfo `json:"blocked"`
}

// buildBlockedInfos lists the blocked machines for the dashboard.
func buildBlockedInfos() []adminBlockedInfo {
	entries := blockedClients()
	infos := make([]adminBlockedInfo, 0, len(entries))
	for _, entry := range entries {
		infos = append(infos, adminBlockedInfo{
			IP:        entry.IP,
			Location:  geoLocationForIP(entry.IP),
			ClientID:  entry.ClientID,
			Channel:   entry.Channel,
			BlockedAt: entry.BlockedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return infos
}

func roleLabel(connectionType string) string {
	switch connectionType {
	case "master":
		return "master"
	case "slave":
		return "slave"
	default:
		return "-"
	}
}

func formatUptime(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return strconv.Itoa(h) + "h" + pad2(m) + "m" + pad2(s) + "s"
	}
	if m > 0 {
		return strconv.Itoa(m) + "m" + pad2(s) + "s"
	}
	return strconv.Itoa(s) + "s"
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// formatLastReceived says how long ago a client last sent anything.
func formatLastReceived(last, now time.Time) string {
	if last.IsZero() {
		return "jamais"
	}
	elapsed := now.Sub(last)
	if elapsed < 0 {
		elapsed = 0
	}
	return "il y a " + formatUptime(elapsed)
}

// duplicateKey groups the connections which most likely come from one computer.
func duplicateKey(info adminClientInfo) string {
	return info.IP + "\x00" + info.Channel + "\x00" + info.Role
}

// markDuplicates flags the connections sharing an address, a key and a role.
// A connection without a key has not joined anything yet and is left alone.
func markDuplicates(infos []adminClientInfo) {
	counts := make(map[string]int, len(infos))
	for _, info := range infos {
		if info.Channel != "" {
			counts[duplicateKey(info)]++
		}
	}
	for i := range infos {
		infos[i].Duplicate = infos[i].Channel != "" && counts[duplicateKey(infos[i])] > 1
	}
}

// buildAdminSnapshot collects the current server state.
func buildAdminSnapshot() adminSnapshot {
	list, channelCount := snapshotClients()
	now := time.Now()

	infos := make([]adminClientInfo, 0, len(list))
	for _, c := range list {
		channelName := ""
		if ch := c.GetChannel(); ch != nil {
			channelName = ch.Name()
		}
		ip := c.GetIP()
		connectedAt := c.GetConnectedAt()
		infos = append(infos, adminClientInfo{
			ID:          c.GetID(),
			IP:          ip,
			Location:    geoLocationForIP(ip),
			Port:        c.GetPort(),
			Protocol:    c.GetProtocol(),
			Role:        roleLabel(c.GetConnectionType()),
			Channel:     channelName,
			ConnectedAt:  connectedAt.Format("2006-01-02 15:04:05"),
			Uptime:       formatUptime(now.Sub(connectedAt)),
			LastReceived: formatLastReceived(c.GetLastReceived(), now),
			sortKey:      connectedAt,
		})
	}
	markDuplicates(infos)

	// Most recently connected clients first.
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].sortKey.After(infos[j].sortKey)
	})

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	memPct, memOK := systemMemUsedPct()
	cpuPct, cpuOK := systemCPUPct()

	return adminSnapshot{
		Connected:  len(list),
		Channels:   channelCount,
		Goroutines: runtime.NumGoroutine(),
		MemAllocMB: float64(mem.Alloc) / (1024 * 1024),
		MemSysMB:   float64(mem.Sys) / (1024 * 1024),
		MemUsedPct: memPct,
		MemUsedOK:  memOK,
		CPUPct:     cpuPct,
		CPUOK:      cpuOK,
		Timestamp:  now.Format("2006-01-02 15:04:05"),
		Clients:    infos,
		Blocked:    buildBlockedInfos(),
	}
}

// systemMemUsedPct returns the system-wide used-memory percentage by reading
// /proc/meminfo (Linux only). The second return value is false when the metric
// is unavailable (e.g. on non-Linux platforms).
func systemMemUsedPct() (float64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	var total, available uint64
	haveTotal, haveAvail := false, false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				total = v
				haveTotal = true
			}
		case "MemAvailable:":
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				available = v
				haveAvail = true
			}
		}
		if haveTotal && haveAvail {
			break
		}
	}
	if !haveTotal || !haveAvail || total == 0 {
		return 0, false
	}
	used := total - available
	return (float64(used) / float64(total)) * 100, true
}

var (
	cpuSampleMu   sync.Mutex
	cpuLastTotal  uint64
	cpuLastIdle   uint64
	cpuLastProcess uint64
	cpuLastPct    float64
	cpuLastProcessPct float64
	cpuLastSample time.Time
	cpuHaveSample bool
	cpuHaveValue  bool
)

// systemCPUPct returns the system-wide CPU usage percentage computed from the
// delta since the previous sample, reading /proc/stat (Linux only). The second
// return value is false when the metric is unavailable. A recently computed
// value is reused so concurrent dashboard clients do not disturb the delta.
func systemCPUPct() (float64, bool) {
	systemPct, _, ok := cpuMetrics()
	return systemPct, ok
}

// processCPUPct returns the CPU percentage used by this server process. The
// value is expressed like the value shown by common process monitors: 100% is
// one fully used CPU core and a process can therefore exceed 100% on a
// multi-core machine. The second return value is false when the metric is
// unavailable.
func processCPUPct() (float64, bool) {
	_, processPct, ok := cpuMetrics()
	return processPct, ok
}

// cpuMetrics returns cached system-wide and process CPU percentages. Both
// values are sampled together so the historical metrics use the same interval.
func cpuMetrics() (float64, float64, bool) {
	cpuSampleMu.Lock()
	defer cpuSampleMu.Unlock()

	if cpuHaveSample && time.Since(cpuLastSample) < time.Second {
		return cpuLastPct, cpuLastProcessPct, cpuHaveValue
	}

	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	content := string(data)
	line := content
	if idx := strings.IndexByte(content, '\n'); idx >= 0 {
		line = content[:idx]
	}
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			continue
		}
		total += v
		if i == 4 || i == 5 { // idle + iowait
			idle += v
		}
	}
	processTicks, err := processCPUTicks()
	if err != nil {
		return 0, 0, false
	}

	if !cpuHaveSample {
		cpuLastTotal = total
		cpuLastIdle = idle
		cpuLastProcess = processTicks
		cpuLastSample = time.Now()
		cpuHaveSample = true
		cpuHaveValue = false
		return 0, 0, false
	}

	if total < cpuLastTotal || idle < cpuLastIdle || processTicks < cpuLastProcess {
		cpuLastTotal = total
		cpuLastIdle = idle
		cpuLastProcess = processTicks
		cpuLastSample = time.Now()
		cpuHaveValue = false
		return 0, 0, false
	}
	totalDelta := total - cpuLastTotal
	idleDelta := idle - cpuLastIdle
	processDelta := processTicks - cpuLastProcess
	cpuLastTotal = total
	cpuLastIdle = idle
	cpuLastProcess = processTicks
	cpuLastSample = time.Now()

	if totalDelta == 0 {
		return cpuLastPct, cpuLastProcessPct, cpuHaveValue
	}
	pct := (float64(totalDelta-idleDelta) / float64(totalDelta)) * 100
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	processPct := float64(processDelta) / float64(totalDelta) * float64(runtime.NumCPU()) * 100
	if processPct < 0 {
		processPct = 0
	}
	cpuLastPct = pct
	cpuLastProcessPct = processPct
	cpuHaveValue = true
	return pct, processPct, true
}

func processCPUTicks() (uint64, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, err
	}
	line := string(data)
	endName := strings.LastIndexByte(line, ')')
	if endName < 0 || endName+2 >= len(line) {
		return 0, strconv.ErrSyntax
	}
	fields := strings.Fields(line[endName+2:])
	// The slice starts at field 3 (state), so fields 11 and 12 are utime
	// (field 14) and stime (field 15) from procfs' process stat format.
	if len(fields) < 13 {
		return 0, strconv.ErrSyntax
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return utime + stime, nil
}

// registerAdminRoutes registers the admin dashboard handlers on the provided
// mux. It is a no-op when the dashboard is disabled.
func registerAdminRoutes(mux *http.ServeMux) {
	if !AdminEnabled() {
		return
	}
	loadAdminPassword()
	startAdminHistory()
	base := adminPath
	mux.HandleFunc(base, handleAdminDashboard)
	mux.HandleFunc(base+"/login", handleAdminLogin)
	mux.HandleFunc(base+"/logout", handleAdminLogout)
	mux.HandleFunc(base+"/setup", handleAdminSetup)
	mux.HandleFunc(base+"/events", handleAdminEvents)
	mux.HandleFunc(base+"/history", handleAdminHistory)
	mux.HandleFunc(base+"/history/day", handleAdminHistoryDay)
	mux.HandleFunc(base+"/clients/block", handleAdminBlockClient)
	mux.HandleFunc(base+"/clients/unblock", handleAdminUnblockClient)
	if adminPasswordDefined() {
		Log(LOG_INFO, "Admin dashboard enabled at "+base+" (user: "+AdminUsername+")")
	} else {
		Log(LOG_INFO, "Admin dashboard enabled at "+base+" (user: "+AdminUsername+") - password not set yet, define it at "+base+"/setup on first connection. NVDA Remote connections are refused until then.")
	}
}

// adminActionAuthorized checks a state-changing request from the dashboard. The
// session cookie is SameSite=Strict, and the browser headers are checked too, so a
// page on another site cannot cut connections through the administrator's browser.
func adminActionAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !adminRequestAuthorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		// The dashboard is only served over TLS, and r.TLS cannot be trusted here:
		// it is nil when the connection went through the protocol detector.
		expected := "https://" + r.Host
		if !strings.EqualFold(origin, expected) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return false
		}
	}
	return true
}

func writeAdminJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// handleAdminBlockClient cuts one connection and blocks its address.
func handleAdminBlockClient(w http.ResponseWriter, r *http.Request) {
	if !adminActionAuthorized(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "Requête invalide."})
		return
	}
	id, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("id")))
	if err != nil || id <= 0 {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "Identifiant de connexion invalide."})
		return
	}
	entry, blocked, err := blockClientByID(id)
	if err != nil {
		writeAdminJSON(w, http.StatusNotFound, map[string]string{"error": "Cette connexion n'existe plus."})
		return
	}
	msg := "Connexion " + strconv.Itoa(entry.ClientID) + " coupée. L'adresse " + entry.IP + " ne peut plus rejoindre le canal " + entry.Channel + "."
	if !blocked {
		msg = "Connexion " + strconv.Itoa(entry.ClientID) + " coupée. Elle n'avait rejoint aucun canal, rien n'a été bloqué."
	}
	writeAdminJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"ip":      entry.IP,
		"channel": entry.Channel,
		"id":      entry.ClientID,
		"blocked": blocked,
		"msg":     msg,
	})
}

// handleAdminUnblockClient lifts the block on an address for one channel.
func handleAdminUnblockClient(w http.ResponseWriter, r *http.Request) {
	if !adminActionAuthorized(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "Requête invalide."})
		return
	}
	ip := strings.TrimSpace(r.PostFormValue("ip"))
	channel := strings.TrimSpace(r.PostFormValue("channel"))
	if ip == "" || channel == "" {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "Adresse ou canal manquant."})
		return
	}
	if !unblock(ip, channel) {
		writeAdminJSON(w, http.StatusNotFound, map[string]string{"error": "Cette adresse n'est pas bloquée sur ce canal."})
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"ip":      ip,
		"channel": channel,
		"msg":     "L'adresse " + ip + " peut de nouveau rejoindre le canal " + channel + ".",
	})
}

func handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != adminPath {
		http.NotFound(w, r)
		return
	}
	if !adminPasswordDefined() {
		http.Redirect(w, r, adminPath+"/setup", http.StatusSeeOther)
		return
	}
	if !adminRequestAuthorized(r) {
		http.Redirect(w, r, adminPath+"/login", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(strings.ReplaceAll(adminDashboardHTML, "ADMINPATH", html.EscapeString(adminPath))))
}

func handleAdminSetup(w http.ResponseWriter, r *http.Request) {
	// Once a password exists, the setup page is no longer available.
	if adminPasswordDefined() {
		http.Redirect(w, r, adminPath+"/login", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		password := r.PostFormValue("password")
		confirm := r.PostFormValue("confirm")
		if len(password) < adminMinPasswordLen {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(adminSetupHTML("Le mot de passe doit contenir au moins " + strconv.Itoa(adminMinPasswordLen) + " caractères.")))
			return
		}
		if password != confirm {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(adminSetupHTML("Les deux mots de passe ne correspondent pas.")))
			return
		}
		if err := setAdminPassword(password); err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(adminSetupHTML("Impossible d'enregistrer le mot de passe. Consultez les journaux du serveur.")))
			return
		}
		token, err := createAdminSession()
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     adminSessionCookie,
			Value:    token,
			Path:     adminPath,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   int(adminSessionTTL.Seconds()),
		})
		http.Redirect(w, r, adminPath, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(adminSetupHTML("")))
}

func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	// Before any password is defined, redirect to the first-run setup page.
	if !adminPasswordDefined() {
		http.Redirect(w, r, adminPath+"/setup", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		user := r.PostFormValue("username")
		password := r.PostFormValue("password")
		if checkAdminCredentials(user, password) {
			token, err := createAdminSession()
			if err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     adminSessionCookie,
				Value:    token,
				Path:     adminPath,
				HttpOnly: true,
				Secure:   true,
				SameSite: http.SameSiteStrictMode,
				MaxAge:   int(adminSessionTTL.Seconds()),
			})
			http.Redirect(w, r, adminPath, http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(adminLoginHTML(true)))
		return
	}
	if adminRequestAuthorized(r) {
		http.Redirect(w, r, adminPath, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(adminLoginHTML(false)))
}

func handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(adminSessionCookie); err == nil {
		deleteAdminSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    "",
		Path:     adminPath,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, adminPath+"/login", http.StatusSeeOther)
}

func handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	if !adminRequestAuthorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	writeSnapshot := func() bool {
		data, err := json.Marshal(buildAdminSnapshot())
		if err != nil {
			return false
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			return false
		}
		if _, err := w.Write(data); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !writeSnapshot() {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !writeSnapshot() {
				return
			}
		}
	}
}

func adminLoginHTML(failed bool) string {
	errBlock := ""
	if failed {
		errBlock = `<p class="error" role="alert">Identifiants incorrects.</p>`
	}
	return `<!DOCTYPE html>
<html lang="fr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>NVDA REMOTE GO Accessolutions - Connexion administration</title>
<style>
body{font-family:system-ui,Arial,sans-serif;background:#0f1720;color:#e6edf3;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}
.card{background:#161f2b;padding:2rem;border-radius:12px;box-shadow:0 8px 30px rgba(0,0,0,.4);width:320px}
h1{font-size:1.25rem;margin-top:0}
label{display:block;margin:.75rem 0 .25rem}
input{width:100%;padding:.5rem;border-radius:6px;border:1px solid #30363d;background:#0d1117;color:#e6edf3;box-sizing:border-box}
button{margin-top:1.25rem;width:100%;padding:.6rem;border:0;border-radius:6px;background:#2563eb;color:#fff;font-size:1rem;cursor:pointer}
button:hover{background:#1d4ed8}
.error{color:#f87171}
</style>
</head>
<body>
<main class="card">
<h1>Administration NVDA REMOTE GO Accessolutions</h1>
` + errBlock + `
<form method="post" action="` + html.EscapeString(adminPath) + `/login">
<label for="username">Nom d'utilisateur</label>
<input id="username" name="username" autocomplete="username" required autofocus>
<label for="password">Mot de passe</label>
<input id="password" name="password" type="password" autocomplete="current-password" required>
<button type="submit">Se connecter</button>
</form>
</main>
</body>
</html>`
}

func adminSetupHTML(errMsg string) string {
	errBlock := ""
	if errMsg != "" {
		errBlock = `<p class="error" role="alert">` + html.EscapeString(errMsg) + `</p>`
	}
	return `<!DOCTYPE html>
<html lang="fr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>NVDA REMOTE GO Accessolutions - Première configuration</title>
<style>
body{font-family:system-ui,Arial,sans-serif;background:#0f1720;color:#e6edf3;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}
.card{background:#161f2b;padding:2rem;border-radius:12px;box-shadow:0 8px 30px rgba(0,0,0,.4);width:340px}
h1{font-size:1.25rem;margin-top:0}
p.info{color:#9aa7b4;font-size:.9rem}
label{display:block;margin:.75rem 0 .25rem}
input{width:100%;padding:.5rem;border-radius:6px;border:1px solid #30363d;background:#0d1117;color:#e6edf3;box-sizing:border-box}
button{margin-top:1.25rem;width:100%;padding:.6rem;border:0;border-radius:6px;background:#2563eb;color:#fff;font-size:1rem;cursor:pointer}
button:hover{background:#1d4ed8}
.error{color:#f87171}
</style>
</head>
<body>
<main class="card">
<h1>Première configuration</h1>
<p class="info">Définissez le mot de passe du compte <strong>` + html.EscapeString(AdminUsername) + `</strong>. Il sera demandé lors des prochaines connexions.</p>
` + errBlock + `
<form method="post" action="` + html.EscapeString(adminPath) + `/setup">
<label for="password">Nouveau mot de passe</label>
<input id="password" name="password" type="password" autocomplete="new-password" minlength="` + strconv.Itoa(adminMinPasswordLen) + `" required autofocus>
<label for="confirm">Confirmer le mot de passe</label>
<input id="confirm" name="confirm" type="password" autocomplete="new-password" minlength="` + strconv.Itoa(adminMinPasswordLen) + `" required>
<button type="submit">Enregistrer</button>
</form>
</main>
</body>
</html>`
}

var adminDashboardHTML = `<!DOCTYPE html>
<html lang="fr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>NVDA REMOTE GO Accessolutions</title>
<style>
body{font-family:system-ui,Arial,sans-serif;background:#0f1720;color:#e6edf3;margin:0;padding:1.5rem}
header{display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:1rem}
h1{font-size:1.35rem;margin:0}
a.logout{color:#93c5fd;text-decoration:none}
a.logout:hover{text-decoration:underline}
a.history{color:#93c5fd;text-decoration:none;margin-right:1rem}
a.history:hover{text-decoration:underline}
.stats{margin:1.25rem 0}
.stat-line{background:#161f2b;border-radius:8px;padding:.65rem .95rem;margin:.45rem 0;font-size:1.05rem}
.stat-line .label{color:#9aa7b4}
.stat-line .value{font-weight:700;margin-left:.35rem}
table{width:100%;border-collapse:collapse;background:#161f2b;border-radius:10px;overflow:hidden}
th,td{padding:.6rem .75rem;text-align:left;border-bottom:1px solid #263243;font-size:.9rem}
th{background:#1d2836;position:sticky;top:0}
tr:hover td{background:#1b2534}
.role-master{color:#4ade80;font-weight:600}
.role-slave{color:#facc15;font-weight:600}
.muted{color:#9aa7b4}
.duplicate{color:#fbbf24;font-weight:600}
.duplicate-row td{opacity:.75}
.updated{color:#9aa7b4;font-size:.8rem;margin-top:1rem}
button.action{padding:.35rem .7rem;border:0;border-radius:6px;font-size:.85rem;cursor:pointer;color:#fff}
button.action:disabled{opacity:.5;cursor:default}
button.block{background:#b91c1c}
button.block:hover{background:#991b1b}
button.unblock{background:#15803d}
button.unblock:hover{background:#166534}
button.action:focus-visible{outline:3px solid #93c5fd;outline-offset:2px}
.status{min-height:1.5rem;margin:1rem 0;padding:.6rem .95rem;border-radius:8px;background:#161f2b;color:#e6edf3}
.status:empty{display:none}
.status.error{color:#fca5a5}
h2{font-size:1.1rem;margin:2rem 0 .5rem}
</style>
</head>
<body>
<header>
<h1>NVDA REMOTE GO Accessolutions</h1>
<span><a class="history" href="ADMINPATH/history">Historique</a><a class="logout" href="ADMINPATH/logout">Se déconnecter</a></span>
</header>
<section class="stats">
<p class="stat-line"><span class="label">Nombre de connectés :</span><span class="value" id="stat-connected">-</span></p>
<p class="stat-line"><span class="label">Pourcentage de mémoire utilisée :</span><span class="value" id="stat-mem">-</span></p>
<p class="stat-line"><span class="label">Processeur :</span><span class="value" id="stat-cpu">-</span></p>
</section>
<p class="status" id="status" role="status" aria-live="polite"></p>
<table>
<caption class="muted" style="text-align:left;margin-bottom:.5rem">Connexions actives (les plus récentes en haut)</caption>
<thead>
<tr>
<th scope="col">ID</th>
<th scope="col">Adresse IP</th>
<th scope="col">Localisation</th>
<th scope="col">Port</th>
<th scope="col">Protocole</th>
<th scope="col">Rôle</th>
<th scope="col">Clé d'accès (canal)</th>
<th scope="col">Connecté depuis</th>
<th scope="col">Durée</th>
<th scope="col">Dernier message reçu</th>
<th scope="col">Action</th>
</tr>
</thead>
<tbody id="clients"><tr><td colspan="11" class="muted">Chargement…</td></tr></tbody>
</table>
<p class="muted">Une ligne marquée « doublon probable » partage son adresse, sa clé et son rôle avec une autre : le plus souvent la même machine, reconnectée avant que le serveur ait constaté la fin de sa connexion précédente. Le serveur la retire de lui-même en une minute environ ; sa désactivation bloquerait aussi la connexion vivante.</p>
<h2 id="blocked-title">Machines bloquées</h2>
<table aria-labelledby="blocked-title">
<caption class="muted" style="text-align:left;margin-bottom:.5rem">Le blocage porte sur une adresse IP et une clé d'accès ensemble : les autres machines de la même adresse, sur d'autres clés, ne sont pas concernées. Cliquer sur « Activer » autorise de nouveau la machine à rejoindre cette clé.</caption>
<thead>
<tr>
<th scope="col">Adresse IP</th>
<th scope="col">Clé d'accès (canal)</th>
<th scope="col">Localisation</th>
<th scope="col">Connexion coupée (ID)</th>
<th scope="col">Bloquée le</th>
<th scope="col">Action</th>
</tr>
</thead>
<tbody id="blocked"><tr><td colspan="6" class="muted">Chargement…</td></tr></tbody>
</table>
<p class="updated">Dernière mise à jour : <span id="updated">-</span></p>
<script>
function esc(s){var d=document.createElement('div');d.textContent=s==null?'':s;return d.innerHTML;}
function roleClass(r){return r==='master'?'role-master':(r==='slave'?'role-slave':'muted');}
var pending={};
function setStatus(msg,isError){var p=document.getElementById('status');p.textContent=msg||'';p.className='status'+(isError?' error':'');}
function render(d){
	document.getElementById('stat-connected').textContent=d.connected;
	document.getElementById('stat-mem').textContent=d.mem_used_ok?(d.mem_used_pct.toFixed(1)+' %'):'indisponible';
	document.getElementById('stat-cpu').textContent=d.cpu_ok?(d.cpu_pct.toFixed(1)+' %'):'indisponible';
	document.getElementById('updated').textContent=d.timestamp;
	var tb=document.getElementById('clients');
	if(!d.clients||d.clients.length===0){tb.innerHTML='<tr><td colspan="11" class="muted">Aucune connexion active.</td></tr>';}
	else{
		var rows='';
		for(var i=0;i<d.clients.length;i++){
			var c=d.clients[i];
			var busy=pending['block:'+c.id]?' disabled':'';
			var dup=c.duplicate?' <span class="duplicate">(doublon probable)</span>':'';
			rows+='<tr'+(c.duplicate?' class="duplicate-row"':'')+'>'+
				'<td>'+esc(c.id)+dup+'</td>'+
				'<td>'+esc(c.ip)+'</td>'+
				'<td>'+esc(c.location)+'</td>'+
				'<td>'+esc(c.port)+'</td>'+
				'<td>'+esc(c.protocol)+'</td>'+
				'<td class="'+roleClass(c.role)+'">'+esc(c.role)+'</td>'+
				'<td>'+esc(c.channel)+'</td>'+
				'<td>'+esc(c.connected_at)+'</td>'+
				'<td>'+esc(c.uptime)+'</td>'+
				'<td>'+esc(c.last_received)+'</td>'+
				'<td><button type="button" class="action block" data-action="block" data-id="'+esc(c.id)+'" data-ip="'+esc(c.ip)+'" data-channel="'+esc(c.channel)+'" aria-label="Désactiver la connexion '+esc(c.id)+' depuis '+esc(c.ip)+(c.channel?' sur la clé '+esc(c.channel):' (aucune clé)')+'"'+busy+'>Désactiver</button></td>'+
			'</tr>';
		}
		tb.innerHTML=rows;
	}
	var bb=document.getElementById('blocked');
	if(!d.blocked||d.blocked.length===0){bb.innerHTML='<tr><td colspan="6" class="muted">Aucune machine bloquée.</td></tr>';}
	else{
		var brows='';
		for(var j=0;j<d.blocked.length;j++){
			var b=d.blocked[j];
			var bkey=b.ip+'|'+b.channel;
			var bbusy=pending['unblock:'+bkey]?' disabled':'';
			brows+='<tr>'+
				'<td>'+esc(b.ip)+'</td>'+
				'<td>'+esc(b.channel)+'</td>'+
				'<td>'+esc(b.location)+'</td>'+
				'<td>'+esc(b.client_id)+'</td>'+
				'<td>'+esc(b.blocked_at)+'</td>'+
				'<td><button type="button" class="action unblock" data-action="unblock" data-ip="'+esc(b.ip)+'" data-channel="'+esc(b.channel)+'" aria-label="Activer de nouveau la machine '+esc(b.ip)+' sur la clé '+esc(b.channel)+'"'+bbusy+'>Activer</button></td>'+
			'</tr>';
		}
		bb.innerHTML=brows;
	}
}
function act(action,key,body){
	if(pending[action+':'+key]){return;}
	pending[action+':'+key]=true;
	fetch('ADMINPATH/clients/'+action,{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:body})
	.then(function(r){return r.json().then(function(j){return {ok:r.ok,j:j};});})
	.then(function(res){setStatus(res.ok?res.j.msg:(res.j.error||'Erreur.'),!res.ok);})
	.catch(function(){setStatus('Le serveur n\'a pas répondu.',true);})
	.then(function(){delete pending[action+':'+key];});
}
document.addEventListener('click',function(e){
	var btn=e.target.closest('button[data-action]');
	if(!btn){return;}
	if(btn.getAttribute('data-action')==='block'){
		var id=btn.getAttribute('data-id'),ip=btn.getAttribute('data-ip'),ch=btn.getAttribute('data-channel');
		var q=ch?('Couper la connexion '+id+' et interdire à l\'adresse '+ip+' de rejoindre la clé '+ch+' ?'):('Couper la connexion '+id+' depuis '+ip+' ? Elle n\'a rejoint aucune clé, rien ne sera bloqué.');
		if(!window.confirm(q)){return;}
		btn.disabled=true;
		act('block',id,'id='+encodeURIComponent(id));
	}else{
		var bip=btn.getAttribute('data-ip'),bch=btn.getAttribute('data-channel');
		btn.disabled=true;
		act('unblock',bip+'|'+bch,'ip='+encodeURIComponent(bip)+'&channel='+encodeURIComponent(bch));
	}
});
var es=new EventSource('ADMINPATH/events');
es.onmessage=function(e){try{render(JSON.parse(e.data));}catch(err){}};
</script>
</body>
</html>`
