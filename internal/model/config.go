package model

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

func defaultHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ValidatePublicURL validates and normalizes an optional external/reverse-proxy Web UI URL.
// Empty input is allowed and clears the setting. Trailing slashes are trimmed.
func ValidatePublicURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("public_url must be a valid http(s)://host[:port][/path] URL without credentials, query, or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

// ValidateEndpoint ensures the model endpoint uses HTTPS or loopback HTTP without
// embedded credentials, query strings, or URL fragments.
func ValidateEndpoint(value string) (*url.URL, error) {
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("expected a model endpoint URL without credentials, query, or fragment")
	}
	host := endpoint.Hostname()
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && loopback) {
		return nil, errors.New("model endpoint must use HTTPS or loopback HTTP")
	}
	return endpoint, nil
}

// FromEnv constructs optional external model providers from environment variables.
func FromEnv() (Providers, error) {
	return buildProviders(configFromEnv(), defaultHTTPClient())
}
