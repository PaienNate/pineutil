package util_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/PaienNate/pineutil/ftlserver/util"
)

// 创建一个测试用的CA证书
func createTestCACert() (tls.Certificate, error) {
	// 生成CA私钥
	caPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}

	// 创建CA证书模板
	caTemplate := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Country:      []string{"CN"},
			Organization: []string{"Test CA"},
			CommonName:   "Test CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// 生成CA证书
	caCertDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caPrivateKey.PublicKey, caPrivateKey)
	if err != nil {
		return tls.Certificate{}, err
	}

	// 创建TLS证书
	caCert := tls.Certificate{
		Certificate: [][]byte{caCertDER},
		PrivateKey:  caPrivateKey,
	}

	return caCert, nil
}

func TestSetAndGetExternalIPs(t *testing.T) {
	// 测试设置和获取external IP
	testIPs := []string{"192.168.1.100", "10.0.0.1", "203.0.113.1"}
	util.SetExternalIPs(testIPs)

	result := util.GetExternalIPs()
	if len(result) != len(testIPs) {
		t.Errorf("期望 %d 个IP，实际得到 %d 个", len(testIPs), len(result))
	}

	for i, ip := range testIPs {
		if result[i] != ip {
			t.Errorf("期望IP %s，实际得到 %s", ip, result[i])
		}
	}
}

func TestAddExternalIP(t *testing.T) {
	// 清空现有IP
	util.SetExternalIPs([]string{})

	// 添加有效IP
	util.AddExternalIP("192.168.1.1")
	result := util.GetExternalIPs()
	if len(result) != 1 || result[0] != "192.168.1.1" {
		t.Errorf("添加有效IP失败")
	}

	// 添加重复IP
	util.AddExternalIP("192.168.1.1")
	result = util.GetExternalIPs()
	if len(result) != 1 {
		t.Errorf("重复IP应该被忽略")
	}

	// 添加无效IP
	util.AddExternalIP("invalid-ip")
	result = util.GetExternalIPs()
	if len(result) != 1 {
		t.Errorf("无效IP应该被忽略")
	}
}

func TestIsCACapable(t *testing.T) {
	// 创建测试CA证书
	caCert, err := createTestCACert()
	if err != nil {
		t.Fatalf("创建测试CA证书失败: %v", err)
	}

	// 解析证书
	cert, err := x509.ParseCertificate(caCert.Certificate[0])
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}

	// 测试CA能力检测
	if !util.IsCACapable(cert) {
		t.Errorf("CA证书应该被识别为具有CA能力")
	}
}

func TestGenerateCertificate(t *testing.T) {
	// 创建测试CA证书
	caCert, err := createTestCACert()
	if err != nil {
		t.Fatalf("创建测试CA证书失败: %v", err)
	}

	// 生成服务器证书
	hostname := "test.example.com"
	ips := []string{"192.168.1.100", "10.0.0.1"}
	serverCert, err := util.GenerateCertificate(caCert, hostname, ips)
	if err != nil {
		t.Fatalf("生成服务器证书失败: %v", err)
	}

	// 验证生成的证书
	cert, err := x509.ParseCertificate(serverCert.Certificate[0])
	if err != nil {
		t.Fatalf("解析生成的证书失败: %v", err)
	}

	// 检查主机名
	found := false
	for _, name := range cert.DNSNames {
		if name == hostname {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("生成的证书中未找到主机名 %s", hostname)
	}

	// 检查IP地址
	if len(cert.IPAddresses) == 0 {
		t.Errorf("生成的证书中未找到IP地址")
	}

	// 检查证书不是CA证书
	if cert.IsCA {
		t.Errorf("生成的服务器证书不应该是CA证书")
	}
}

func TestGenerateWildcardDomainIfNeeded(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"example.com", "example.com"},
		{"www.example.com", "*.example.com"},
		{"api.v1.example.com", "*.v1.example.com"},
		{"test.api.v1.example.com", "*.api.v1.example.com"},
	}

	for _, test := range tests {
		result := util.GenerateWildcardDomainIfNeeded(test.input)
		if result != test.expected {
			t.Errorf("输入 %s，期望 %s，实际得到 %s", test.input, test.expected, result)
		}
	}
}

