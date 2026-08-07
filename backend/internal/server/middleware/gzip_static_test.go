package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newGzipTestRouter(contentType string, body []byte) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(GzipStatic())
	r.GET("/asset", func(c *gin.Context) {
		c.Data(http.StatusOK, contentType, body)
	})
	return r
}

func TestGzipStatic_CompressesLargeJS(t *testing.T) {
	body := []byte(strings.Repeat("export const x = 1;\n", 5000)) // ~100KB, 高度可压缩
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/asset", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	newGzipTestRouter("text/javascript; charset=utf-8", body).ServeHTTP(rec, req)

	require.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	require.Contains(t, rec.Header().Get("Vary"), "Accept-Encoding")
	require.Less(t, rec.Body.Len(), len(body)/5, "gzip must shrink repetitive JS by >5x")

	gr, err := gzip.NewReader(rec.Body)
	require.NoError(t, err)
	decoded, err := io.ReadAll(gr)
	require.NoError(t, err)
	require.Equal(t, body, decoded, "decompressed payload must be byte-identical")
}

func TestGzipStatic_SkipsWhenClientDoesNotAccept(t *testing.T) {
	body := []byte(strings.Repeat("a", 4096))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/asset", nil) // 无 Accept-Encoding
	newGzipTestRouter("text/css", body).ServeHTTP(rec, req)

	require.Empty(t, rec.Header().Get("Content-Encoding"))
	require.Equal(t, body, rec.Body.Bytes())
}

func TestGzipStatic_SkipsIncompressibleAndTinyPayloads(t *testing.T) {
	// 二进制类型不压缩
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/asset", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	newGzipTestRouter("image/png", []byte(strings.Repeat("x", 4096))).ServeHTTP(rec, req)
	require.Empty(t, rec.Header().Get("Content-Encoding"), "binary types must pass through")

	// 小响应不压缩（Content-Length 已知且 < 1KB）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/asset", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	newGzipTestRouter("application/json", []byte(`{"ok":true}`)).ServeHTTP(rec, req)
	require.Empty(t, rec.Header().Get("Content-Encoding"), "tiny payloads must pass through")
}
