// Package tlscert builds the TLS material this service serves with.
//
// Two modes are supported:
//
//	files        certificates supplied by the operator (company CA, Let's Encrypt, …)
//	self-signed  a CA and a leaf certificate generated on first start
//
// The self-signed mode exists so an intranet docker client can talk to the mirror
// over HTTPS without editing daemon.json: the client only drops the exported CA
// into /etc/docker/certs.d/<host:port>/ca.crt. The generated CA is stable — the
// leaf is re-signed in place when the set of addresses changes (e.g. a new NIC),
// which keeps already installed CA copies valid.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Mode names reported by the API and the UI.
const (
	ModeSelfSigned = "self-signed"
	ModeFiles      = "files"
)

// File names inside Options.Dir.
const (
	caCertFile     = "ca.pem"
	caKeyFile      = "ca.key"
	serverCertFile = "server.pem"
	serverKeyFile  = "server.key"
)

// Defaults for generated certificates.
const (
	DefaultValidity = 825 * 24 * time.Hour // 2 years, like the public CAs
	caValidity      = 10 * 365 * 24 * time.Hour
	renewBefore     = 30 * 24 * time.Hour
)

// Info is the read-only description of the served TLS configuration. It is
// exposed through GET /api/mirror/stats so the UI can render the exact client
// instructions (HTTPS address plus where the CA has to be installed).
type Info struct {
	Enabled  bool      `json:"enabled"`
	Mode     string    `json:"mode,omitempty"`
	SANs     []string  `json:"sans,omitempty"`
	NotAfter time.Time `json:"notAfter,omitempty"`
	// CAPEM is the CA certificate in PEM form, empty when the operator supplied
	// the certificates (we do not know their CA file then).
	CAPEM []byte `json:"-"`
}

// Material is everything the HTTP server needs to serve TLS.
type Material struct {
	Info
	CertFile string
	KeyFile  string
}

// Options configures the self-signed generator.
type Options struct {
	// Dir keeps the generated files; it is created when missing.
	Dir string
	// Hosts are extra DNS names / IPs to put into the certificate SANs.
	Hosts []string
	// Validity of the leaf certificate, defaults to DefaultValidity.
	Validity time.Duration
	// Now overrides the clock (tests).
	Now time.Time
	// InterfaceIPs overrides the auto-detected local addresses (tests).
	InterfaceIPs []string
}

// SelfSigned loads or creates a CA and a server certificate in opts.Dir and
// returns them ready for ListenAndServeTLS.
func SelfSigned(opts Options) (*Material, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("tls: 自签证书目录不能为空")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	validity := opts.Validity
	if validity <= 0 {
		validity = DefaultValidity
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("tls: 创建证书目录失败: %w", err)
	}

	sans, err := desiredSANs(opts.Hosts, opts.InterfaceIPs)
	if err != nil {
		return nil, err
	}

	ca, caPEM, err := loadOrCreateCA(opts.Dir, now)
	if err != nil {
		return nil, err
	}

	certPath := filepath.Join(opts.Dir, serverCertFile)
	keyPath := filepath.Join(opts.Dir, serverKeyFile)
	if ok, err := leafUsable(certPath, keyPath, ca, sans, now); err != nil {
		return nil, err
	} else if !ok {
		leafPEM, leafKeyPEM, err := signLeaf(ca, sans, now, validity)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(certPath, leafPEM, 0o644); err != nil {
			return nil, fmt.Errorf("tls: 写入服务端证书失败: %w", err)
		}
		if err := os.WriteFile(keyPath, leafKeyPEM, 0o600); err != nil {
			return nil, fmt.Errorf("tls: 写入服务端私钥失败: %w", err)
		}
	}

	info, err := describeFiles(certPath, keyPath, ModeSelfSigned, now)
	if err != nil {
		return nil, err
	}
	info.CAPEM = caPEM
	return &Material{Info: *info, CertFile: certPath, KeyFile: keyPath}, nil
}

// FromFiles wraps certificates provided by the operator.
func FromFiles(certFile, keyFile string) (*Material, error) {
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)
	if certFile == "" || keyFile == "" {
		return nil, errors.New("tls: 需要同时提供证书与私钥文件")
	}
	info, err := describeFiles(certFile, keyFile, ModeFiles, time.Now())
	if err != nil {
		return nil, err
	}
	return &Material{Info: *info, CertFile: certFile, KeyFile: keyFile}, nil
}

// describeFiles validates a certificate/key pair and summarises it.
func describeFiles(certFile, keyFile, mode string, now time.Time) (*Info, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: 读取证书 %q / 私钥 %q 失败: %w", certFile, keyFile, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("tls: 解析证书失败: %w", err)
	}
	if now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("tls: 证书已于 %s 过期", leaf.NotAfter.Format(time.RFC3339))
	}
	return &Info{
		Enabled:  true,
		Mode:     mode,
		SANs:     certSANs(leaf),
		NotAfter: leaf.NotAfter,
	}, nil
}

