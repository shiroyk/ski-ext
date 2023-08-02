package fetch

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewRequest(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=iso-8859-9")
		switch r.Method {
		case http.MethodPut:
			if token := r.Header.Get("Authorization"); token != "1919810" {
				t.Errorf("unexpected token %s", token)
			}
		case http.MethodGet:
			_, err := fmt.Fprint(w, "114514")
			if err != nil {
				t.Error(err)
			}
			return
		}

		if strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
			}

			body, err := io.ReadAll(file)
			if err != nil {
				t.Error(err)
			}

			_, err = fmt.Fprint(w, string(body))
			if err != nil {
				t.Error(err)
			}
		} else {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}

			_, err = fmt.Fprint(w, string(body))
			if err != nil {
				t.Error(err)
			}
		}
	})

	fetch := newTestFetcher()

	mpBytes, mpwHeader := createMultiPart(t, map[string]any{
		"key":  "foo",
		"file": []byte{226, 153, 130, 239, 184, 142},
	})

	jsonData := struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}{Key: "foo", Value: "bar"}

	token := map[string]string{"Authorization": "1919810"}

	testCase := []struct {
		method string
		body   any
		header map[string]string
		want   string
	}{
		{http.MethodGet, nil, nil, "114514"},
		{
			http.MethodPost, url.Values{"key": {"holy"}}.Encode(),
			map[string]string{"Content-Type": "application/x-www-form-url"},
			"key=holy",
		},
		{http.MethodPost, []byte{226, 153, 130, 239, 184, 142}, nil, "♂︎"},
		{http.MethodPost, strings.NewReader("fa"), nil, "fa"},
		{http.MethodPost, bytes.NewBuffer(mpBytes), mpwHeader, "♂︎"},
		{http.MethodPost, bytes.NewReader(mpBytes), mpwHeader, "♂︎"},
		{http.MethodPost, jsonData, nil, `{"key":"foo","value":"bar"}`},
		{http.MethodPut, jsonData, token, `{"key":"foo","value":"bar"}`},
	}

	for _, useTLS := range []bool{false, true} {
		var ts *httptest.Server
		if useTLS {
			ts = httptest.NewTLSServer(h)
			fetch.Client = ts.Client()
		} else {
			ts = httptest.NewServer(h)
		}

		t.Run(fmt.Sprintf("useTLS=%v", useTLS), func(t *testing.T) {
			defer ts.Close()
			for _, r := range testCase {
				switch b := r.body.(type) {
				// rewrite bytes
				case *bytes.Buffer:
					b.Write(mpBytes)
				case *bytes.Reader:
					b.Reset(mpBytes)
				case *strings.Reader:
					b.Reset("fa")
				}
				req, err := NewRequest(r.method, ts.URL, r.body, r.header)
				if err != nil {
					t.Error(err)
				}

				res, err := DoString(fetch, req)
				if err != nil {
					t.Error(err)
					continue
				}
				assert.Equal(t, r.want, res)
			}
		})
	}
}

func createMultiPart(t *testing.T, data map[string]any) ([]byte, map[string]string) {
	buf := &bytes.Buffer{}
	mpw := multipart.NewWriter(buf)
	for k, v := range data {
		if f, ok := v.([]byte); ok {
			// Creates a new form-data header with the provided field name and file name.
			fw, err := mpw.CreateFormFile(k, "blob")
			if err != nil {
				t.Fatal(err)
			}
			// Write bytes to the part
			if _, err := fw.Write(f); err != nil {
				t.Fatal(err)
			}
		} else {
			// Write string value
			if err := mpw.WriteField(k, fmt.Sprintf("%v", v)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := mpw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), map[string]string{"Content-Type": mpw.FormDataContentType()}
}

func newTestFetcher() *fetchImpl {
	return NewFetch(Options{
		MaxBodySize:    DefaultMaxBodySize,
		RetryTimes:     DefaultRetryTimes,
		RetryHTTPCodes: DefaultRetryHTTPCodes,
		Timeout:        DefaultTimeout,
		CachePolicy:    RFC2616,
	}).(*fetchImpl)
}
