package api

import (
	"errors"
	"net/url"
)

// parseOrigin 提取 host:port 形式，便于与请求 Host 比较。
func parseOrigin(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("非法 Origin 协议")
	}
	host := u.Host
	if host == "" {
		return "", errors.New("缺少 Host")
	}
	return host, nil
}
