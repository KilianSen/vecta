// Package netguard decides which backend addresses the gateway may dial.
// Owners register addresses through the API, so without a policy a token
// holder could make the gateway connect to its own loopback, cloud metadata
// endpoints or arbitrary internal hosts.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

var metadataAddr = netip.MustParseAddr("169.254.169.254")

// Policy restricts dial targets. The zero value denies loopback, link-local,
// unspecified and multicast addresses and allows everything else.
type Policy struct {
	// Allowed, if non-empty, is the only set of networks that may be dialed.
	Allowed []netip.Prefix
	// AllowLoopback permits 127.0.0.0/8 and ::1 (single-host setups).
	AllowLoopback bool
}

// ParsePrefixes parses CIDRs or bare IPs.
func ParsePrefixes(list []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("netguard: %q is not a CIDR or IP", s)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// CheckIP returns an error if ip may not be dialed.
func (p Policy) CheckIP(ip netip.Addr) error {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid():
		return errors.New("invalid address")
	case ip.IsUnspecified():
		return fmt.Errorf("%s is unspecified", ip)
	case ip.IsLoopback() && !p.AllowLoopback:
		return fmt.Errorf("%s is loopback", ip)
	case ip == metadataAddr || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return fmt.Errorf("%s is link-local", ip)
	case ip.IsMulticast() || ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("%s is multicast", ip)
	}
	if len(p.Allowed) == 0 {
		return nil
	}
	for _, pre := range p.Allowed {
		if pre.Contains(ip) {
			return nil
		}
	}
	return fmt.Errorf("%s is outside the allowed networks", ip)
}

// Resolve resolves host:port and returns the allowed addresses. It fails if
// any resolved address is disallowed, so a hostname cannot mix a permitted
// and a forbidden answer.
func (p Policy) Resolve(ctx context.Context, address string) ([]netip.AddrPort, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("netguard: bad port in %q", address)
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("netguard: %s did not resolve", host)
	}
	out := make([]netip.AddrPort, 0, len(ips))
	for _, ip := range ips {
		if err := p.CheckIP(ip); err != nil {
			return nil, fmt.Errorf("netguard: %s: %w", address, err)
		}
		out = append(out, netip.AddrPortFrom(ip.Unmap(), uint16(port)))
	}
	return out, nil
}

// DialContext resolves address under the policy and dials the first address
// that accepts, so DNS answers cannot change between check and connect.
func (p Policy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	addrs, err := p.Resolve(ctx, address)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	var last error
	for _, a := range addrs {
		conn, err := d.DialContext(ctx, network, a.String())
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}
