package infra

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestIsInternalHost 内网直连的判定：本机 / 私有网段 / 内置自有服务（含子域）
// 直连；公网站点仍交给代理。
func TestIsInternalHost(t *testing.T) {
	internal := []string{
		"localhost", "localhost:9090", "127.0.0.1", "127.0.0.1:8080", "::1",
		"10.1.2.3", "172.16.0.9", "192.168.8.251", "169.254.1.2",
		"ehr.tenops.com", "ehr.tenops.com:443", "1.14.34.232", "1.14.34.232:9090",
		"mcp.ehr.tenops.com", // 子域
	}

	for _, h := range internal {
		if !IsInternalHost(h) {
			t.Errorf("%q 应判定为内网直连", h)
		}
	}

	external := []string{
		"api.deepseek.com", "dashscope.aliyuncs.com:443",
		"8.8.8.8", "1.14.34.233",             // 邻居地址不算
		"notehr.tenops.com", "tenops.com",     // 相似域名/父域不算
	}

	for _, h := range external {
		if IsInternalHost(h) {
			t.Errorf("%q 不应判定为内网直连", h)
		}
	}
}

// TestDirectHostsEnvExtend HRBUDDY_NO_PROXY_HOSTS 追加自定义内网网关。
func TestDirectHostsEnvExtend(t *testing.T) {
	t.Setenv(EnvNoProxyHosts, "gateway.corp.local, 10.0.0.5 ,")

	if !IsInternalHost("gateway.corp.local") {
		t.Fatal("环境变量追加的域名应直连")
	}
	if !IsInternalHost("api.gateway.corp.local") {
		t.Fatal("追加域名的子域也应直连")
	}
	if !IsInternalHost("ehr.tenops.com") {
		t.Fatal("追加后内置域名仍应直连")
	}
	if IsInternalHost("api.deepseek.com") {
		t.Fatal("公网站点不应直连")
	}
}

// TestInternalTransportSkipsProxyForInternalHosts 传输层对内部地址返回"无代理"。
//
// 公网分支刻意不断言（http.ProxyFromEnvironment 会缓存首次读到的环境变量，
// 断言结果取决于测试执行顺序）；这里只钉住本次要保证的性质：
// 内置自有服务绝不走代理。
func TestInternalTransportSkipsProxyForInternalHosts(t *testing.T) {
	// 即便用户配了代理，内部地址也必须直连
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:65000")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:65000")

	tr := InternalTransport()
	if tr.Proxy == nil {
		t.Fatal("传输层应设置 Proxy 选择函数")
	}

	for _, raw := range []string{"http://ehr.tenops.com/mcp/ehr", "http://1.14.34.232:9090/mcp", "http://127.0.0.1:9/status"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", raw, err)
		}

		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		_ = u

		proxy, err := tr.Proxy(req)
		if err != nil {
			t.Fatalf("%s 取代理失败: %v", raw, err)
		}
		if proxy != nil {
			t.Fatalf("%s 应直连，实际走代理 %s", raw, proxy)
		}
	}
}

// TestNewInternalClientUsesInternalTransport 客户端必须带上直连传输（否则
// 传空 Transport 会退回 DefaultTransport，代理规则失效）。
func TestNewInternalClientUsesInternalTransport(t *testing.T) {
	client := NewInternalClient(0)
	if client.Transport == nil {
		t.Fatal("客户端应使用自定义传输")
	}

	// 端到端：直连本机测试服务器仍可正常工作（回环被判定为内网）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("直连本机失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
}
