package server

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type historyMetricView struct {
	Average string
	Maximum string
}

type historyPeriodView struct {
	Label              string
	DetailURL          string
	TotalConnections   int
	CumulativeDuration string
	AverageConcurrent  string
	UniqueIPs          int
	SystemCPU          historyMetricView
	ProcessCPU         historyMetricView
	MemoryUsed         historyMetricView
	ProcessMemory      historyMetricView
}

type historyPageData struct {
	BasePath string
	View     string
	Title    string
	Date     string
	Periods  []historyPeriodView
}

type historyConnectionView struct {
	ClientID  int
	IP        string
	Role      string
	Channel   string
	Started   string
	Ended     string
	Duration  string
}

type historyHourView struct {
	Hour              string
	ActiveConnections int
	Duration          string
	AverageConcurrent string
	SystemCPU         historyMetricView
	ProcessCPU        historyMetricView
	MemoryUsed        historyMetricView
	ProcessMemory     historyMetricView
}

type historyDetailPageData struct {
	BasePath     string
	Date         string
	Label        string
	Stats        historyPeriodView
	Hours        []historyHourView
	Connections  []historyConnectionView
}

var adminHistoryTemplate = template.Must(template.New("admin-history").Parse(`<!DOCTYPE html>
<html lang="fr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>NVDA REMOTE GO Accessolutions - {{if .Date}}Détails du {{.Label}}{{else}}Historique{{end}}</title>
<style>
:root{color-scheme:dark}
body{font-family:system-ui,Arial,sans-serif;background:#0f1720;color:#e6edf3;margin:0;padding:1.5rem}
header{display:flex;align-items:flex-start;justify-content:space-between;gap:1rem;flex-wrap:wrap}
h1{font-size:1.4rem;margin:.3rem 0 0}
a{color:#93c5fd;text-decoration:none}a:hover{text-decoration:underline}
nav{display:flex;gap:.5rem;flex-wrap:wrap;margin:1.25rem 0}
nav a{background:#1d2836;border:1px solid #334155;border-radius:7px;padding:.5rem .8rem}
nav a.active{background:#2563eb;border-color:#2563eb;color:#fff}
.card-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:.75rem;margin:1.25rem 0}
.card{background:#161f2b;border:1px solid #263243;border-radius:9px;padding:.8rem 1rem}
.card .label{color:#9aa7b4;font-size:.82rem;display:block;margin-bottom:.25rem}
.card .value{font-size:1.12rem;font-weight:700}
.table-wrap{overflow-x:auto;margin:1rem 0}
table{width:100%;border-collapse:collapse;background:#161f2b;border-radius:10px;overflow:hidden;min-width:850px}
th,td{padding:.6rem .7rem;text-align:left;border-bottom:1px solid #263243;font-size:.88rem;vertical-align:top}
th{background:#1d2836;white-space:nowrap}
tr:hover td{background:#1b2534}
.muted{color:#9aa7b4}
.updated{color:#9aa7b4;font-size:.82rem;margin-top:1rem}
.back{font-size:.9rem}
@media(max-width:700px){body{padding:1rem}h1{font-size:1.2rem}}
</style>
</head>
<body>
<header>
<div><a class="back" href="{{.BasePath}}">← Tableau de bord</a><h1>{{if .Date}}Détails du {{.Label}}{{else}}{{.Title}}{{end}}</h1></div>
<a href="{{.BasePath}}/logout">Se déconnecter</a>
</header>
{{if .Date}}
<nav aria-label="Période"><a href="{{.BasePath}}/history?view=day">Jours</a><a href="{{.BasePath}}/history?view=month">Mois</a><a href="{{.BasePath}}/history?view=year">Années</a></nav>
<section class="card-grid">
<div class="card"><span class="label">Connexions actives</span><span class="value">{{.Stats.TotalConnections}}</span></div>
<div class="card"><span class="label">Durée cumulée</span><span class="value">{{.Stats.CumulativeDuration}}</span></div>
<div class="card"><span class="label">Moyenne simultanée</span><span class="value">{{.Stats.AverageConcurrent}}</span></div>
<div class="card"><span class="label">Adresses IP uniques</span><span class="value">{{.Stats.UniqueIPs}}</span></div>
<div class="card"><span class="label">CPU système moyen / maximum</span><span class="value">{{.Stats.SystemCPU.Average}} / {{.Stats.SystemCPU.Maximum}}</span></div>
<div class="card"><span class="label">CPU serveur moyen / maximum</span><span class="value">{{.Stats.ProcessCPU.Average}} / {{.Stats.ProcessCPU.Maximum}}</span></div>
<div class="card"><span class="label">Mémoire système moyenne / maximum</span><span class="value">{{.Stats.MemoryUsed.Average}} / {{.Stats.MemoryUsed.Maximum}}</span></div>
<div class="card"><span class="label">Mémoire Go moyenne / maximum</span><span class="value">{{.Stats.ProcessMemory.Average}} / {{.Stats.ProcessMemory.Maximum}}</span></div>
</section>
<h2>Statistiques heure par heure</h2>
<div class="table-wrap"><table>
<thead><tr><th>Heure</th><th>Connexions actives</th><th>Durée cumulée</th><th>Moyenne simultanée</th><th>CPU système moy. / max.</th><th>CPU serveur moy. / max.</th><th>Mémoire système moy. / max.</th><th>Mémoire Go moy. / max.</th></tr></thead>
<tbody>{{range .Hours}}<tr><td>{{.Hour}}</td><td>{{.ActiveConnections}}</td><td>{{.Duration}}</td><td>{{.AverageConcurrent}}</td><td>{{.SystemCPU.Average}} / {{.SystemCPU.Maximum}}</td><td>{{.ProcessCPU.Average}} / {{.ProcessCPU.Maximum}}</td><td>{{.MemoryUsed.Average}} / {{.MemoryUsed.Maximum}}</td><td>{{.ProcessMemory.Average}} / {{.ProcessMemory.Maximum}}</td></tr>{{end}}</tbody>
</table></div>
<h2>Connexions de la journée</h2>
<div class="table-wrap"><table>
<thead><tr><th>ID client</th><th>Adresse IP</th><th>Rôle</th><th>Canal</th><th>Début</th><th>Fin</th><th>Durée dans la journée</th></tr></thead>
<tbody>{{if .Connections}}{{range .Connections}}<tr><td>{{.ClientID}}</td><td>{{.IP}}</td><td>{{.Role}}</td><td>{{.Channel}}</td><td>{{.Started}}</td><td>{{.Ended}}</td><td>{{.Duration}}</td></tr>{{end}}{{else}}<tr><td colspan="7" class="muted">Aucune connexion ce jour-là.</td></tr>{{end}}</tbody>
</table></div>
{{else}}
<nav aria-label="Période"><a class="{{if eq .View "day"}}active{{end}}" href="{{.BasePath}}/history?view=day">Jours</a><a class="{{if eq .View "month"}}active{{end}}" href="{{.BasePath}}/history?view=month">Mois</a><a class="{{if eq .View "year"}}active{{end}}" href="{{.BasePath}}/history?view=year">Années</a></nav>
<div class="table-wrap"><table>
<thead><tr><th>Période</th><th>Connexions actives</th><th>Durée cumulée</th><th>Moyenne simultanée</th><th>IP uniques</th><th>CPU système moy. / max.</th><th>CPU serveur moy. / max.</th><th>Mémoire système moy. / max.</th><th>Mémoire Go moy. / max.</th></tr></thead>
<tbody>{{if .Periods}}{{range .Periods}}<tr><td>{{if .DetailURL}}<a href="{{.DetailURL}}">{{.Label}}</a>{{else}}{{.Label}}{{end}}</td><td>{{.TotalConnections}}</td><td>{{.CumulativeDuration}}</td><td>{{.AverageConcurrent}}</td><td>{{.UniqueIPs}}</td><td>{{.SystemCPU.Average}} / {{.SystemCPU.Maximum}}</td><td>{{.ProcessCPU.Average}} / {{.ProcessCPU.Maximum}}</td><td>{{.MemoryUsed.Average}} / {{.MemoryUsed.Maximum}}</td><td>{{.ProcessMemory.Average}} / {{.ProcessMemory.Maximum}}</td></tr>{{end}}{{else}}<tr><td colspan="9" class="muted">Aucune donnée historique.</td></tr>{{end}}</tbody>
</table></div>
<p class="updated">Les périodes utilisent l’heure locale Europe/Paris. Les données sont conservées sans limite automatique.</p>
{{end}}
</body>
</html>`))

