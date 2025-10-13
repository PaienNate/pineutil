package util

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"github.com/PaienNate/goutil/syncmap"
	"math/big"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// CertificateStore 证书缓存接口
type CertificateStore interface {
	// Get 获取缓存的证书
	Get(key string) (CachedCertificate, bool)
	// Set 设置缓存的证书
	Set(key string, cert CachedCertificate)
	// Delete 删除缓存的证书
	Delete(key string)
	// Range 遍历所有缓存的证书
	Range(fn func(key string, cert CachedCertificate) bool)
	// Len 获取缓存中证书的数量
	Len() int
}

// SyncMapCertificateStore 基于SyncMap的证书缓存实现
type SyncMapCertificateStore struct {
	cache *syncmap.SyncMap[string, CachedCertificate]
}

// NewSyncMapCertificateStore 创建新的SyncMap证书缓存存储
func NewSyncMapCertificateStore() *SyncMapCertificateStore {
	return &SyncMapCertificateStore{
		cache: &syncmap.SyncMap[string, CachedCertificate]{},
	}
}

// Get 获取缓存的证书
func (s *SyncMapCertificateStore) Get(key string) (CachedCertificate, bool) {
	return s.cache.Load(key)
}

// Set 设置缓存的证书
func (s *SyncMapCertificateStore) Set(key string, cert CachedCertificate) {
	s.cache.Store(key, cert)
}

// Delete 删除缓存的证书
func (s *SyncMapCertificateStore) Delete(key string) {
	s.cache.Delete(key)
}

// Range 遍历所有缓存的证书
func (s *SyncMapCertificateStore) Range(fn func(key string, cert CachedCertificate) bool) {
	s.cache.Range(fn)
}

// Len 获取缓存中证书的数量
func (s *SyncMapCertificateStore) Len() int {
	return s.cache.Len()
}

// CachedCertificate 缓存的证书结构，包含证书和过期时间
type CachedCertificate struct {
	Certificate tls.Certificate
	ExpiresAt   time.Time
}

var (
	certStore     CertificateStore
	certStoreOnce sync.Once
	// 全局配置，用于存储external IP列表
	globalConfig struct {
		mu          sync.RWMutex
		externalIPs []string
	}
)

// ensureCertStore 确保证书存储已初始化（懒加载）
func ensureCertStore() {
	certStoreOnce.Do(func() {
		if certStore == nil {
			certStore = NewSyncMapCertificateStore()
		}
	})
}

// SetCertificateStore 设置自定义的证书存储实现
// 允许用户使用自己的缓存实现（如Redis、内存数据库等）
func SetCertificateStore(store CertificateStore) {
	certStore = store
}

// GetCertificateStore 获取当前的证书存储实现
func GetCertificateStore() CertificateStore {
	ensureCertStore()
	return certStore
}

// SetExternalIPs 设置external IP列表，支持并发安全
// 这些IP会被添加到所有生成的证书中
func SetExternalIPs(ips []string) {
	globalConfig.mu.Lock()
	defer globalConfig.mu.Unlock()

	// 过滤有效的IP地址
	var validIPs []string
	for _, ip := range ips {
		if net.ParseIP(ip) != nil {
			validIPs = append(validIPs, ip)
		}
	}
	globalConfig.externalIPs = validIPs
}

// GetExternalIPs 获取当前设置的external IP列表
func GetExternalIPs() []string {
	globalConfig.mu.RLock()
	defer globalConfig.mu.RUnlock()

	result := make([]string, len(globalConfig.externalIPs))
	copy(result, globalConfig.externalIPs)
	return result
}

// AddExternalIP 添加单个external IP
func AddExternalIP(ip string) {
	if net.ParseIP(ip) == nil {
		return // 无效IP，忽略
	}

	globalConfig.mu.Lock()
	defer globalConfig.mu.Unlock()

	// 检查是否已存在
	for _, existingIP := range globalConfig.externalIPs {
		if existingIP == ip {
			return
		}
	}
	globalConfig.externalIPs = append(globalConfig.externalIPs, ip)
}

// IsCACapable 检查证书是否具有CA能力
func IsCACapable(cert *x509.Certificate) bool {
	return cert.IsCA && (cert.KeyUsage&x509.KeyUsageCertSign) != 0
}

