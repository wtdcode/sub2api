package middleware

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// 前端产物是未压缩嵌入的（Vite 不预压缩，上游默认假设前面有 nginx/Caddy 反代）。
// 直接暴露端口时首屏要传 ~1.9MB 明文 JS/CSS，管理页因此有秒级空转。
// 这里只压缩静态文本资源：API 响应体量小且多为流式（SSE），不参与。

const gzipMinContentLength = 1024

var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
		return w
	},
}

type gzipResponseWriter struct {
	gin.ResponseWriter
	gz      *gzip.Writer
	started bool
	skip    bool
	// 首次写入不足阈值且未再有写入时不压缩：Content-Length 常常缺失
	// （gin c.Data 不设），必须按实际写入量决策而非依赖响应头。
	pending []byte
}

func (w *gzipResponseWriter) start() {
	if w.started {
		return
	}
	w.started = true

	header := w.ResponseWriter.Header()
	// 已被上游/其他中间件压缩，或明确不可压缩的类型：直通。
	if header.Get("Content-Encoding") != "" || !gzipCompressibleContentType(header.Get("Content-Type")) {
		w.skip = true
		return
	}
	header.Del("Content-Length") // 压缩后长度未知，交由分块传输
	header.Set("Content-Encoding", "gzip")
	header.Add("Vary", "Accept-Encoding")
	w.gz = gzipWriterPool.Get().(*gzip.Writer)
	w.gz.Reset(w.ResponseWriter)
}

func (w *gzipResponseWriter) Write(data []byte) (int, error) {
	// 缓冲到阈值再决定是否压缩：小响应（多为 API JSON）压缩不划算。
	if !w.started && w.gz == nil && !w.skip {
		if len(w.pending)+len(data) < gzipMinContentLength {
			w.pending = append(w.pending, data...)
			return len(data), nil
		}
	}
	w.start()
	if w.skip {
		return w.flushPendingUncompressed(data)
	}
	if len(w.pending) > 0 {
		buffered := w.pending
		w.pending = nil
		if _, err := w.gz.Write(buffered); err != nil {
			return 0, err
		}
	}
	return w.gz.Write(data)
}

func (w *gzipResponseWriter) flushPendingUncompressed(data []byte) (int, error) {
	if len(w.pending) > 0 {
		buffered := w.pending
		w.pending = nil
		if _, err := w.ResponseWriter.Write(buffered); err != nil {
			return 0, err
		}
	}
	return w.ResponseWriter.Write(data)
}

func (w *gzipResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	// 204/304 无响应体，压缩头会让部分客户端困惑。
	if code == http.StatusNoContent || code == http.StatusNotModified {
		w.skip = true
		w.started = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) close() {
	// 整个响应都没达到压缩阈值：原样写出缓冲内容。
	if len(w.pending) > 0 {
		buffered := w.pending
		w.pending = nil
		_, _ = w.ResponseWriter.Write(buffered)
	}
	if w.gz == nil {
		return
	}
	_ = w.gz.Close()
	gzipWriterPool.Put(w.gz)
	w.gz = nil
}

func gzipCompressibleContentType(contentType string) bool {
	mediaType := strings.TrimSpace(strings.ToLower(contentType))
	if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
		mediaType = strings.TrimSpace(mediaType[:idx])
	}
	switch mediaType {
	case "text/javascript", "application/javascript", "text/css", "text/html",
		"application/json", "image/svg+xml", "text/plain", "application/manifest+json",
		"application/wasm", "font/ttf", "application/xml", "text/xml":
		return true
	}
	// Content-Type 未知时（FileServer 对少数扩展名不设）不猜测。
	return false
}

// GzipStatic 压缩前端静态资源响应。仅作用于注册它的路由（嵌入式前端），
// 不影响 API 与网关流式转发。
func GzipStatic() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}
		writer := &gzipResponseWriter{ResponseWriter: c.Writer}
		c.Writer = writer
		defer func() {
			writer.close()
			c.Writer = writer.ResponseWriter
		}()
		c.Next()
	}
}
