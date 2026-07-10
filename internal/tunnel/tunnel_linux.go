package tunnel

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/ipc"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// linuxDevice drives either a kernel WireGuard interface or an embedded
// wireguard-go device. Both are configured and inspected uniformly through
// wgctrl (the userspace device exposes the same UAPI socket the kernel
// tooling uses).
type linuxDevice struct {
	name string
	cfg  Config
	wg   *wgctrl.Client

	// userspace mode only
	usDev  *device.Device
	usUAPI net.Listener
}

// openDevice creates and configures the tunnel interface, preferring the
// kernel module and falling back to userspace wireguard-go when it is
// unavailable. It returns the device and the mode it came up in.
func openDevice(cfg Config, log *slog.Logger) (wgDevice, string, error) {
	d := &linuxDevice{name: cfg.InterfaceName, cfg: cfg}

	// Remove any leftover interface from a previous run so we always start
	// from a known state.
	if link, err := netlink.LinkByName(d.name); err == nil {
		if err := netlink.LinkDel(link); err != nil {
			return nil, "", fmt.Errorf("removing stale interface %s: %w", d.name, err)
		}
	}

	mode := "kernel"
	if cfg.ForceUserspace {
		mode = "userspace"
	} else {
		err := netlink.LinkAdd(&netlink.Wireguard{
			LinkAttrs: netlink.LinkAttrs{Name: d.name, MTU: cfg.MTU},
		})
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) {
			log.Info("kernel WireGuard unavailable, using userspace wireguard-go")
			mode = "userspace"
		} else if err != nil {
			return nil, "", fmt.Errorf("creating kernel wireguard interface: %w", err)
		}
	}

	if mode == "userspace" {
		if err := d.startUserspace(log); err != nil {
			return nil, "", err
		}
	}

	if err := d.finishSetup(); err != nil {
		d.Close()
		return nil, "", err
	}
	return d, mode, nil
}

// startUserspace runs an embedded wireguard-go device on a TUN interface and
// exposes its UAPI socket so wgctrl can manage it like a kernel device.
func (d *linuxDevice) startUserspace(log *slog.Logger) error {
	tunDev, err := tun.CreateTUN(d.name, d.cfg.MTU)
	if err != nil {
		return fmt.Errorf("creating TUN device (is /dev/net/tun available?): %w", err)
	}

	wgLog := &device.Logger{
		Verbosef: func(string, ...any) {}, // wireguard-go is chatty; errors only
		Errorf: func(format string, args ...any) {
			log.Error("wireguard-go: " + fmt.Sprintf(format, args...))
		},
	}
	d.usDev = device.NewDevice(tunDev, conn.NewDefaultBind(), wgLog)

	uapiFile, err := ipc.UAPIOpen(d.name)
	if err != nil {
		d.usDev.Close()
		return fmt.Errorf("opening UAPI socket: %w", err)
	}
	d.usUAPI, err = ipc.UAPIListen(d.name, uapiFile)
	if err != nil {
		d.usDev.Close()
		return fmt.Errorf("listening on UAPI socket: %w", err)
	}
	go func() {
		for {
			c, err := d.usUAPI.Accept()
			if err != nil {
				return // listener closed on teardown
			}
			go d.usDev.IpcHandle(c)
		}
	}()

	if err := d.usDev.Up(); err != nil {
		d.Close()
		return fmt.Errorf("bringing userspace device up: %w", err)
	}
	return nil
}

