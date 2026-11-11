package mcpclient

import (
	"net/url"
	"testing"
)

func TestBearerTransport_SameOrigin(t *testing.T) {
	endpoint, _ := url.Parse("https://example.com/mcp")
	bt := &bearerTransport{token: "t", origin: endpoint}
	rows := []struct {
		name, url string
		send      bool
	}{
		{"explicit default port", "https://example.com:443/mcp", true},
		{"host case", "https://EXAMPLE.com/mcp", true},
		{"userinfo decoy before real host", "https://evil.com@example.com/mcp", true},
		{"scheme downgrade", "http://example.com/mcp", false},
		{"scheme differs, same explicit port", "http://example.com:443/mcp", false},
		{"other port", "https://example.com:8443/mcp", false},
		{"subdomain", "https://sub.example.com/mcp", false},
		{"userinfo real host is evil", "https://example.com@evil.com/mcp", false},
		{"trailing dot", "https://example.com./mcp", false},
	}
	for _, r := range rows {
		u, err := url.Parse(r.url)
		if err != nil {
			t.Fatal(err)
		}
		if got := bt.sameOrigin(u); got != r.send {
			t.Errorf("%s: sameOrigin(%s) = %v, want %v", r.name, r.url, got, r.send)
		}
	}
}
