package util

import (
	"math/rand"
	"strconv"
	"sync"
	"time"

	"net/http"

	"github.com/aws/aws-sdk-go/aws/request"
)

type ConvoyAWSRetryer struct {
	NumMaxRetries    int
	MinDelay         int
	MinThrottleDelay int
	MaxDelay         int
}

// DefaultConvoyAWSRetryer creates a new ConvoyAWSRetryer with sensible defaults
func DefaultConvoyAWSRetryer() *ConvoyAWSRetryer {
	return &ConvoyAWSRetryer{NumMaxRetries: 10, MinDelay: 200, MinThrottleDelay: 500, MaxDelay: 30000}
}

// MaxRetries returns the number of maximum returns the service will use to make
// an individual API request.
func (c ConvoyAWSRetryer) MaxRetries() int {
	return c.NumMaxRetries
}

var seededRand = rand.New(&lockedSource{src: rand.NewSource(time.Now().UnixNano())})

// RetryRules returns the delay duration before retrying this request again
func (c ConvoyAWSRetryer) RetryRules(r *request.Request) time.Duration {
	minTime := c.MinDelay
	throttle := c.shouldThrottle(r)
	if throttle {
		if delay, ok := getRetryDelay(r); ok {
			log.Infof("Retrying with suggested delay from headers: %v", delay)
			return delay
		}

		minTime = c.MinThrottleDelay
	}

	retryCount := r.RetryCount

	// exponential delay with jitter
	delay := (1 << uint(retryCount)) * (seededRand.Intn(minTime) + minTime)

	// delay increases exponentially, but we cap it at a configurable max delay
	if delay > c.MaxDelay {
		delay = c.MaxDelay
	}
	timeDelay := time.Duration(delay) * time.Millisecond
	log.Infof("Retrying Request (%v) - retry #%v, delay: %v", r.Error, retryCount, timeDelay)
	return timeDelay
}

// ShouldRetry returns true if the request should be retried.
func (c ConvoyAWSRetryer) ShouldRetry(r *request.Request) bool {
	// If one of the other handlers already set the retry state
	// we don't want to override it based on the service's state
	if r.Retryable != nil {
		return *r.Retryable
	}

	if r.HTTPResponse.StatusCode >= 500 {
		return true
	}
	return r.IsErrorRetryable() || c.shouldThrottle(r)
}

// ShouldThrottle returns true if the request should be throttled.
func (c ConvoyAWSRetryer) shouldThrottle(r *request.Request) bool {
	switch r.HTTPResponse.StatusCode {
	case http.StatusTooManyRequests:
	case http.StatusBadGateway:
	case http.StatusServiceUnavailable:
	case http.StatusGatewayTimeout:
	default:
		return r.IsErrorThrottle()
	}

	return true
}

// This will look in the Retry-After header, RFC 7231, for how long
// it will wait before attempting another request
func getRetryDelay(r *request.Request) (time.Duration, bool) {
	if !canUseRetryAfterHeader(r) {
		return 0, false
	}

	delayStr := r.HTTPResponse.Header.Get("Retry-After")
	if len(delayStr) == 0 {
		return 0, false
	}

	delay, err := strconv.Atoi(delayStr)
	if err != nil {
		return 0, false
	}

	return time.Duration(delay) * time.Second, true
}

// Will look at the status code to see if the retry header pertains to
// the status code.
func canUseRetryAfterHeader(r *request.Request) bool {
	switch r.HTTPResponse.StatusCode {
	case http.StatusTooManyRequests:
	case http.StatusServiceUnavailable:
	default:
		return false
	}

	return true
}

// lockedSource is a thread-safe implementation of rand.Source
type lockedSource struct {
	lk  sync.Mutex
	src rand.Source
}

func (r *lockedSource) Int63() int64 {
	r.lk.Lock()
	defer r.lk.Unlock()
	return r.src.Int63()
}

func (r *lockedSource) Seed(seed int64) {
	r.lk.Lock()
	defer r.lk.Unlock()
	r.src.Seed(seed)
}
