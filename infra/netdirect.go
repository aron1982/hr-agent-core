// 内网/自有服务直连，公网仍按用户代理走。
//
// 为什么需要：后端 HTTP 客户端默认遵循 `HTTP_PROXY`/`HTTPS_PROXY`（Go 的
// http.ProxyFromEnvironment，桌面端 spawn 后端时透传 process.env，用户开着
// v2ray 之类代理就会带上）。而 EHR / BOSS 是我们自己的服务，走代理既慢又不可控
// —— 2026-09-24 实测：开着代理时 EHR 面板明显变慢，BOSS MCP 的 `tools/list`
// 甚至不返回响应，把 MCP 装配挂死（表现为"启动不了"）。同一时间用 curl 直连
// 秒回，因为 curl 默认不走系统代理 —— 这就是当时"只有客户端慢/挂"的原因。
//
// 规则（按优先级）：
//
//	1. 本机与私有网段（localhost / 127.0.0.0-8 / ::1 / 10./172.16-31./192.168.）
//	2. 内置自有服务（见 builtinDirectHosts，含其子域）
//	3. HRBUDDY_NO_PROXY_HOSTS 追加的域名（逗号分隔，便于自建网关）
//	4. 其余：交给 http.ProxyFromEnvironment（用户自己的 NO_PROXY 也仍生效）
//
// 联网工具（agent/web）抓的是公网，保持走代理，不经这里。
package infra

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// EnvNoProxyHosts 追加"直连"域名（逗号分隔），用于自建 EHR/内网网关。
const EnvNoProxyHosts = "HRBUDDY_NO_PROXY_HOSTS"

// builtinDirectHosts 内置自有服务的域名/地址：EHR、BOSS 与 hr-buddy 官网部署在
// 自有服务器上，与 ehr_auth.go 的 defaultEHRMCPURL / defaultBOSSMCPURL、
// buddy_account.go 的 fallbackBuddyWebsite 同源。
var builtinDirectHosts = []string{
	"ehr.tenops.com",
	"buddy.tenops.com",
	"1.14.34.232",
}

// NewInternalClient 返回"内网直连、公网走代理"的 HTTP 客户端。
func NewInternalClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: InternalTransport()}
}

// InternalTransport 在 http.DefaultTransport 基础上加内网直连规则（保留默认的
// 连接池、超时与 HTTP/2 支持）。
func InternalTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// 理论上不可达（DefaultTransport 固定是 *http.Transport）；
		// 真出现就用零值 Transport，仍然受下面的 Proxy 规则约束
		base = &http.Transport{}
	}

	tr := base.Clone()
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		if IsInternalHost(req.URL.Host) {
			return nil, nil // 直连
		}

		return http.ProxyFromEnvironment(req)
	}

	return tr
}

// IsInternalHost 判断 host（可带端口）是否属于"应直连"的内网/自有服务。
func IsInternalHost(hostport string) bool {
	host := hostport

	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}

	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if host == "" || host == "localhost" {
		return true
	}

	// IP 字面量：回环 / 私有网段 / 链路本地一律直连。
	// 公网 IP 也可能是自有服务（内置 EHR 就在腾讯云上），所以不命中时
	// 继续往下比对域名清单，不能提前返回。
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return true
		}
	}

	for _, h := range directHosts() {
		// 命中自身或其子域（如 mcp.ehr.tenops.com）
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}

	return false
}

// directHosts 内置直连域名 + 环境变量追加项。
//
// 每次调用都读环境变量（请求频率低，代价可忽略）：这样测试与运行期都能改，
// 不必操心缓存失效。
func directHosts() []string {
	extra := strings.TrimSpace(os.Getenv(EnvNoProxyHosts))
	if extra == "" {
		return builtinDirectHosts
	}

	out := make([]string, 0, len(builtinDirectHosts)+4)
	out = append(out, builtinDirectHosts...)

	for _, h := range strings.Split(extra, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			out = append(out, h)
		}
	}

	return out
}
