package router

import (
	"math"
	"sync"
	"time"
)

// limiter enforces per-IP connection rates (token bucket) and concurrent
// connection caps. It is only meaningful when the router sees real client
// addresses (directly or via PROXY protocol); behind a proxy without PROXY
// protocol every player shares one IP.
type limiter struct {
	mu            sync.Mutex
	rate          float64 // tokens per second; <= 0 disables the bucket
	burst         float64
	maxConcurrent int // <= 0 disables the cap
	ips           map[string]*ipState
	now           func() time.Time
}

type ipState struct {
	tokens float64
	last   time.Time
	active int
}

func newLimiter(ratePerSecond float64, burst int, maxConcurrent int) *limiter {
	if burst < 1 {
		burst = 1
	}
	return &limiter{
		rate:          ratePerSecond,
		burst:         float64(burst),
		maxConcurrent: maxConcurrent,
		ips:           map[string]*ipState{},
		now:           time.Now,
	}
}

func (l *limiter) enabled() bool {
	return l != nil && (l.rate > 0 || l.maxConcurrent > 0)
}

// acquire admits a new connection from ip. The returned release must be
// called when the connection ends. reason is "rate" or "concurrency" when
// refused.
func (l *limiter) acquire(ip string) (release func(), reason string) {
	if !l.enabled() {
		return func() {}, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.ips[ip]
	if st == nil {
		st = &ipState{tokens: l.burst, last: now}
		l.ips[ip] = st
	}
	if l.rate > 0 {
		st.tokens = math.Min(l.burst, st.tokens+now.Sub(st.last).Seconds()*l.rate)
		st.last = now
		if st.tokens < 1 {
			return nil, "rate"
		}
	}
	if l.maxConcurrent > 0 && st.active >= l.maxConcurrent {
		return nil, "concurrency"
	}
	if l.rate > 0 {
		st.tokens--
	}
	st.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			st.active--
			l.mu.Unlock()
		})
	}, ""
}

// sweep forgets idle IPs whose bucket is full again.
func (l *limiter) sweep() {
	if !l.enabled() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for ip, st := range l.ips {
		full := l.rate <= 0 || st.tokens+now.Sub(st.last).Seconds()*l.rate >= l.burst
		if st.active == 0 && full {
			delete(l.ips, ip)
		}
	}
}

// semaphore caps concurrent lobby/limbo sessions; nil means unlimited.
type semaphore chan struct{}

func newSemaphore(n int) semaphore {
	if n <= 0 {
		return nil
	}
	return make(semaphore, n)
}

func (s semaphore) tryAcquire() bool {
	if s == nil {
		return true
	}
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s semaphore) release() {
	if s != nil {
		<-s
	}
}
