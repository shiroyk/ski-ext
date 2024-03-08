package fetch

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/shiroyk/ski"
	"github.com/shiroyk/ski-ext/fetch/http2"
	"golang.org/x/net/html/charset"
)

type fetchImpl struct {
	*http.Client
	charsetAutoDetect bool
	maxBodySize       int64
	retryTimes        int
	retryHTTPCodes    []int
	timeout           time.Duration
	headers           http.Header
}

const (
	// DefaultMaxBodySize fetch.Response default max body size
	DefaultMaxBodySize int64 = 1024 * 1024 * 1024
	// DefaultRetryTimes fetch.RequestConfig retry times
	DefaultRetryTimes = 3
	// DefaultTimeout fetch.RequestConfig timeout
	DefaultTimeout = time.Minute
)

var (
	// DefaultRetryHTTPCodes retry fetch.RequestConfig error status code
	DefaultRetryHTTPCodes = []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, //nolint:lll
		http.StatusGatewayTimeout, http.StatusRequestTimeout}
	// DefaultHeaders defaults http headers
	DefaultHeaders = http.Header{
		"Accept":          {"*/*"},
		"Accept-Language": {"en-US,en;"},
		"User-Agent":      {"ski"},
	}
)

// Options The fetchImpl instance options
type Options struct {
	CharsetAutoDetect bool              `yaml:"charset-auto-detect"`
	MaxBodySize       int64             `yaml:"max-body-size"`
	RetryTimes        int               `yaml:"retry-times"` // greater than or equal 0
	RetryHTTPCodes    []int             `yaml:"retry-http-codes"`
	Timeout           time.Duration     `yaml:"timeout"`
	Headers           http.Header       `yaml:"headers"`
	RoundTripper      http.RoundTripper `yaml:"-"`
	Jar               http.CookieJar    `yaml:"-"`
}

// NewFetch returns a new ski.Fetch instance
func NewFetch(opt Options) ski.Fetch {
	fetch := &fetchImpl{
		timeout:        opt.Timeout,
		retryHTTPCodes: opt.RetryHTTPCodes,
		headers:        opt.Headers,
		retryTimes:     opt.RetryTimes,
	}

	fetch.charsetAutoDetect = opt.CharsetAutoDetect
	fetch.maxBodySize = opt.MaxBodySize
	if opt.Timeout == 0 {
		fetch.timeout = DefaultTimeout
	}
	if len(opt.RetryHTTPCodes) == 0 {
		fetch.retryHTTPCodes = DefaultRetryHTTPCodes
	}
	if len(fetch.headers) == 0 {
		fetch.headers = DefaultHeaders
	}

	transport := opt.RoundTripper
	if transport == nil {
		transport = DefaultRoundTripper()
	}

	fetch.Client = &http.Client{
		Transport: transport,
		Timeout:   fetch.timeout,
		Jar:       opt.Jar,
	}

	return fetch
}

// DefaultRoundTripper the fetch default RoundTripper
func DefaultRoundTripper() http.RoundTripper {
	return http2.ConfigureTransports(&http.Transport{
		Proxy: ProxyFromRequest,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		DisableCompression:    false,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	})
}

// Do sends an HTTP request and returns an HTTP response, following
// policy (such as redirects, cookies, auth) as configured on the
// client.
func (f *fetchImpl) Do(req *http.Request) (res *http.Response, err error) {
	for k, v := range f.headers {
		if _, ok := req.Header[k]; !ok {
			req.Header[k] = v
		}
	}
	for retry := 0; retry < f.retryTimes+1; retry++ {
		res, err = f.Client.Do(req)
		if err == nil && !slices.Contains(f.retryHTTPCodes, res.StatusCode) {
			break
		}
	}
	if err != nil {
		return nil, err
	}

	body := res.Body
	if f.maxBodySize > 0 {
		// Limit response body reading
		body = &http2.WarpReadCloser{Reader: io.LimitReader(res.Body, f.maxBodySize), Closer: body.Close}
	}

	if res.Request.Method != http.MethodHead {
		if res.ContentLength > 0 {
			if f.charsetAutoDetect {
				contentType := req.Header.Get("Content-Type")
				cr, err := charset.NewReader(body, contentType)
				if err != nil {
					return nil, fmt.Errorf("charset detection error on content-type %s: %w", contentType, err)
				}
				res.Body = &http2.WarpReadCloser{Reader: cr, Closer: body.Close}
			}
		}
	}

	return res, nil
}
