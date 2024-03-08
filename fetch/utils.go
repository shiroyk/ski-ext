package fetch

import (
	"io"
	"net/http"

	"github.com/shiroyk/ski"
)

// DoString do request and read response body as string.
func DoString(fetch ski.Fetch, req *http.Request) (string, error) {
	body, err := DoByte(fetch, req)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// DoByte do request and read response body.
func DoByte(fetch ski.Fetch, req *http.Request) ([]byte, error) {
	res, err := fetch.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}
