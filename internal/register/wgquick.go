package register

import (
	"fmt"
	"strings"
)

// Tunnel modes sent at registration.
const (
	// TunnelManaged: the agent creates and supervises the WireGuard
	// interface itself (default).
	TunnelManaged = "managed"
	// TunnelExternal: the user carries the Tunnelo peer on WireGuard they
	// already run; the agent only registers, monitors Jellyfin, and renders
	// the config below.
	TunnelExternal = "external"
)

// WgQuickConfig renders the registration as a standard wg-quick file for
// users who bring their own WireGuard (routers, wg-quick, existing
// containers). It contains the private key, so treat it like one.
func (s *State) WgQuickConfig() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Tunnelo peer for %s\n", s.Subdomain)
	b.WriteString("# Add this to your existing WireGuard setup (wg-quick, router, container).\n")
	fmt.Fprintf(&b, "# The gateway routes your subdomain to %s port %d — make sure\n",
		strings.Split(s.WireGuard.Address, "/")[0], s.ServicePort)
	b.WriteString("# traffic arriving there reaches your Jellyfin server.\n\n")

	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", s.PrivateKey)
	fmt.Fprintf(&b, "Address = %s\n", s.WireGuard.Address)
	if s.WireGuard.MTU > 0 {
		fmt.Fprintf(&b, "MTU = %d\n", s.WireGuard.MTU)
	}

	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", s.WireGuard.Peer.PublicKey)
	fmt.Fprintf(&b, "Endpoint = %s\n", s.WireGuard.Peer.Endpoint)
	fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(s.WireGuard.Peer.AllowedIPs, ", "))
	if s.WireGuard.Peer.PersistentKeepalive > 0 {
		fmt.Fprintf(&b, "PersistentKeepalive = %d\n", s.WireGuard.Peer.PersistentKeepalive)
	}
	return b.String()
}
