package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadLeaf(t *testing.T, m *Material) *x509.Certificate {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(m.CertFile, m.KeyFile)
	if err != nil {
		t.Fatalf("load leaf: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf
}

func rootsFrom(t *testing.T, caPEM []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA PEM 无法解析")
	}
	return pool
}

func TestSelfSignedGeneratesUsablePair(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	m, err := SelfSigned(Options{Dir: dir, InterfaceIPs: []string{"192.168.1.10"}, Now: now})
	if err != nil {
		t.Fatalf("SelfSigned: %v", err)
	}
	if !m.Enabled || m.Mode != ModeSelfSigned {
		t.Fatalf("info = %+v", m.Info)
	}
	if len(m.CAPEM) == 0 {
		t.Fatal("CA PEM 为空，客户端将无处下载信任锚")
	}
	for _, want := range []string{"localhost", "127.0.0.1", "192.168.1.10"} {
		found := false
		for _, san := range m.SANs {
			if san == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("SANs = %v, 缺少 %s", m.SANs, want)
		}
	}
	for _, name := range []string{caCertFile, caKeyFile, serverCertFile, serverKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("缺少生成文件 %s: %v", name, err)
		}
	}

	leaf := loadLeaf(t, m)
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: rootsFrom(t, m.CAPEM), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		t.Fatalf("证书链校验失败: %v", err)
	}
	if len(chains) == 0 {
		t.Fatal("空证书链")
	}
	if err := leaf.VerifyHostname("192.168.1.10"); err != nil {
		t.Fatalf("IP SAN 校验失败: %v", err)
	}
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Fatalf("localhost SAN 校验失败: %v", err)
	}
	if leaf.IsCA {
		t.Fatal("服务端证书不应是 CA")
	}
	if got := leaf.NotAfter.Sub(now); got < DefaultValidity-2*time.Hour {
		t.Fatalf("有效期 = %v, 期望约 %v", got, DefaultValidity)
	}
}

func TestSelfSignedReusesMaterial(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	opts := Options{Dir: dir, InterfaceIPs: []string{"10.1.2.3"}, Now: now}
	first, err := SelfSigned(opts)
	if err != nil {
		t.Fatalf("第一次生成失败: %v", err)
	}
	firstLeaf := loadLeaf(t, first)

	opts.Now = now.Add(24 * time.Hour)
	second, err := SelfSigned(opts)
	if err != nil {
		t.Fatalf("第二次生成失败: %v", err)
	}
	secondLeaf := loadLeaf(t, second)
	if firstLeaf.SerialNumber.Cmp(secondLeaf.SerialNumber) != 0 {
		t.Fatal("地址集合没变却重新签发了证书（会让已安装的 ca.crt 失效）")
	}
	if string(first.CAPEM) != string(second.CAPEM) {
		t.Fatal("CA 不应变化")
	}
}

func TestSelfSignedResignsWhenAddressesChange(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first, err := SelfSigned(Options{Dir: dir, InterfaceIPs: []string{"10.1.2.3"}, Now: now})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	firstLeaf := loadLeaf(t, first)

	second, err := SelfSigned(Options{Dir: dir, InterfaceIPs: []string{"10.1.2.3", "10.9.9.9"}, Now: now})
	if err != nil {
		t.Fatalf("重新签发失败: %v", err)
	}
	secondLeaf := loadLeaf(t, second)
	if firstLeaf.SerialNumber.Cmp(secondLeaf.SerialNumber) == 0 {
		t.Fatal("地址集合变了必须重新签发")
	}
	if string(first.CAPEM) != string(second.CAPEM) {
		t.Fatal("CA 必须保持不变")
	}
	if err := secondLeaf.VerifyHostname("10.9.9.9"); err != nil {
		t.Fatalf("新地址未进 SAN: %v", err)
	}
}

func TestSelfSignedRenewsExpiringLeaf(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first, err := SelfSigned(Options{Dir: dir, InterfaceIPs: []string{"10.1.2.3"}, Now: now, Validity: 40 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	firstLeaf := loadLeaf(t, first)

	// 20 天后剩余有效期已经进入续期窗口。
	second, err := SelfSigned(Options{Dir: dir, InterfaceIPs: []string{"10.1.2.3"}, Now: now.Add(20 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	secondLeaf := loadLeaf(t, second)
	if firstLeaf.SerialNumber.Cmp(secondLeaf.SerialNumber) == 0 {
		t.Fatal("临近过期的证书应被续签")
	}
	if !secondLeaf.NotAfter.After(firstLeaf.NotAfter) {
		t.Fatalf("续签后 NotAfter 未延长: %v -> %v", firstLeaf.NotAfter, secondLeaf.NotAfter)
	}
}

func TestFromFiles(t *testing.T) {
	dir := t.TempDir()
	generated, err := SelfSigned(Options{Dir: dir, InterfaceIPs: []string{"10.1.2.3"}, Now: time.Now()})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	m, err := FromFiles(generated.CertFile, generated.KeyFile)
	if err != nil {
		t.Fatalf("FromFiles: %v", err)
	}
	if m.Mode != ModeFiles || !m.Enabled {
		t.Fatalf("info = %+v", m.Info)
	}
	if len(m.CAPEM) != 0 {
		t.Fatal("外部证书不应导出 CA")
	}
	if len(m.SANs) == 0 || m.NotAfter.IsZero() {
		t.Fatalf("info = %+v", m.Info)
	}
}

func TestFromFilesErrors(t *testing.T) {
	dir := t.TempDir()
	generated, err := SelfSigned(Options{Dir: dir, InterfaceIPs: []string{"10.1.2.3"}, Now: time.Now()})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if _, err := FromFiles("", ""); err == nil {
		t.Fatal("空路径应报错")
	}
	if _, err := FromFiles(generated.CertFile, filepath.Join(dir, "missing.key")); err == nil {
		t.Fatal("缺失私钥应报错")
	}

	// 证书与私钥不匹配
	if _, err := FromFiles(generated.CertFile, filepath.Join(dir, caKeyFile)); err == nil {
		t.Fatal("证书与私钥不匹配应报错")
	}

	// 过期证书
	expiredCert := filepath.Join(dir, "expired.pem")
	expiredKey := filepath.Join(dir, "expired.key")
	writeExpiredPair(t, expiredCert, expiredKey)
	if _, err := FromFiles(expiredCert, expiredKey); err == nil {
		t.Fatal("过期证书应报错")
	}
}

func TestSelfSignedRejectsBadHosts(t *testing.T) {
	dir := t.TempDir()
	if _, err := SelfSigned(Options{Dir: dir, Hosts: []string{"bad host"}, Now: time.Now()}); err == nil {
		t.Fatal("含空格的主机名应报错")
	}
	if _, err := SelfSigned(Options{Dir: "", Now: time.Now()}); err == nil {
		t.Fatal("空目录应报错")
	}
}

func TestLocalInterfaceIPsSkipsLoopback(t *testing.T) {
	for _, ip := range LocalInterfaceIPs() {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			t.Fatalf("非法地址 %q", ip)
		}
		if parsed.IsLoopback() || parsed.IsLinkLocalUnicast() {
			t.Fatalf("不应包含回环/链路本地地址: %q", ip)
		}
	}
}

func writeExpiredPair(t *testing.T, certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "expired"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("签发证书: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("编码密钥: %v", err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("写证书: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("写私钥: %v", err)
	}
}
