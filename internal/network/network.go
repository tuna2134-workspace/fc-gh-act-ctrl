// Package network manages the host bridge, per-VM TAP devices and IPv4
// allocation for microVMs.
package network

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"

	"github.com/vishvananda/netlink"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
)

// Lease describes the host and guest networking of one microVM.
type Lease struct {
	IP      net.IP
	Mask    net.IPMask
	Gateway net.IP
	DNS     []string
	Tap     string
}

// Manager owns the bridge and allocates one lease per running VM.
type Manager struct {
	bridgeName   string
	manageBridge bool
	mtu          int
	tapPrefix    string

	dns []string

	mu     sync.Mutex
	bridge netlink.Link
	ipam   *ipam
	seq    int
}

// New validates the network configuration and returns a Manager.
func New(cfg config.NetworkConfig) (*Manager, error) {
	_, cidr, err := net.ParseCIDR(cfg.CIDR)
	if err != nil {
		return nil, fmt.Errorf("parse network.cidr: %w", err)
	}
	gateway := net.ParseIP(cfg.Gateway).To4()
	if gateway == nil {
		return nil, fmt.Errorf("network.gateway must be IPv4: %q", cfg.Gateway)
	}
	return &Manager{
		bridgeName:   cfg.Bridge,
		manageBridge: cfg.ManageBridge,
		mtu:          cfg.MTU,
		tapPrefix:    cfg.TapPrefix,
		dns:          append([]string(nil), cfg.DNS...),
		ipam:         newIPAM(cidr, gateway),
	}, nil
}

// EnsureBridge creates the bridge (when configured to manage it) and makes sure
// it is up with the gateway address assigned.
func (m *Manager) EnsureBridge() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureBridgeLocked()
}

func (m *Manager) ensureBridgeLocked() error {
	if m.bridge != nil {
		return nil
	}
	link, err := netlink.LinkByName(m.bridgeName)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("look up bridge %s: %w", m.bridgeName, err)
		}
		if !m.manageBridge {
			return fmt.Errorf("bridge %s does not exist and network.manage_bridge is false", m.bridgeName)
		}
		attrs := netlink.NewLinkAttrs()
		attrs.Name = m.bridgeName
		attrs.MTU = m.mtu
		bridge := &netlink.Bridge{LinkAttrs: attrs}
		if err := netlink.LinkAdd(bridge); err != nil {
			return fmt.Errorf("create bridge %s: %w", m.bridgeName, err)
		}
		link, err = netlink.LinkByName(m.bridgeName)
		if err != nil {
			return fmt.Errorf("look up new bridge %s: %w", m.bridgeName, err)
		}
	}

	if m.manageBridge {
		if err := m.ensureGatewayAddrLocked(link); err != nil {
			return err
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring bridge %s up: %w", m.bridgeName, err)
	}
	m.bridge = link
	return nil
}

func (m *Manager) ensureGatewayAddrLocked(link netlink.Link) error {
	want := &net.IPNet{IP: m.ipam.gateway, Mask: m.ipam.cidr.Mask}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list bridge addresses: %w", err)
	}
	for _, addr := range addrs {
		if addr.IPNet != nil && addr.IPNet.IP.Equal(want.IP) {
			return nil
		}
	}
	if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: want}); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("assign %s to bridge %s: %w", want, m.bridgeName, err)
	}
	return nil
}

// Acquire allocates an IP and creates the TAP device attached to the bridge.
func (m *Manager) Acquire() (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureBridgeLocked(); err != nil {
		return nil, err
	}
	ip, err := m.ipam.allocate()
	if err != nil {
		return nil, err
	}
	tap := m.nextTapNameLocked()
	if err := m.createTapLocked(tap); err != nil {
		m.ipam.release(ip)
		return nil, err
	}
	return &Lease{
		IP:      ip,
		Mask:    m.ipam.cidr.Mask,
		Gateway: m.ipam.gateway,
		DNS:     append([]string(nil), m.dns...),
		Tap:     tap,
	}, nil
}

// SetDNS records the nameservers handed to guests.
func (m *Manager) SetDNS(dns []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dns = append([]string(nil), dns...)
}

// DNS returns the nameservers handed to guests.
func (m *Manager) DNS() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.dns...)
}