// ParseCertificate 解析DER编码的证书
func ParseCertificate(certDER []byte) (*x509.Certificate, error) {
	return x509.ParseCertificate(certDER)
}

// GenerateWildcardDomainIfNeeded
//
//	@描述: 根据域名的级别返回对应的域名或泛域名
//	@参数 domain
//	@返回值 string
func GenerateWildcardDomainIfNeeded(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) >= 3 {
		return "*." + strings.Join(parts[1:], ".")
	}
	return domain
}

// GetTLSHostNameAndIP
//
//	@描述: 通过ClientHello信息获取访问域名或IP
//	@参数 hello
//	@返回值 name
//	@返回值 ip
//	@返回值 err
func GetTLSHostNameAndIP(hello *tls.ClientHelloInfo) (name string, ipList []string, err error) {
	domainHostName := hello.ServerName
	addr := hello.Conn.LocalAddr()
	// 切分出IP地址
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", nil, err
	}

	// 获取配置的external IP列表
	externalIPs := GetExternalIPs()
	// 补充配置的IP和当前连接的本地IP
	externalIPs = append(externalIPs, host)
	// 去重（避免重复 IP）
	ipList = removeDuplicateIPs(externalIPs)

	// 如果用户根据域名访问
	if domainHostName != "" {
		domainHostName = GenerateWildcardDomainIfNeeded(domainHostName)
		return domainHostName, ipList, nil
	}
	return "", ipList, nil
}

// removeDuplicateIPs 去除重复的 IP
func removeDuplicateIPs(ips []string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, ip := range ips {
		if _, ok := seen[ip]; !ok {
			seen[ip] = struct{}{}
			result = append(result, ip)
		}
	}
	return result
}

// GenerateCertificate
//
//	@描述: 通过传入的CA证书和私钥动态生成对应域名的证书
//	@参数 caCertTLS 使用tls.LoadX509KeyPair("ca.pem", "ca.key")获取的CA证书
//	@参数 hostname 域名
//	@参数 ips IP列表，如果用户使用IP访问，则签发IP证书
//	@返回值 tls.Certificate
//	@返回值 error
func GenerateCertificate(caCertTLS tls.Certificate, hostname string, ips []string) (tls.Certificate, error) {
	ensureCertStore()

	if hostname == "" {
		hostname = "localhost"
	}

	// 1. 对 IP 列表排序，确保相同的 IP 集合生成相同的 cacheKey
	sortedIPs := make([]string, len(ips))
	copy(sortedIPs, ips)
	sort.Strings(sortedIPs)

	// 解析 IP 地址
	var ipAddresses []net.IP
	for _, ip := range ips {
		if parsedIP := net.ParseIP(ip); parsedIP != nil {
			ipAddresses = append(ipAddresses, parsedIP)
		}
	}

	// 生成缓存的 key，可以根据 hostname 和 ip 的组合来区分不同的证书
	cacheKey := hostname + "|" + strings.Join(sortedIPs, ",")

	// 检查缓存中是否已有该证书，并验证是否过期
	if cachedCert, ok := certStore.Get(cacheKey); ok {
		// 检查证书是否过期（提前30分钟刷新）
		if time.Now().Add(30 * time.Minute).Before(cachedCert.ExpiresAt) {
			return cachedCert.Certificate, nil
		} else {
			// 证书即将过期或已过期，从缓存中删除
			certStore.Delete(cacheKey)
		}
	}

	// 转换TLS证书为X509证书
	caCert, err := x509.ParseCertificate(caCertTLS.Certificate[0])
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("解析CA证书失败: %w", err)
	}

	// 读取CA证书私钥用于签发，支持RSA和EC私钥
	var caPrivateKey interface{}
	var serverPrivateKey interface{}

	switch key := caCertTLS.PrivateKey.(type) {
	case *rsa.PrivateKey:
		caPrivateKey = key
		// 为服务器证书生成新的RSA私钥
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("生成服务器RSA私钥失败: %w", err)
		}
		serverPrivateKey = rsaKey
	case *ecdsa.PrivateKey:
		caPrivateKey = key
		// 为服务器证书生成新的EC私钥（使用与CA相同的曲线）
		ecKey, err := ecdsa.GenerateKey(key.Curve, rand.Reader)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("生成服务器EC私钥失败: %w", err)
		}
		serverPrivateKey = ecKey
	default:
		return tls.Certificate{}, fmt.Errorf("不支持的CA私钥类型: %T", key)
	}

	// 计算证书有效期，确保：
	// 1. 从当前时间开始
	// 2. 不超过浏览器接受的 398 天（约 13 个月）
	// 3. 不超过 CA 证书的有效期
	notBefore := time.Now().Add(-5 * time.Minute) // 提前5分钟避免时钟偏差
	if notBefore.Before(caCert.NotBefore) {
		notBefore = caCert.NotBefore
	}

	maxBrowserValidity := notBefore.Add(398 * 24 * time.Hour) // 浏览器最大接受 398 天
	notAfter := maxBrowserValidity

	// 如果 CA 证书的有效期更短，则使用 CA 的有效期
	if caCert.NotAfter.Before(notAfter) {
		notAfter = caCert.NotAfter
	}

	// 生成随机序列号
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("生成序列号失败: %w", err)
	}

	// 创建证书模板，按照行业规范设置字段
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Country:            []string{"CN"},
			Organization:       []string{"Pinenut's GCertificate"},
			OrganizationalUnit: []string{"IT Department"},
			CommonName:         hostname,
		},
		DNSNames:              []string{hostname},
		IPAddresses:           ipAddresses,
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		// 添加Subject Key Identifier
		SubjectKeyId: generateSubjectKeyId(getPublicKey(serverPrivateKey)),
		// 添加Authority Key Identifier
		AuthorityKeyId: caCert.SubjectKeyId,
	}

	// 生成证书
	certificateDER, err := x509.CreateCertificate(rand.Reader, &template, caCert, getPublicKey(serverPrivateKey), caPrivateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("创建证书失败: %w", err)
	}

	// 将证书用于服务器
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})

	// 根据私钥类型编码私钥
	var keyPEM []byte
	switch key := serverPrivateKey.(type) {
	case *rsa.PrivateKey:
		keyBytes := x509.MarshalPKCS1PrivateKey(key)
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyBytes})
	case *ecdsa.PrivateKey:
		keyBytes, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("编码EC私钥失败: %w", err)
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	default:
		return tls.Certificate{}, fmt.Errorf("不支持的服务器私钥类型: %T", key)
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("创建X509密钥对失败: %w", err)
	}

	// 将证书和过期时间存储到缓存
	cachedCert := CachedCertificate{
		Certificate: pair,
		ExpiresAt:   notAfter,
	}
	certStore.Set(cacheKey, cachedCert)
	return pair, nil
}

