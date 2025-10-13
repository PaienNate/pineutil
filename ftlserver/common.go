package ftlserver

import (
	"crypto/tls"

	"github.com/PaienNate/pineutil/ftlserver/util"
)

func GetFakeHttpTLSConfig(certFile, keyFile string, existingConfig *tls.Config) (*tls.Config, error) {
	// 读取certFile和keyFile的数据
	var err error
	certTLS, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	// 解析证书以检查是否为CA证书
	cert, err := util.ParseCertificate(certTLS.Certificate[0])
	if err != nil {
		return nil, err
	}

	// 检查是否为CA证书
	isCA := util.IsCACapable(cert)

	// 如果传入的配置为空，则新建一个
	tlsConfig := existingConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	}
	if isCA {
		// CA证书模式：动态生成证书
		tlsConfig.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// 获取请求的主机名和IP
			hostname, ipList, err2 := util.GetTLSHostNameAndIP(hello)
			if err2 != nil {
				return nil, err2
			}
			// 使用CA证书动态生成服务器证书
			serverCert, err2 := util.GenerateCertificate(certTLS, hostname, ipList)
			if err2 != nil {
				return nil, err2
			}
			return &serverCert, nil
		}
		// 确保清除原有的Certificates，因为我们要使用动态生成的方式
		tlsConfig.Certificates = nil
	} else {
		// 普通证书模式：直接使用提供的证书
		tlsConfig.Certificates = []tls.Certificate{certTLS}
		// 确保清除GetCertificate函数，因为我们要使用静态证书
		tlsConfig.GetCertificate = nil
	}
	// 修改证书校验防止弱参数
	tlsConfig.MinVersion = tls.VersionTLS12
	tlsConfig.CipherSuites = []uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	}
	tlsConfig.Renegotiation = tls.RenegotiateNever
	return tlsConfig, nil
}
