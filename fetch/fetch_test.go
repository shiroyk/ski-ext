package fetch

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCharsetFromHeaders(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=iso-8859-9")
		_, _ = fmt.Fprint(w, "G\xfcltekin")
	}))
	defer ts.Close()

	req, _ := NewRequest("GET", ts.URL, nil, nil)
	res, err := doString(newFetcherDefault(), req)
	require.NoError(t, err)

	assert.Equal(t, "Gültekin", res)
}

func TestCharsetFromBody(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, "G\xfcltekin")
	}))
	defer ts.Close()

	req, _ := NewRequest("POST", ts.URL, nil, nil)
	res, err := doString(newFetcherDefault(), req)
	require.NoError(t, err)

	assert.Equal(t, "Gültekin", res)
}

func TestRetry(t *testing.T) {
	t.Parallel()
	var times atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if times.Load() < DefaultRetryTimes {
			times.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{226})
		}
	}))
	defer ts.Close()

	fetch := newFetcherDefault()

	for i, s := range []string{"Status code retry", "Other error retry"} {
		t.Run(s, func(t *testing.T) {
			times.Store(0)
			var req *http.Request
			if i > 0 {
				req, _ = NewRequest("GET", ts.URL, nil, map[string]string{"Location": "\x00"})
			} else {
				req, _ = NewRequest("HEAD", ts.URL, nil, nil)
			}

			res, err := fetch.Do(req)
			if err != nil {
				assert.ErrorContains(t, err, "Location")
			} else {
				assert.Equal(t, http.StatusOK, res.StatusCode)
			}
		})
	}
}

func TestDecode(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoding := r.Header.Get("Content-Encoding")
		w.Header().Set("Content-Encoding", encoding)
		w.Header().Set("Content-Type", "text/plain")

		var bodyWriter io.WriteCloser
		switch encoding {
		case "deflate":
			bodyWriter = zlib.NewWriter(w)
		case "gzip":
			bodyWriter = gzip.NewWriter(w)
		case "br":
			bodyWriter = brotli.NewWriter(w)
		}
		defer bodyWriter.Close()

		bytes, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		_, _ = bodyWriter.Write(bytes)
	}))
	defer ts.Close()

	testCases := []struct {
		compress, want string
	}{
		{"deflate", "test1"},
		{"gzip", "test2"},
		{"br", "test3"},
	}

	fetch := newFetcherDefault()

	for _, testCase := range testCases {
		t.Run(testCase.compress, func(t *testing.T) {
			req, _ := NewRequest(http.MethodGet, ts.URL, testCase.want, map[string]string{
				"Content-Encoding": testCase.compress,
			})

			str, err := doString(fetch, req)
			require.NoError(t, err)
			assert.Equal(t, testCase.want, str)
		})
	}
}

// newFetcherDefault creates new client with default options
func newFetcherDefault() *Fetch {
	return NewFetch(Options{
		MaxBodySize:       DefaultMaxBodySize,
		RetryTimes:        DefaultRetryTimes,
		RetryHTTPCodes:    DefaultRetryHTTPCodes,
		Timeout:           DefaultTimeout,
		CharsetAutoDetect: true,
	})
}

func doString(fetch *Fetch, req *http.Request) (string, error) {
	res, err := fetch.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
