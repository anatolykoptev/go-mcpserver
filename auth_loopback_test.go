package mcpserver

import (
	"net/http"
	"testing"
)

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:12345", true},
		{"[::1]:12345", true},
		{"192.168.1.1:12345", false},
		{"10.0.0.1:12345", false},
	}
	for _, tt := range tests {
		r := &http.Request{RemoteAddr: tt.addr}
		got := isLoopback(r)
		if got != tt.want {
			t.Errorf("isLoopback(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}
