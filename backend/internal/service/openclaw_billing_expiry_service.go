package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const openClawBillingLeaseExpiryMaxInterval = 30 * time.Second

// OpenClawBillingLeaseExpiryService releases expired task-budget holds even
// while Suite is idle. The repository's FOR UPDATE SKIP LOCKED query makes
// concurrent instances and request-path expiry scans safe to run together.
type OpenClawBillingLeaseExpiryService struct {
	billing  *OpenClawBillingService
	interval time.Duration

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

func NewOpenClawBillingLeaseExpiryService(billing *OpenClawBillingService) *OpenClawBillingLeaseExpiryService {
	interval := openClawBillingLeaseExpiryMaxInterval
	if billing != nil && billing.config.LeaseTTLSeconds > 0 {
		halfTTL := time.Duration(billing.config.LeaseTTLSeconds) * time.Second / 2
		if halfTTL > 0 && halfTTL < interval {
			interval = halfTTL
		}
	}
	if interval < time.Second {
		interval = time.Second
	}
	return &OpenClawBillingLeaseExpiryService{
		billing:  billing,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

func (s *OpenClawBillingLeaseExpiryService) Start() {
	if s == nil || s.billing == nil || s.interval <= 0 || s.billing.requireEnabled() != nil {
		return
	}
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(s.interval)
			defer ticker.Stop()

			s.expireOnce()
			for {
				select {
				case <-ticker.C:
					s.expireOnce()
				case <-s.stopCh:
					return
				}
			}
		}()
	})
}

func (s *OpenClawBillingLeaseExpiryService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *OpenClawBillingLeaseExpiryService) expireOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	expired, err := s.billing.ExpireLeases(ctx)
	if err != nil {
		logger.LegacyPrintf("service.openclaw_billing_expiry", "[OpenClawBillingExpiry] release expired leases failed: %v", err)
		return
	}
	if expired > 0 {
		logger.LegacyPrintf("service.openclaw_billing_expiry", "[OpenClawBillingExpiry] released expired leases count=%d", expired)
	}
}