func TestCertificateCache(t *testing.T) {
	// 创建测试CA证书
	caCert, err := createTestCACert()
	if err != nil {
		t.Fatalf("创建测试CA证书失败: %v", err)
	}

	hostname := "cache-test.example.com"
	ips := []string{"192.168.1.200"}

	// 第一次生成证书
	cert1, err := util.GenerateCertificate(caCert, hostname, ips)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}

	// 第二次生成相同的证书，应该从缓存获取
	cert2, err := util.GenerateCertificate(caCert, hostname, ips)
	if err != nil {
		t.Fatalf("从缓存获取证书失败: %v", err)
	}

	// 验证两个证书是否相同（通过比较证书内容）
	if len(cert1.Certificate) != len(cert2.Certificate) {
		t.Errorf("缓存的证书与原证书不匹配")
	}

	for i := range cert1.Certificate {
		if string(cert1.Certificate[i]) != string(cert2.Certificate[i]) {
			t.Errorf("缓存的证书内容与原证书不匹配")
		}
	}
}

func TestClearExpiredCertificates(t *testing.T) {
	// 获取清理前的统计信息
	totalBefore, expiredBefore := util.GetCacheStats()

	// 执行清理
	deletedCount := util.ClearExpiredCertificates()

	// 获取清理后的统计信息
	totalAfter, expiredAfter := util.GetCacheStats()

	// 验证清理结果
	if expiredAfter > expiredBefore {
		t.Errorf("清理后过期证书数量不应该增加")
	}

	if totalAfter > totalBefore {
		t.Errorf("清理后总证书数量不应该增加")
	}

	// 验证删除数量的合理性
	if deletedCount < 0 {
		t.Errorf("删除数量不应该为负数")
	}
}

func TestGetCacheStats(t *testing.T) {
	// 获取缓存统计信息
	total, expired := util.GetCacheStats()

	// 验证统计信息的合理性
	if total < 0 {
		t.Errorf("总证书数量不应该为负数")
	}

	if expired < 0 {
		t.Errorf("过期证书数量不应该为负数")
	}

	if expired > total {
		t.Errorf("过期证书数量不应该超过总数量")
	}
}

func TestCertificateStoreInterface(t *testing.T) {
	// 测试默认的SyncMap实现
	store := util.NewSyncMapCertificateStore()

	// 创建测试证书
	testCert := util.CachedCertificate{
		Certificate: tls.Certificate{},
		ExpiresAt:   time.Now().Add(time.Hour),
	}

	testKey := "test-key"

	// 测试Set和Get
	store.Set(testKey, testCert)
	cachedCert, ok := store.Get(testKey)
	if !ok {
		t.Errorf("应该能够获取存储的证书")
	}

	if cachedCert.ExpiresAt != testCert.ExpiresAt {
		t.Errorf("获取的证书过期时间不匹配")
	}

	// 测试Len
	if store.Len() == 0 {
		t.Errorf("存储后长度应该大于0")
	}

	// 测试Range
	found := false
	store.Range(func(key string, cert util.CachedCertificate) bool {
		if key == testKey {
			found = true
		}
		return true
	})

	if !found {
		t.Errorf("Range应该能够遍历到存储的证书")
	}

	// 测试Delete
	store.Delete(testKey)
	_, ok = store.Get(testKey)
	if ok {
		t.Errorf("删除后不应该能够获取证书")
	}
}

func TestSetAndGetCertificateStore(t *testing.T) {
	// 保存原始存储
	originalStore := util.GetCertificateStore()

	// 创建新的存储实现
	newStore := util.NewSyncMapCertificateStore()

	// 设置新的存储
	util.SetCertificateStore(newStore)

	// 验证设置成功
	currentStore := util.GetCertificateStore()
	if currentStore != newStore {
		t.Errorf("设置的证书存储与获取的不匹配")
	}

	// 恢复原始存储
	util.SetCertificateStore(originalStore)
}