func (m *Manager) nextTapNameLocked() string {
	for {
		m.seq++
		name := fmt.Sprintf("%s%d", m.tapPrefix, m.seq)
		if len(name) > 15 {
			name = name[:15]
		}
		if _, err := netlink.LinkByName(name); err != nil {
			return name
		}
	}
}

func (m *Manager) createTapLocked(name string) error {
	if m.bridge == nil {
		return errors.New("bridge is not initialized")
	}
	attrs := netlink.NewLinkAttrs()
	attrs.Name = name
	attrs.MTU = m.mtu
	tap := &netlink.Tuntap{LinkAttrs: attrs, Mode: netlink.TUNTAP_MODE_TAP}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("create tap %s: %w", name, err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		_ = netlink.LinkDel(tap)
		return fmt.Errorf("look up tap %s: %w", name, err)
	}
	if err := netlink.LinkSetMaster(link, m.bridge); err != nil {
		_ = netlink.LinkDel(link)
		return fmt.Errorf("attach tap %s to bridge %s: %w", name, m.bridgeName, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		_ = netlink.LinkDel(link)
		return fmt.Errorf("bring tap %s up: %w", name, err)
	}
	return nil
}

// Release removes the TAP device and returns the IP to the pool.
func (m *Manager) Release(lease *Lease) error {
	if lease == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ipam.release(lease.IP)
	var errs []error
	if lease.Tap != "" {
		if link, err := netlink.LinkByName(lease.Tap); err == nil {
			if err := netlink.LinkDel(link); err != nil {
				errs = append(errs, fmt.Errorf("delete tap %s: %w", lease.Tap, err))
			}
		}
	}
	return errors.Join(errs...)
}

// ReleaseByName removes a TAP device by name. It is used when reaping state left
// behind by a previous controller process.
func ReleaseByName(name string) error {
	if name == "" {
		return nil
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return err
	}
	return netlink.LinkDel(link)
}

// ipam is a small IPv4 allocator for the guest subnet.
type ipam struct {
	cidr    *net.IPNet
	gateway net.IP
	used    map[string]struct{}
	next    uint32
}

func newIPAM(cidr *net.IPNet, gateway net.IP) *ipam {
	base := ipToUint32(cidr.IP.To4())
	return &ipam{
		cidr:    cidr,
		gateway: gateway,
		used:    make(map[string]struct{}),
		next:    base + 1,
	}
}

func (p *ipam) allocate() (net.IP, error) {
	base := ipToUint32(p.cidr.IP.To4())
	mask := ipToUint32(net.IP(p.cidr.Mask))
	broadcast := base | ^mask

	first := base + 2 // base+1 is the gateway
	if first < p.next {
		first = p.next
	}
	candidate := first
	for candidate < broadcast {
		ip := uint32ToIP(candidate)
		if _, taken := p.used[ip.String()]; !taken && !ip.Equal(p.gateway) {
			p.used[ip.String()] = struct{}{}
			p.next = candidate + 1
			return ip, nil
		}
		candidate++
	}
	// Wrap around once before giving up.
	for candidate = base + 2; candidate < broadcast; candidate++ {
		ip := uint32ToIP(candidate)
		if _, taken := p.used[ip.String()]; !taken && !ip.Equal(p.gateway) {
			p.used[ip.String()] = struct{}{}
			p.next = candidate + 1
			return ip, nil
		}
	}
	return nil, fmt.Errorf("no free IP addresses in %s", p.cidr)
}

func (p *ipam) release(ip net.IP) {
	if ip == nil {
		return
	}
	delete(p.used, ip.String())
}

func ipToUint32(ip net.IP) uint32 {
	v := ip.To4()
	return uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
}

func uint32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// ReapTaps removes every TAP device using the configured prefix. It must only
// be called when no VM owned by this controller is running (for example at
// startup, after reaping VM state).
func (m *Manager) ReapTaps() error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}
	var errs []error
	for _, link := range links {
		name := link.Attrs().Name
		if !strings.HasPrefix(name, m.tapPrefix) {
			continue
		}
		if err := netlink.LinkDel(link); err != nil {
			errs = append(errs, fmt.Errorf("delete tap %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
