package http

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/fault"
)

const (
	headerRequestID = "X-Request-ID"
	ctxRequestID    = "requestId"
)

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// requestID accepts a well-formed inbound X-Request-ID (from the proxy) or
// generates one, and echoes it in the response.
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(headerRequestID)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		c.Set(ctxRequestID, id)
		c.Header(headerRequestID, id)
		c.Request = c.Request.WithContext(audit.WithRequestID(c.Request.Context(), id))
		c.Next()
	}
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RequestIDFrom returns the request ID assigned by the middleware.
func RequestIDFrom(c *gin.Context) string { return c.GetString(ctxRequestID) }

// baseMiddleware is the chain every request passes through. The access log
// wraps recovery, so a request whose handler panics is still logged (as the
// 500 recovery answers); inside recovery the panic unwound past the log write.
func baseMiddleware(log *slog.Logger) []gin.HandlerFunc {
	return []gin.HandlerFunc{requestID(), accessLog(log), recovery(log), securityHeaders()}
}

// accessLog writes one structured record per request. Query strings are
// omitted because they may carry identifiers or search terms.
func accessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		status := c.Writer.Status()
		attrs := []any{
			"requestId", RequestIDFrom(c),
			"method", c.Request.Method,
			"route", c.FullPath(),
			"path", c.Request.URL.Path,
			"status", status,
			"durationMs", time.Since(start).Milliseconds(),
			"bytes", c.Writer.Size(),
			"clientIp", c.ClientIP(),
		}
		if len(c.Errors) > 0 {
			attrs = append(attrs, "error", c.Errors.Last().Error())
		}
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case c.Request.URL.Path == "/health/live" || c.Request.URL.Path == "/health/ready":
			level = slog.LevelDebug
		}
		log.Log(c.Request.Context(), level, "http request", attrs...)
	}
}

// recovery converts panics into a 500 problem and logs the stack.
func recovery(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic recovered", "requestId", RequestIDFrom(c), "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				if !c.Writer.Written() {
					writeError(c, fault.Internal(fmt.Errorf("panic: %v", rec)))
				} else {
					c.Abort()
				}
			}
		}()
		c.Next()
	}
}

// securityHeaders sets conservative headers for an API that returns JSON.
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Cache-Control", "no-store")
		c.Next()
	}
}
