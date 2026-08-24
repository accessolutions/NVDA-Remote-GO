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
	sortKey     time.Time
}

// adminSnapshot is the JSON payload sent to the dashboard.
type adminSnapshot struct {
	Connected  int               `json:"connected"`
	Channels   int               `json:"channels"`
	Goroutines int               `json:"goroutines"`
	MemAllocMB float64           `json:"mem_alloc_mb"`
	MemSysMB   float64           `json:"mem_sys_mb"`
	MemUsedPct float64           `json:"mem_used_pct"`
	MemUsedOK  bool              `json:"mem_used_ok"`
	CPUPct     float64           `json:"cpu_pct"`
	CPUOK      bool              `json:"cpu_ok"`
	Timestamp  string            `json:"timestamp"`
	Clients    []adminClientInfo `json:"clients"`
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
			ConnectedAt: connectedAt.Format("2006-01-02 15:04:05"),
			Uptime:      formatUptime(now.Sub(connectedAt)),
			sortKey:     connectedAt,
		})
	}

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
	if adminPasswordDefined() {
		Log(LOG_INFO, "Admin dashboard enabled at "+base+" (user: "+AdminUsername+")")
	} else {
		Log(LOG_INFO, "Admin dashboard enabled at "+base+" (user: "+AdminUsername+") - password not set yet, define it at "+base+"/setup on first connection")
	}
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
.updated{color:#9aa7b4;font-size:.8rem;margin-top:1rem}
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
</tr>
</thead>
<tbody id="clients"><tr><td colspan="9" class="muted">Chargement…</td></tr></tbody>
</table>
<p class="updated">Dernière mise à jour : <span id="updated">-</span></p>
<script>
function esc(s){var d=document.createElement('div');d.textContent=s==null?'':s;return d.innerHTML;}
function roleClass(r){return r==='master'?'role-master':(r==='slave'?'role-slave':'muted');}
function render(d){
	document.getElementById('stat-connected').textContent=d.connected;
	document.getElementById('stat-mem').textContent=d.mem_used_ok?(d.mem_used_pct.toFixed(1)+' %'):'indisponible';
	document.getElementById('stat-cpu').textContent=d.cpu_ok?(d.cpu_pct.toFixed(1)+' %'):'indisponible';
	document.getElementById('updated').textContent=d.timestamp;
	var tb=document.getElementById('clients');
	if(!d.clients||d.clients.length===0){tb.innerHTML='<tr><td colspan="9" class="muted">Aucune connexion active.</td></tr>';return;}
	var rows='';
	for(var i=0;i<d.clients.length;i++){
		var c=d.clients[i];
		rows+='<tr>'+
			'<td>'+esc(c.id)+'</td>'+
			'<td>'+esc(c.ip)+'</td>'+
			'<td>'+esc(c.location)+'</td>'+
			'<td>'+esc(c.port)+'</td>'+
			'<td>'+esc(c.protocol)+'</td>'+
			'<td class="'+roleClass(c.role)+'">'+esc(c.role)+'</td>'+
			'<td>'+esc(c.channel)+'</td>'+
			'<td>'+esc(c.connected_at)+'</td>'+
			'<td>'+esc(c.uptime)+'</td>'+
		'</tr>';
	}
	tb.innerHTML=rows;
}
var es=new EventSource('ADMINPATH/events');
es.onmessage=function(e){try{render(JSON.parse(e.data));}catch(err){}};
</script>
</body>
</html>`