// ClearExpiredCertificates 手动清理所有过期的证书缓存
func ClearExpiredCertificates() int {
	ensureCertStore()

	var deletedCount int
	now := time.Now()

	certStore.Range(func(key string, cachedCert CachedCertificate) bool {
		if now.After(cachedCert.ExpiresAt) {
			certStore.Delete(key)
			deletedCount++
		}
		return true
	})

	return deletedCount
}

// GetCacheStats 获取缓存统计信息
func GetCacheStats() (total int, expired int) {
	ensureCertStore()

	now := time.Now()

	certStore.Range(func(key string, cachedCert CachedCertificate) bool {
		total++
		if now.After(cachedCert.ExpiresAt) {
			expired++
		}
		return true
	})

	return total, expired
}

// getPublicKey 从私钥中提取公钥
func getPublicKey(privateKey interface{}) interface{} {
	switch key := privateKey.(type) {
	case *rsa.PrivateKey:
		return &key.PublicKey
	case *ecdsa.PrivateKey:
		return &key.PublicKey
	default:
		return nil
	}
}

// generateSubjectKeyId 生成Subject Key Identifier
func generateSubjectKeyId(pub interface{}) []byte {
	// 使用公钥的SHA-256哈希作为Subject Key Identifier
	var pubKeyBytes []byte

	switch key := pub.(type) {
	case *rsa.PublicKey:
		pubKeyBytes = x509.MarshalPKCS1PublicKey(key)
	case *ecdsa.PublicKey:
		var err error
		pubKeyBytes, err = x509.MarshalPKIXPublicKey(key)
		if err != nil {
			return nil
		}
	default:
		return nil
	}

	// 计算SHA-256哈希（更安全的哈希算法）
	h := sha256.New()
	h.Write(pubKeyBytes)
	return h.Sum(nil)
}