// certSANs lists the DNS names and IPs of a certificate in a stable order.
func certSANs(cert *x509.Certificate) []string {
	out := make([]string, 0, len(cert.DNSNames)+len(cert.IPAddresses))
	out = append(out, cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		out = append(out, ip.String())
	}
	sort.Strings(out)
	return out
}

// desiredSANs merges the defaults, the local interface addresses and the extra
// hosts requested by the operator.
func desiredSANs(hosts, interfaceIPs []string) ([]string, error) {
	set := map[string]bool{"localhost": true, "127.0.0.1": true}
	ips := interfaceIPs
	if ips == nil {
		ips = LocalInterfaceIPs()
	}
	for _, ip := range ips {
		if ip = strings.TrimSpace(ip); ip != "" {
			set[ip] = true
		}
	}
	if hn, err := os.Hostname(); err == nil && strings.TrimSpace(hn) != "" {
		set[strings.ToLower(strings.TrimSpace(hn))] = true
	}
	for _, host := range hosts {
		for _, part := range strings.Split(host, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part == "" {
				continue
			}
			if strings.ContainsAny(part, " /\\") {
				return nil, fmt.Errorf("tls: 非法的主机名或 IP %q", part)
			}
			set[part] = true
		}
	}
	out := make([]string, 0, len(set))
	for san := range set {
		out = append(out, san)
	}
	sort.Strings(out)
	return out, nil
}

// LocalInterfaceIPs lists the usable unicast addresses of this host so a client
// reaching the mirror by any of them passes certificate validation.
func LocalInterfaceIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]string, 0, 4)
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
			ip := ipnet.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				continue
			}
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return out
}

// caHolder keeps a parsed CA around while the leaf is (re)issued.
type caHolder struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// loadOrCreateCA reuses the CA in dir or creates a new one.
func loadOrCreateCA(dir string, now time.Time) (*caHolder, []byte, error) {
	certPath := filepath.Join(dir, caCertFile)
	keyPath := filepath.Join(dir, caKeyFile)

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		cert, key, err := parseCA(certPEM, keyPEM)
		if err == nil && now.Before(cert.NotAfter.Add(-renewBefore)) {
			return &caHolder{cert: cert, key: key}, certPEM, nil
		}
	}

	certPEM, keyPEM, err := newCA(now)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, nil, fmt.Errorf("tls: 写入 CA 证书失败: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("tls: 写入 CA 私钥失败: %w", err)
	}
	cert, key, err := parseCA(certPEM, keyPEM)
	if err != nil {
		return nil, nil, err
	}
	return &caHolder{cert: cert, key: key}, certPEM, nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, errors.New("tls: CA 证书 PEM 无效")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: 解析 CA 证书失败: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("tls: CA 私钥 PEM 无效")
	}
	parsed, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: 解析 CA 私钥失败: %w", err)
	}
	if !parsed.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, errors.New("tls: CA 私钥与证书不匹配")
	}
	return cert, parsed, nil
}

// newCA creates a self-signed CA suitable as a trust anchor for docker clients.
func newCA(now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: 生成 CA 私钥失败: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "docker-sync mirror CA " + host, Organization: []string{"docker-sync"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: 签发 CA 证书失败: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: 编码 CA 私钥失败: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// leafUsable reports whether the existing leaf can be served as is: same address
// set, signed by our CA and not about to expire.
func leafUsable(certPath, keyPath string, ca *caHolder, sans []string, now time.Time) (bool, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return false, nil // missing, half-written or garbage: re-sign
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false, nil
	}
	if !now.Before(leaf.NotAfter.Add(-renewBefore)) {
		return false, nil
	}
	if !sameStrings(certSANs(leaf), sans) {
		return false, nil
	}
	if err := leaf.CheckSignatureFrom(ca.cert); err != nil {
		return false, nil
	}
	return true, nil
}

// signLeaf issues a server certificate for sans signed by ca.
func signLeaf(ca *caHolder, sans []string, now time.Time, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: 生成服务端私钥失败: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "docker-sync mirror", Organization: []string{"docker-sync"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, san := range sans {
		if ip := net.ParseIP(san); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, san)
	}
	if len(tmpl.DNSNames) == 0 && len(tmpl.IPAddresses) > 0 {
		tmpl.Subject.CommonName = tmpl.IPAddresses[0].String()
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: 签发服务端证书失败: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: 编码服务端私钥失败: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("tls: 生成证书序列号失败: %w", err)
	}
	return serial, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
