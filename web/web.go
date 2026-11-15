// Package web 内嵌前端静态资源，零外部依赖部署。
package web

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var files embed.FS

var (
	once    sync.Once
	gzipped map[string][]byte
	modTime time.Time
)

func load() {
	gzipped = make(map[string][]byte, 8)
	modTime = time.Now()
	sub, err := fs.Sub(files, "static")
	if err != nil {
		return
	}
	_ = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return nil
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := zw.Write(b); err == nil {
			_ = zw.Close()
			gzipped["/"+p] = buf.Bytes()
		}
		return nil
	})
}

// Handler 返回带缓存与压缩的静态资源处理器，未命中回落到 index.html（SPA）。
func Handler() http.Handler {
	once.Do(load)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := path.Clean(r.URL.Path)
		if p == "/" {
			p = "/index.html"
		}
		body, ok := gzipped[p]
		if !ok {
			// 回落到 SPA 入口；深链接由前端路由处理。
			if strings.HasPrefix(p, "/api/") {
				http.NotFound(w, r)
				return
			}
			p = "/index.html"
			body, ok = gzipped[p]
			if !ok {
				http.Error(w, "前端资源缺失", http.StatusInternalServerError)
				return
			}
		}
		h := w.Header()
		h.Set("Content-Type", contentType(p))
		h.Set("Vary", "Accept-Encoding")
		h.Set("Cache-Control", "no-cache")
		h.Set("Last-Modified", modTime.UTC().Format(http.TimeFormat))
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.Set("Content-Encoding", "gzip")
			h.Set("Content-Length", itoa(len(body)))
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = w.Write(body)
			return
		}
		// 客户端不支持 gzip 时解压后回退（少见，保证兼容）。
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			http.Error(w, "资源读取失败", http.StatusInternalServerError)
			return
		}
		defer zr.Close()
		plain, err := io.ReadAll(zr)
		if err != nil {
			http.Error(w, "资源解压失败", http.StatusInternalServerError)
			return
		}
		h.Set("Content-Length", itoa(len(plain)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(plain)
	})
}

func contentType(p string) string {
	switch path.Ext(p) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json; charset=utf-8"
	case ".webmanifest":
		return "application/manifest+json"
	default:
		return "application/octet-stream"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
