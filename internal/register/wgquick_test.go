package register

import (
	"strings"
	"testing"
)

func TestWgQuickConfig(t *testing.T) {
	s := &State{
		Subdomain:   "brave-otter.tunnelo.io",
		PrivateKey:  "privkey=",
		ServicePort: 8096,
		WireGuard: WireGuardConfig{
			Address: "10.66.0.7/32",
			MTU:     1280,
			Peer: Peer{
				PublicKey:           "gwpubkey=",
				Endpoint:            "gw1.tunnelo.io:51820",
				AllowedIPs:          []string{"10.66.0.1/32", "fd66::1/128"},
				PersistentKeepalive: 25,
			},
		},
	}

	got := s.WgQuickConfig()
	for _, want := range []string{
		"[Interface]",
		"PrivateKey = privkey=",
		"Address = 10.66.0.7/32",
		"MTU = 1280",
		"[Peer]",
		"PublicKey = gwpubkey=",
		"Endpoint = gw1.tunnelo.io:51820",
		"AllowedIPs = 10.66.0.1/32, fd66::1/128",
		"PersistentKeepalive = 25",
		"brave-otter.tunnelo.io",
		"port 8096",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
}

func TestWgQuickConfigOmitsUnsetOptionals(t *testing.T) {
	s := &State{
		PrivateKey: "privkey=",
		WireGuard: WireGuardConfig{
			Address: "10.66.0.7/32",
			Peer:    Peer{PublicKey: "gwpubkey=", Endpoint: "gw:51820", AllowedIPs: []string{"10.66.0.1/32"}},
		},
	}
	got := s.WgQuickConfig()
	if strings.Contains(got, "MTU") || strings.Contains(got, "PersistentKeepalive") {
		t.Errorf("config contains unset optional fields:\n%s", got)
	}
}
