package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	geoCacheTTL     = 24 * time.Hour
	geoRetryDelay   = 10 * time.Minute
	geoHTTPTimeout  = 8 * time.Second
	geoAPIFields    = "success,message,city,region,country,country_code"
	geoPendingLabel = "Recherche…"
)

type geoLocation struct {
	City        string
	Region      string
	Country     string
	CountryCode string
}

func (g geoLocation) display() string {
	parts := make([]string, 0, 3)
	if g.City != "" {
		parts = append(parts, g.City)
	}
	if g.Region != "" {
		parts = append(parts, g.Region)
	}
	if g.Country != "" {
		parts = append(parts, g.Country)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

type geoAPIResponse struct {
	Success     bool   `json:"success"`
	Message     string `json:"message"`
	City        string `json:"city"`
	Region      string `json:"region"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
}

type geoCacheEntry struct {
	location  geoLocation
	expiresAt time.Time
	retryAt   time.Time
	inFlight  bool
}

var (
	geoCacheMu sync.Mutex
	geoCache   = make(map[string]geoCacheEntry)
	geoClient  = &http.Client{Timeout: geoHTTPTimeout}
)

// requestGeoLocation starts a non-blocking lookup for a public IP address.
// Results are cached because the same client can reconnect frequently and the
// free API endpoint has a daily request limit.
func requestGeoLocation(ip string) {
	if strings.TrimSpace(geoIPAPIURL) == "" || !isPublicIP(ip) {
		return
	}

	now := time.Now()
	geoCacheMu.Lock()
	entry := geoCache[ip]
	if entry.inFlight || now.Before(entry.expiresAt) || now.Before(entry.retryAt) {
		geoCacheMu.Unlock()
		return
	}
	entry.inFlight = true
	geoCache[ip] = entry
	geoCacheMu.Unlock()

	go func() {
		location, err := lookupGeoLocation(ip)

		geoCacheMu.Lock()
		entry := geoCache[ip]
		entry.inFlight = false
		if err != nil {
			entry.location = geoLocation{}
			entry.expiresAt = time.Time{}
			entry.retryAt = time.Now().Add(geoRetryDelay)
		} else {
			entry.location = location
			entry.expiresAt = time.Now().Add(geoCacheTTL)
			entry.retryAt = time.Time{}
		}
		geoCache[ip] = entry
		geoCacheMu.Unlock()

		if err != nil {
			Log(LOG_DEBUG, "Unable to geolocate client IP "+ip+": "+err.Error())
		}
	}()
}

// geoLocationForIP returns the display value used by the admin dashboard. If
// no cached result exists, it schedules a lookup and returns a temporary label.
func geoLocationForIP(ip string) string {
	if strings.TrimSpace(geoIPAPIURL) == "" || !isPublicIP(ip) {
		return "-"
	}

	now := time.Now()
	geoCacheMu.Lock()
	entry := geoCache[ip]
	if entry.inFlight {
		geoCacheMu.Unlock()
		return geoPendingLabel
	}
	if now.Before(entry.expiresAt) {
		location := entry.location.display()
		geoCacheMu.Unlock()
		return location
	}
	if now.Before(entry.retryAt) {
		geoCacheMu.Unlock()
		return "Indisponible"
	}
	geoCacheMu.Unlock()

	requestGeoLocation(ip)
	return geoPendingLabel
}

func lookupGeoLocation(ip string) (geoLocation, error) {
	if !isPublicIP(ip) {
		return geoLocation{}, errors.New("IP address is not public")
	}
	endpoint, err := geoAPIEndpoint(ip)
	if err != nil {
		return geoLocation{}, err
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return geoLocation{}, err
	}
	resp, err := geoClient.Do(req)
	if err != nil {
		return geoLocation{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return geoLocation{}, fmt.Errorf("geolocation API returned HTTP %d", resp.StatusCode)
	}

	var payload geoAPIResponse
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		return geoLocation{}, err
	}
	if !payload.Success {
		if payload.Message == "" {
			payload.Message = "the API did not return a successful result"
		}
		return geoLocation{}, errors.New(payload.Message)
	}

	location := geoLocation{
		City:        strings.TrimSpace(payload.City),
		Region:      strings.TrimSpace(payload.Region),
		Country:     strings.TrimSpace(payload.Country),
		CountryCode: strings.TrimSpace(payload.CountryCode),
	}
	if location.display() == "-" {
		return geoLocation{}, errors.New("the API returned no location data")
	}
	return location, nil
}

func geoAPIEndpoint(ip string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(geoIPAPIURL))
	if err != nil {
		return "", fmt.Errorf("invalid geolocation API URL: %w", err)
	}
	if (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return "", errors.New("geolocation API URL must contain an HTTP or HTTPS host")
	}

	base.Path = strings.TrimRight(base.Path, "/") + "/" + url.PathEscape(ip)
	query := base.Query()
	query.Set("fields", geoAPIFields)
	query.Set("lang", "fr")
	base.RawQuery = query.Encode()
	return base.String(), nil
}

func isPublicIP(address string) bool {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil {
		return false
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate()
}
