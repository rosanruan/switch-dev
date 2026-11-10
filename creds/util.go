package creds

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpGet 发起 GET 请求，返回 body + statusCode
func httpGet(url string, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// httpPost 发起 POST 请求，返回 body + statusCode
func httpPost(url, payload string, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequest("POST", url, strings.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// parseJSON 解析 JSON 到结构体
func parseJSON(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

// ParseJSONPublic 公开版本，供其他包使用
func ParseJSONPublic(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
