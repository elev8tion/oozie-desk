package hub

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"strconv"
	"time"
)

func privateIPv4() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			return ip, nil
		}
	}
	return nil, errors.New("no private network address")
}

// validateAddr accepts only a private host:port. Loopback is refused unless
// the desk is in a test that explicitly allows it.
func validateAddr(addr string, allowLoopback bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("address must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("address has no usable port")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("address must be an IP, not a name")
	}
	if ip.IsLoopback() {
		if allowLoopback {
			return nil
		}
		return errors.New("loopback addresses are not a company network")
	}
	if !ip.IsPrivate() {
		return errors.New("only private network addresses can connect")
	}
	return nil
}

func remoteAllowed(remote string, allowLoopback bool) error {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return errors.New("peer address refused")
	}
	return validateAddr(net.JoinHostPort(host, "1"), allowLoopback)
}

func (s *Service) certificate(ip net.IP) (tls.Certificate, error) {
	priv := s.key
	if priv == nil {
		return tls.Certificate{}, errors.New("desk key is not ready")
	}
	pub := priv.Public().(ed25519.PublicKey)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{ip},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

func nodeIDFromCert(cert *x509.Certificate) (string, error) {
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", errors.New("desk certificate is not an ed25519 key")
	}
	return hex.EncodeToString(pub), nil
}
