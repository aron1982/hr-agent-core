package infra

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// 常见的 CA bundle 环境变量名
var caBundleEnvVars = []string{
	"HERMES_CA_BUNDLE",
	"SSL_CERT_FILE",
	"REQUESTS_CA_BUNDLE",
	"CURL_CA_BUNDLE",
}

// CheckSSLCert 检查 SSL 证书文件有效性
// 返回 nil 表示证书有效，返回 error 表示证书有问题
func CheckSSLCert(certPath string) error {
	if certPath == "" {
		return fmt.Errorf("证书路径为空")
	}

	// 检查文件是否存在
	info, err := os.Stat(certPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &SSLError{Message: fmt.Sprintf("证书文件不存在: %s", certPath)}
		}
		return &SSLError{Message: fmt.Sprintf("无法访问证书文件 %s: %v", certPath, err)}
	}

	// 检查文件大小是否合理
	if info.Size() < 64 {
		return &SSLError{Message: fmt.Sprintf("证书文件太小，可能已损坏: %s (%d bytes)", certPath, info.Size())}
	}

	// 读取证书文件并尝试解析
	data, err := os.ReadFile(certPath)
	if err != nil {
		return &SSLError{Message: fmt.Sprintf("无法读取证书文件 %s: %v", certPath, err)}
	}

	// 尝试解析 PEM 证书
	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(data) {
		return &SSLError{Message: fmt.Sprintf("证书文件中未找到有效的 PEM 证书: %s", certPath)}
	}

	return nil
}

// IsSSLConfigured 检查是否配置了 SSL CA bundle
// 通过检查环境变量判断
func IsSSLConfigured() bool {
	for _, envVar := range caBundleEnvVars {
		val := os.Getenv(envVar)
		if val != "" {
			// 检查文件是否存在
			if _, err := os.Stat(val); err == nil {
				return true
			}
		}
	}

	// 如果没有环境变量，检查系统默认 CA 池是否可用
	_, err := tls.LoadX509KeyPair("", "")
	if err == nil {
		return true
	}

	// 尝试获取系统 CA 池
	pool, err := x509.SystemCertPool()
	if err == nil && pool != nil {
		return true
	}

	return false
}

// CheckAllSSLCertificates 检查所有环境变量配置的 CA bundle
// 返回收集到的所有错误
func CheckAllSSLCertificates() []error {
	var errs []error

	// 跳过 SSL 守卫检查（通过环境变量）
	skipVals := map[string]bool{"1": true, "true": true, "yes": true, "on": true}
	if skipVals[os.Getenv("HERMES_SKIP_SSL_GUARD")] {
		return nil
	}

	for _, envVar := range caBundleEnvVars {
		val := os.Getenv(envVar)
		if val == "" {
			continue
		}
		if err := CheckSSLCert(val); err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}
