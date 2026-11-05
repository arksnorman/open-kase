package http

import (
	"crypto/subtle"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/product"
)

// Cookie names and paths (docs/api-contract-v1.md).
const (
	cookieRefresh     = product.CookiePrefix + "refresh"
	cookieCSRF        = product.CookiePrefix + "csrf"
	refreshCookiePath = "/api/v1/auth"
	headerCSRF        = "X-CSRF-Token"
)

func (a *api) setCookie(c *gin.Context, name, value, path string, httpOnly bool, expires time.Time) {
	maxAge := int(expires.Sub(a.Clock.Now()) / time.Second)
	if value == "" || maxAge <= 0 {
		maxAge = -1
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name: name, Value: value, Path: path, HttpOnly: httpOnly, Secure: a.SecureCookies,
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge,
	})
}

func (a *api) setSessionCookies(c *gin.Context, iss auth.Issued) {
	// A refresh inside the grace window issues no refresh token; the cookie
	// must then be left alone (an empty value would clear it).
	if iss.RefreshToken != "" {
		a.setCookie(c, cookieRefresh, iss.RefreshToken, refreshCookiePath, true, iss.SessionExpiresAt)
	}
	if iss.CSRFToken != "" {
		// Readable by the SPA for the double-submit header.
		a.setCookie(c, cookieCSRF, iss.CSRFToken, "/", false, iss.SessionExpiresAt)
	}
}

func (a *api) clearSessionCookies(c *gin.Context) {
	a.setCookie(c, cookieRefresh, "", refreshCookiePath, true, time.Time{})
	a.setCookie(c, cookieCSRF, "", "/", false, time.Time{})
}

// checkOrigin requires a same-origin Origin header: its host must equal the
// request Host (the browser-facing host, preserved by the proxy) or match
// the configured public base URL.
func (a *api) checkOrigin(c *gin.Context, required bool) error {
	origin := c.GetHeader("Origin")
	reject := fault.Forbidden("originRejected", "The request origin is not allowed.")
	if origin == "" {
		if required {
			return reject
		}
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return reject
	}
	if a.BaseURL != nil {
		if base := a.BaseURL(); base != "" {
			if b, err := url.Parse(base); err == nil && strings.EqualFold(b.Scheme, u.Scheme) && strings.EqualFold(b.Host, u.Host) {
				return nil
			}
		}
	}
	if strings.EqualFold(u.Host, c.Request.Host) {
		return nil
	}
	return reject
}

// checkCSRF enforces the Origin check and the double-submit header on
// cookie-authenticated routes. The service additionally binds the token to
// the session.
func (a *api) checkCSRF(c *gin.Context) (string, bool) {
	if err := a.checkOrigin(c, true); err != nil {
		writeError(c, err)
		return "", false
	}
	header := c.GetHeader(headerCSRF)
	cookie, _ := c.Cookie(cookieCSRF)
	if header == "" || cookie == "" || subtle.ConstantTimeCompare([]byte(header), []byte(cookie)) != 1 {
		writeError(c, fault.Forbidden("csrfFailed", "The request could not be verified. Reload the page and try again."))
		return "", false
	}
	return header, true
}

// throttleIP returns the client IP for per-IP throttling, or "" when no
// per-IP key should be used.
//
// Requests from a configured trusted proxy use the forwarded client IP.
// A request that carries X-Forwarded-For from a loopback or private address
// that is not configured as trusted most likely comes through a reverse
// proxy the operator has not declared: every user would share the proxy's
// address, and a few bad passwords could lock out the whole installation, so
// no IP key is used (per-account throttling still applies). Any other
// request is keyed on its connecting address: a client cannot opt out of
// per-IP limits by sending its own X-Forwarded-For.
func throttleIP(c *gin.Context) string {
	ip := c.ClientIP()
	remote := c.RemoteIP()
	if c.GetHeader("X-Forwarded-For") == "" || ip != remote {
		return ip
	}
	if addr, err := netip.ParseAddr(remote); err == nil {
		addr = addr.Unmap()
		if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() {
			return ""
		}
	}
	return remote
}

// requireJSON rejects non-JSON bodies so cross-site HTML forms
// (text/plain, urlencoded) cannot submit to cookie-bearing auth routes.
func requireJSON(c *gin.Context) error {
	mt := strings.ToLower(strings.TrimSpace(strings.Split(c.GetHeader("Content-Type"), ";")[0]))
	if mt != "application/json" {
		return fault.New(fault.KindUnsupportedMedia, "unsupportedMediaType", "Send the request body as application/json.")
	}
	return nil
}

// rejectCrossSite refuses browser requests that declare a cross-site
// context even when they carry no Origin header.
func rejectCrossSite(c *gin.Context) error {
	if site := c.GetHeader("Sec-Fetch-Site"); site == "cross-site" {
		return fault.Forbidden("originRejected", "The request origin is not allowed.")
	}
	return nil
}

// loginRequest is the request body of login.
type loginRequest struct {
	Login    string `json:"login" required:"true" doc:"Email address or username. An identifier containing @ is treated as an email address, otherwise as a username."`
	Password string `json:"password" required:"true"`
}

func (a *api) login(c *gin.Context) {
	if err := a.checkOrigin(c, false); err != nil {
		writeError(c, err)
		return
	}
	if err := rejectCrossSite(c); err != nil {
		writeError(c, err)
		return
	}
	if err := requireJSON(c); err != nil {
		writeError(c, err)
		return
	}
	var body loginRequest
	if !decode(c, &body) {
		return
	}
	in := auth.LoginInput{Login: body.Login, Password: body.Password, UserAgent: c.Request.UserAgent(), ClientIP: throttleIP(c)}
	iss, err := a.Auth.Login(c.Request.Context(), in)
	if err != nil {
		writeError(c, err)
		return
	}
	if iss.MFARequired {
		c.JSON(http.StatusOK, loginDTO{MFARequired: true, MFAToken: &iss.MFAToken, MFAExpiresIn: &iss.MFAExpiresIn})
		return
	}
	a.setSessionCookies(c, iss)
	u := toUser(iss.User)
	c.JSON(http.StatusOK, loginDTO{AccessToken: &iss.AccessToken, ExpiresIn: &iss.ExpiresIn, User: &u,
		MFAEnrolmentRequired: iss.MFAEnrolmentRequired})
}

func (a *api) refresh(c *gin.Context) {
	csrf, ok := a.checkCSRF(c)
	if !ok {
		return
	}
	refresh, _ := c.Cookie(cookieRefresh)
	iss, err := a.Auth.Refresh(c.Request.Context(), refresh, csrf)
	if err != nil {
		if fault.As(err).Kind == fault.KindUnauthenticated {
			a.clearSessionCookies(c)
		}
		writeError(c, err)
		return
	}
	a.setSessionCookies(c, iss)
	c.JSON(http.StatusOK, toAuth(iss))
}

func (a *api) logout(c *gin.Context) {
	csrf, ok := a.checkCSRF(c)
	if !ok {
		return
	}
	refresh, _ := c.Cookie(cookieRefresh)
	if err := a.Auth.Logout(c.Request.Context(), refresh, csrf); err != nil {
		writeError(c, err)
		return
	}
	a.clearSessionCookies(c)
	c.Status(http.StatusNoContent)
}
