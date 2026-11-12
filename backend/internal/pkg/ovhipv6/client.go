// Package ovhipv6 is the private control protocol for the host IPv6 egress agent.
package ovhipv6

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"
)

type Lease struct {
	ID       string `json:"id"`
	Address  string `json:"address"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	State    string `json:"state"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
}

type Status struct {
	Configured bool   `json:"configured"`
	Available  bool   `json:"available"`
	Enabled    bool   `json:"enabled"`
	Prefix     string `json:"prefix"`
	Issued     string `json:"issued"`
	Bound      int64  `json:"bound"`
	Pending    int64  `json:"pending_cleanup"`
	Error      string `json:"error,omitempty"`
}

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func FromEnv() *Client {
	base, token := strings.TrimRight(os.Getenv("OVH_IPV6_CONTROL_URL"), "/"), os.Getenv("OVH_IPV6_CONTROL_TOKEN")
	if base == "" || token == "" {
		return nil
	}
	return &Client{BaseURL: base, Token: token, HTTP: &http.Client{Timeout: 25 * time.Second, Transport: &http.Transport{Proxy: nil}}}
}

func (c *Client) Call(ctx context.Context, method, path string, input, output any) error {
	if c == nil {
		return fmt.Errorf("OVH IPv6 出口服务尚未配置")
	}
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("OVH IPv6 出口服务连接失败")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("OVH IPv6 出口服务返回 HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(output)
}

// Address calculates a host without enumerating the prefix. Zero is reserved.
// Decimal IDs support all 72 host bits of an IPv6 /56, beyond uint64.
func Address(prefix, sequence string) (netip.Addr, error) {
	p, err := netip.ParsePrefix(prefix)
	if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Bits() > 120 || p != p.Masked() {
		return netip.Addr{}, fmt.Errorf("invalid IPv6 allocation prefix")
	}
	n, ok := new(big.Int).SetString(sequence, 10)
	if !ok || n.Sign() <= 0 || n.BitLen() > 128-p.Bits() {
		return netip.Addr{}, fmt.Errorf("IPv6 allocation range exhausted or invalid")
	}
	b := p.Addr().As16()
	base := new(big.Int).SetBytes(b[:])
	base.Add(base, n).FillBytes(b[:])
	return netip.AddrFrom16(b), nil
}

func Next(sequence string) (string, error) {
	n, ok := new(big.Int).SetString(sequence, 10)
	if !ok || n.Sign() < 0 {
		return "", fmt.Errorf("invalid allocation counter")
	}
	return n.Add(n, big.NewInt(1)).String(), nil
}
