// Package ipv6egress implements a host-local proxy with one immutable IPv6
// source address per lease. It never routes through an IPv4 upstream.
package ipv6egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
)

type Config struct {
	Prefix        string `json:"prefix"`
	Interface     string `json:"interface"`
	ProxyListen   string `json:"proxy_listen"`
	ControlListen string `json:"control_listen"`
	AdvertiseHost string `json:"advertise_host"`
	ControlToken  string `json:"control_token"`
	StateDir      string `json:"state_dir"`
	ProbeURL      string `json:"probe_url"`
}

const pendingLifetime = 15 * time.Minute

func ReadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Config{}, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return Config{}, errors.New("configuration must not be readable by group or others (use mode 0600)")
	}
	var c Config
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if _, err := ovhipv6.Address(c.Prefix, "1"); err != nil {
		return err
	}
	p := netip.MustParsePrefix(c.Prefix)
	if !p.Addr().IsGlobalUnicast() || p.Addr().IsPrivate() {
		return errors.New("public IPv6 prefix required")
	}
	if c.Interface == "" || strings.ContainsAny(c.Interface, " /\\\x00\n\r") || len(c.Interface) > 15 {
		return errors.New("invalid network interface")
	}
	if len(c.ControlToken) < 32 {
		return errors.New("control token must contain at least 32 characters")
	}
	for _, s := range []string{c.ProxyListen, c.ControlListen} {
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			return fmt.Errorf("invalid listener: %w", err)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || (!ip.IsPrivate() && !ip.IsLoopback()) || ip.IsUnspecified() {
			return errors.New("listeners must bind a specific private or loopback address")
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1024 || n > 65535 {
			return errors.New("invalid listener port")
		}
	}
	if c.ProxyListen == c.ControlListen {
		return errors.New("proxy and control listeners must differ")
	}
	host, _, _ := net.SplitHostPort(c.ProxyListen)
	if c.AdvertiseHost == "" || c.AdvertiseHost != host {
		return errors.New("advertise_host must equal the proxy listener address")
	}
	if !filepath.IsAbs(c.StateDir) {
		return errors.New("state_dir must be absolute")
	}
	u, err := url.Parse(c.ProbeURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("probe_url must be an HTTPS endpoint")
	}
	return nil
}