// finishSetup assigns the tunnel address, brings the link up, and applies
// the WireGuard configuration.
func (d *linuxDevice) finishSetup() error {
	link, err := netlink.LinkByName(d.name)
	if err != nil {
		return fmt.Errorf("looking up interface %s: %w", d.name, err)
	}
	addr, err := netlink.ParseAddr(d.cfg.Address)
	if err != nil {
		return fmt.Errorf("parsing tunnel address %q: %w", d.cfg.Address, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("assigning address %s: %w", d.cfg.Address, err)
	}
	if d.cfg.MTU > 0 {
		if err := netlink.LinkSetMTU(link, d.cfg.MTU); err != nil {
			return fmt.Errorf("setting MTU %d: %w", d.cfg.MTU, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bringing link up: %w", err)
	}

	d.wg, err = wgctrl.New()
	if err != nil {
		return fmt.Errorf("opening wgctrl: %w", err)
	}
	if err := d.configure(); err != nil {
		return err
	}
	return d.ensureRoutes()
}

// ensureRoutes installs kernel routes for the peer's allowed IPs. The
// tunnel address is a /32, which creates no connected route — without
// these, replies to gateway-originated traffic (the proxy dialing our
// forwarder) leave via the default route instead of the tunnel, and the
// gateway sees dial timeouts on an otherwise healthy handshake.
func (d *linuxDevice) ensureRoutes() error {
	link, err := netlink.LinkByName(d.name)
	if err != nil {
		return fmt.Errorf("looking up interface %s: %w", d.name, err)
	}
	for _, cidr := range d.cfg.Peer.AllowedIPs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("parsing allowed IP %q: %w", cidr, err)
		}
		route := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       ipNet,
			Scope:     netlink.SCOPE_LINK,
		}
		if err := netlink.RouteAdd(route); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("adding route %s dev %s: %w", cidr, d.name, err)
		}
	}
	return nil
}

// configure (re)applies the full WireGuard config, resolving the gateway
// endpoint hostname as a side effect.
func (d *linuxDevice) configure() error {
	privateKey, err := wgtypes.ParseKey(d.cfg.PrivateKey)
	if err != nil {
		return fmt.Errorf("parsing private key: %w", err)
	}
	peerKey, err := wgtypes.ParseKey(d.cfg.Peer.PublicKey)
	if err != nil {
		return fmt.Errorf("parsing peer public key: %w", err)
	}
	endpoint, err := net.ResolveUDPAddr("udp", d.cfg.Peer.Endpoint)
	if err != nil {
		return fmt.Errorf("resolving gateway endpoint %q: %w", d.cfg.Peer.Endpoint, err)
	}
	allowed := make([]net.IPNet, 0, len(d.cfg.Peer.AllowedIPs))
	for _, cidr := range d.cfg.Peer.AllowedIPs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("parsing allowed IP %q: %w", cidr, err)
		}
		allowed = append(allowed, *ipNet)
	}
	keepalive := d.cfg.Peer.PersistentKeepalive
	if keepalive <= 0 {
		keepalive = 25 * time.Second
	}

	err = d.wg.ConfigureDevice(d.name, wgtypes.Config{
		PrivateKey:   &privateKey,
		ReplacePeers: true,
		Peers: []wgtypes.PeerConfig{{
			PublicKey:                   peerKey,
			Endpoint:                    endpoint,
			AllowedIPs:                  allowed,
			PersistentKeepaliveInterval: &keepalive,
			ReplaceAllowedIPs:           true,
		}},
	})
	if err != nil {
		return fmt.Errorf("configuring wireguard device: %w", err)
	}
	return nil
}

func (d *linuxDevice) RefreshPeer() error {
	if err := d.configure(); err != nil {
		return err
	}
	return d.ensureRoutes()
}

func (d *linuxDevice) Stats() (Status, error) {
	dev, err := d.wg.Device(d.name)
	if err != nil {
		return Status{}, fmt.Errorf("querying device %s: %w", d.name, err)
	}
	var st Status
	for _, p := range dev.Peers {
		st.LastHandshake = p.LastHandshakeTime
		st.RxBytes = p.ReceiveBytes
		st.TxBytes = p.TransmitBytes
	}
	return st, nil
}

func (d *linuxDevice) Close() error {
	var errs []error
	if d.wg != nil {
		errs = append(errs, d.wg.Close())
	}
	if d.usUAPI != nil {
		errs = append(errs, d.usUAPI.Close())
	}
	if d.usDev != nil {
		d.usDev.Close() // also removes the TUN interface
	} else if link, err := netlink.LinkByName(d.name); err == nil {
		errs = append(errs, netlink.LinkDel(link))
	}
	return errors.Join(errs...)
}
