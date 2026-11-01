package atlas

import (
	"testing"
	"time"
)

func TestAddrPrecedence(t *testing.T) {
	tests := []struct {
		name string
		port string
		opts []Option
		want string
	}{
		{name: "default", want: ":8080"},
		{name: "PORT env", port: "9000", want: ":9000"},
		{name: "option beats env", port: "9000", opts: []Option{WithAddr("127.0.0.1:7000")}, want: "127.0.0.1:7000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PORT", tt.port)
			if got := newConfig(tt.opts).addr; got != tt.want {
				t.Errorf("addr = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShutdownDelayDefault(t *testing.T) {
	tests := []struct {
		name    string
		k8sHost string
		opts    []Option
		want    time.Duration
	}{
		{name: "local", want: 0},
		{name: "kubernetes", k8sHost: "10.0.0.1", want: 5 * time.Second},
		{name: "option beats kubernetes", k8sHost: "10.0.0.1", opts: []Option{WithShutdownDelay(time.Second)}, want: time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KUBERNETES_SERVICE_HOST", tt.k8sHost)
			if got := newConfig(tt.opts).shutdownDelay; got != tt.want {
				t.Errorf("shutdownDelay = %v, want %v", got, tt.want)
			}
		})
	}
}