func handleAdminHistory(w http.ResponseWriter, r *http.Request) {
	if !adminRequestAuthorized(r) {
		http.Redirect(w, r, adminPath+"/login", http.StatusSeeOther)
		return
	}
	if r.URL.Path != adminPath+"/history" {
		http.NotFound(w, r)
		return
	}
	store := currentAdminHistory()
	if store == nil {
		http.Error(w, "Historique indisponible", http.StatusServiceUnavailable)
		return
	}
	view := r.URL.Query().Get("view")
	if view != "month" && view != "year" {
		view = "day"
	}
	periods, err := store.historyPeriods(view)
	if err != nil {
		http.Error(w, "Impossible de lire l'historique", http.StatusInternalServerError)
		Log(LOG_DEBUG, "Unable to read administration history: "+err.Error())
		return
	}
	data := historyPageData{BasePath: adminPath, View: view, Title: historyViewTitle(view)}
	for _, period := range periods {
		data.Periods = append(data.Periods, historyPeriodViewFor(period, view))
	}
	writeAdminHistoryPage(w, data)
}

func handleAdminHistoryDay(w http.ResponseWriter, r *http.Request) {
	if !adminRequestAuthorized(r) {
		http.Redirect(w, r, adminPath+"/login", http.StatusSeeOther)
		return
	}
	if r.URL.Path != adminPath+"/history/day" {
		http.NotFound(w, r)
		return
	}
	date := r.URL.Query().Get("date")
	store := currentAdminHistory()
	if store == nil {
		http.Error(w, "Historique indisponible", http.StatusServiceUnavailable)
		return
	}
	period, connections, samples, err := store.dayData(date)
	if err != nil {
		http.Error(w, "Date invalide", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	data := historyDetailPageData{
		BasePath: adminPath,
		Date:     date,
		Label:    period.Label,
		Stats:    historyPeriodViewFor(period, "day"),
	}
	for _, hour := range buildHistoryHours(period.Start, connections, samples, now) {
		data.Hours = append(data.Hours, historyHourViewFor(hour))
	}
	for _, connection := range connections {
		from := connection.Started
		if from.Before(period.Start.UTC()) {
			from = period.Start.UTC()
		}
		to := connection.Ended
		if to.After(period.End.UTC()) {
			to = period.End.UTC()
		}
		if !to.After(from) {
			continue
		}
		ended := connection.Ended.In(store.location).Format("02/01/2006 15:04:05")
		if !connection.EndedKnown {
			ended = "En cours"
		}
		data.Connections = append(data.Connections, historyConnectionView{
			ClientID: connection.ClientID,
			IP:       displayHistoryValue(connection.IP),
			Role:     displayHistoryValue(roleLabel(connection.Type)),
			Channel:  displayHistoryValue(connection.Channel),
			Started:  connection.Started.In(store.location).Format("02/01/2006 15:04:05"),
			Ended:    ended,
			Duration: formatHistoryDuration(to.Sub(from)),
		})
	}
	writeAdminHistoryPage(w, data)
}

func writeAdminHistoryPage(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := adminHistoryTemplate.Execute(w, data); err != nil {
		Log(LOG_DEBUG, "Unable to render administration history: "+err.Error())
	}
}

func historyViewTitle(view string) string {
	switch view {
	case "month":
		return "Historique par mois"
	case "year":
		return "Historique par année"
	default:
		return "Historique par jour"
	}
}

func historyPeriodViewFor(period *historyPeriod, view string) historyPeriodView {
	detailURL := ""
	if view == "day" {
		detailURL = adminPath + "/history/day?date=" + period.Key
	}
	return historyPeriodView{
		Label:              period.Label,
		DetailURL:          detailURL,
		TotalConnections:   period.TotalConnections,
		CumulativeDuration: formatHistoryDuration(time.Duration(period.DurationSeconds * float64(time.Second))),
		AverageConcurrent:  formatHistoryNumber(period.AverageConcurrence),
		UniqueIPs:          len(period.UniqueIPs),
		SystemCPU:          historyMetricViewFor(period.SystemCPU, "%"),
		ProcessCPU:         historyMetricViewFor(period.ProcessCPU, "%"),
		MemoryUsed:         historyMetricViewFor(period.MemoryUsed, "%"),
		ProcessMemory:      historyMetricViewFor(period.ProcessMemory, "Mo"),
	}
}

func historyHourViewFor(hour historyHour) historyHourView {
	return historyHourView{
		Hour:              hour.Start.Format("15:04"),
		ActiveConnections: hour.ActiveConnections,
		Duration:          formatHistoryDuration(time.Duration(hour.DurationSeconds * float64(time.Second))),
		AverageConcurrent: formatHistoryNumber(hour.AverageConcurrence),
		SystemCPU:         historyMetricViewFor(hour.SystemCPU, "%"),
		ProcessCPU:        historyMetricViewFor(hour.ProcessCPU, "%"),
		MemoryUsed:        historyMetricViewFor(hour.MemoryUsed, "%"),
		ProcessMemory:     historyMetricViewFor(hour.ProcessMemory, "Mo"),
	}
}

func historyMetricViewFor(metric historyMetric, suffix string) historyMetricView {
	if metric.count == 0 {
		return historyMetricView{Average: "Indisponible", Maximum: "Indisponible"}
	}
	return historyMetricView{
		Average: formatHistoryMetric(metric.average(), suffix),
		Maximum: formatHistoryMetric(metric.max, suffix),
	}
}

func formatHistoryMetric(value float64, suffix string) string {
	return fmt.Sprintf("%.1f %s", value, suffix)
}

func formatHistoryNumber(value float64) string {
	return fmt.Sprintf("%.2f", value)
}

func formatHistoryDuration(duration time.Duration) string {
	if duration <= 0 {
		return "0 s"
	}
	duration = duration.Round(time.Second)
	days := duration / (24 * time.Hour)
	duration %= 24 * time.Hour
	hours := duration / time.Hour
	duration %= time.Hour
	minutes := duration / time.Minute
	seconds := (duration % time.Minute) / time.Second
	parts := make([]string, 0, 4)
	if days > 0 {
		parts = append(parts, strconv.FormatInt(int64(days), 10)+" j")
	}
	if hours > 0 {
		parts = append(parts, strconv.FormatInt(int64(hours), 10)+" h")
	}
	if minutes > 0 {
		parts = append(parts, strconv.FormatInt(int64(minutes), 10)+" min")
	}
	if seconds > 0 || len(parts) == 0 {
		parts = append(parts, strconv.FormatInt(int64(seconds), 10)+" s")
	}
	return strings.Join(parts, " ")
}

func displayHistoryValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
