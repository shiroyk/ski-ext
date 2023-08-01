package fetch

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"time"

	"github.com/shiroyk/cloudcat"
	"github.com/shiroyk/cloudcat-ext/fetch/http2"
	"golang.org/x/net/html/charset"
)

type fetcher struct {
	*http.Client
	charsetDetectDisabled bool
	maxBodySize           int64
	retryTimes            int
	retryHTTPCodes        []int
	timeout               time.Duration
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
	// DefaultHeaders defaults fetch.RequestConfig headers
	DefaultHeaders = map[string]string{
		"Accept":          "*/*",
		"Accept-Encoding": "gzip, deflate, br",
		"Accept-Language": "en-US,en;",
		"User-Agent":      "cloudcat",
	}
)

// Options The Fetch instance options
type Options struct {
	CharsetDetectDisabled bool              `yaml:"charset-detect-disabled"`
	MaxBodySize           int64             `yaml:"max-body-size"`
	RetryTimes            int               `yaml:"retry-times"`
	RetryHTTPCodes        []int             `yaml:"retry-http-codes"`
	Timeout               time.Duration     `yaml:"timeout"`
	CachePolicy           Policy            `yaml:"cache-policy"`
	RoundTripper          http.RoundTripper `yaml:"-"`
	Jar                   *cookiejar.Jar    `yaml:"-"`
}

// NewFetcher returns a new Fetch instance
func NewFetcher(opt Options) cloudcat.Fetch {
	fetch := new(fetcher)

	fetch.charsetDetectDisabled = opt.CharsetDetectDisabled
	fetch.maxBodySize = cloudcat.ZeroOr(opt.MaxBodySize, DefaultMaxBodySize)
	fetch.timeout = cloudcat.ZeroOr(opt.Timeout, DefaultTimeout)
	fetch.retryTimes = cloudcat.ZeroOr(opt.RetryTimes, DefaultRetryTimes)
	fetch.retryHTTPCodes = cloudcat.EmptyOr(opt.RetryHTTPCodes, DefaultRetryHTTPCodes)

	transport := opt.RoundTripper
	if transport == nil {
		transport = DefaultRoundTripper()
	}

	fetch.Client = &http.Client{
		Transport: transport,
		Timeout:   fetch.timeout,
	}

	if opt.Jar != nil {
		fetch.Client.Jar = opt.Jar
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
func (f *fetcher) Do(req *http.Request) (*http.Response, error) {
	res, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}

	// Limit response body reading
	bodyReader := io.LimitReader(res.Body, f.maxBodySize)

	if res.Request.Method != http.MethodHead { //nolint:nestif
		if encoding := res.Header.Get("Content-Encoding"); encoding != "" {
			bodyReader, err = DecodeReader(encoding, bodyReader)
			if err != nil {
				return nil, err
			}
			res.Body = io.NopCloser(bodyReader)
		}

		if res.ContentLength > 0 {
			if !f.charsetDetectDisabled {
				contentType := req.Header.Get("Content-Type")
				bodyReader, err = charset.NewReader(bodyReader, contentType)
				if err != nil {
					return nil, fmt.Errorf("charset detection error on content-type %s: %w", contentType, err)
				}
			}
			res.Body = io.NopCloser(bodyReader)
		}
	}

	return res, nil
}
