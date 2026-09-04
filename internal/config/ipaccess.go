package config

import (
	"fmt"
	"net"
	"strings"
)

// IPAccessParsed 由 Load 时解析，供 HTTP 层做来源校验。
type IPAccessParsed struct {
	Enabled bool
	Rules   []*net.IPNet
	Trusted []*net.IPNet
}

// ParseIPAccessControl 解析 allowed_client_ips / trusted_proxy_cidrs；allowed 为空表示不启用 IP 限制。
func ParseIPAccessControl(allowed []string, trusted []string) (IPAccessParsed, error) {
	out := IPAccessParsed{}
	if len(allowed) == 0 {
		if len(trusted) > 0 {
			return IPAccessParsed{}, fmt.Errorf("配置无效：填写了 trusted_proxy_cidrs 时必须同时填写 allowed_client_ips（客户端白名单）")
		}
		return out, nil
	}
	out.Enabled = true
	for i, s := range allowed {
		s = strings.TrimSpace(s)
		if s == "" {
			return IPAccessParsed{}, fmt.Errorf("allowed_client_ips[%d] 不能为空", i)
		}
		n, err := parseIPOrCIDR(s)
		if err != nil {
			return IPAccessParsed{}, fmt.Errorf("allowed_client_ips[%d]: %w", i, err)
		}
		out.Rules = append(out.Rules, n)
	}
	for i, s := range trusted {
		s = strings.TrimSpace(s)
		if s == "" {
			return IPAccessParsed{}, fmt.Errorf("trusted_proxy_cidrs[%d] 不能为空", i)
		}
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			return IPAccessParsed{}, fmt.Errorf("trusted_proxy_cidrs[%d]: %w", i, err)
		}
		out.Trusted = append(out.Trusted, cidr)
	}
	return out, nil
}

func parseIPOrCIDR(s string) (*net.IPNet, error) {
	if strings.Contains(s, "/") {
		_, cidr, err := net.ParseCIDR(s)
		return cidr, err
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("无效的 IP 或 CIDR：%q", s)
	}
	if v4 := ip.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}, nil
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}, nil
}
