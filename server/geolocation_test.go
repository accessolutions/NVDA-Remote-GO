package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGeoLocationDisplay(t *testing.T) {
	location := geoLocation{
		City:    "Lille",
		Region:  "Hauts-de-France",
		Country: "France",
	}
	if got, want := location.display(), "Lille, Hauts-de-France, France"; got != want {
		t.Fatalf("geoLocation.display() = %q, want %q", got, want)
	}
	if got := (geoLocation{}).display(); got != "-" {
		t.Fatalf("empty geoLocation.display() = %q, want %q", got, "-")
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, address := range []string{"8.8.8.8", "2001:4860:4860::8888"} {
		if !isPublicIP(address) {
			t.Errorf("isPublicIP(%q) = false, want true", address)
		}
	}
	for _, address := range []string{"192.168.1.10", "10.0.0.1", "127.0.0.1", "::1", "not-an-ip"} {
		if isPublicIP(address) {
			t.Errorf("isPublicIP(%q) = true, want false", address)
		}
	}
}

func TestLookupGeoLocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/8.8.8.8" {
			t.Errorf("request path = %q, want %q", r.URL.Path, "/8.8.8.8")
		}
		query := r.URL.Query()
		if query.Get("lang") != "fr" {
			t.Errorf("lang query = %q, want %q", query.Get("lang"), "fr")
		}
		if !strings.Contains(query.Get("fields"), "city") {
			t.Errorf("fields query = %q, want city", query.Get("fields"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"city":"Lille","region":"Hauts-de-France","country":"France","country_code":"FR"}`))
	}))
	defer server.Close()

	oldURL := geoIPAPIURL
	oldClient := geoClient
	defer func() {
		geoIPAPIURL = oldURL
		geoClient = oldClient
	}()
	geoIPAPIURL = server.URL
	geoClient = server.Client()

	location, err := lookupGeoLocation("8.8.8.8")
	if err != nil {
		t.Fatalf("lookupGeoLocation() returned error: %v", err)
	}
	if got, want := location.display(), "Lille, Hauts-de-France, France"; got != want {
		t.Fatalf("lookupGeoLocation().display() = %q, want %q", got, want)
	}
}

func TestGeoAPIEndpoint(t *testing.T) {
	oldURL := geoIPAPIURL
	defer func() { geoIPAPIURL = oldURL }()
	geoIPAPIURL = "https://ipwho.is/"

	endpoint, err := geoAPIEndpoint("2001:4860:4860::8888")
	if err != nil {
		t.Fatalf("geoAPIEndpoint() returned error: %v", err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("url.Parse() returned error: %v", err)
	}
	if parsed.Host != "ipwho.is" {
		t.Fatalf("endpoint host = %q, want %q", parsed.Host, "ipwho.is")
	}
	if parsed.Query().Get("lang") != "fr" {
		t.Fatalf("endpoint lang query = %q, want %q", parsed.Query().Get("lang"), "fr")
	}
}
