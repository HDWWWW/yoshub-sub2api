package service

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
)

type OVHIPv6Admin interface {
	GetOVHIPv6(context.Context) (*ovhipv6.Status, error)
	SetOVHIPv6(context.Context, bool) (*ovhipv6.Status, error)
}

func (s *adminServiceImpl) GetOVHIPv6(ctx context.Context) (*ovhipv6.Status, error) {
	r, ok := s.proxyRepo.(OVHIPv6Admin)
	if !ok {
		return nil, fmt.Errorf("OVH IPv6 自动分配服务不可用")
	}
	return r.GetOVHIPv6(ctx)
}

func (s *adminServiceImpl) SetOVHIPv6(ctx context.Context, enabled bool) (*ovhipv6.Status, error) {
	r, ok := s.proxyRepo.(OVHIPv6Admin)
	if !ok {
		return nil, fmt.Errorf("OVH IPv6 自动分配服务不可用")
	}
	return r.SetOVHIPv6(ctx, enabled)
}

func (s *adminServiceImpl) attachManagedIPv6(ctx context.Context, proxies ...*Proxy) error {
	r, ok := s.proxyRepo.(interface {
		ManagedIPv6Addresses(context.Context, []int64) (map[int64]string, error)
	})
	if !ok || len(proxies) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(proxies))
	for _, p := range proxies {
		if p != nil {
			ids = append(ids, p.ID)
		}
	}
	addresses, err := r.ManagedIPv6Addresses(ctx, ids)
	if err != nil {
		return err
	}
	for _, p := range proxies {
		if p != nil {
			p.ManagedIPv6 = addresses[p.ID]
		}
	}
	return nil
}

func (s *adminServiceImpl) decorateManagedProxyList(ctx context.Context, proxies []Proxy) error {
	ptrs := make([]*Proxy, len(proxies))
	for i := range proxies {
		ptrs[i] = &proxies[i]
	}
	return s.attachManagedIPv6(ctx, ptrs...)
}

func (s *adminServiceImpl) decorateManagedProxyCounts(ctx context.Context, proxies []ProxyWithAccountCount) error {
	ptrs := make([]*Proxy, len(proxies))
	for i := range proxies {
		ptrs[i] = &proxies[i].Proxy
	}
	if err := s.attachManagedIPv6(ctx, ptrs...); err != nil {
		return err
	}
	for i := range proxies {
		if proxies[i].ManagedIPv6 != "" {
			proxies[i].IPAddress = proxies[i].ManagedIPv6
		}
	}
	return nil
}
