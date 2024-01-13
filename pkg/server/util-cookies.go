package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/italypaleale/traefik-forward-auth/pkg/config"
	"github.com/italypaleale/traefik-forward-auth/pkg/user"
)

const (
	jwtIssuer           = "traefik-forward-auth"
	stateCookieName     = "tf_state"
	acceptableClockSkew = 30 * time.Second
	nonceSize           = 12 // Nonce size in bytes
)

func (s *Server) getSessionCookie(c *gin.Context) (profile *user.Profile, err error) {
	cfg := config.Get()

	// Get the cookie
	cookieValue, err := c.Cookie(cfg.CookieName)
	if errors.Is(err, http.ErrNoCookie) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get cookie: %w", err)
	}
	if cookieValue == "" {
		return nil, fmt.Errorf("cookie %s is empty", cfg.CookieName)
	}

	// Parse the JWT in the cookie
	token, err := jwt.Parse([]byte(cookieValue),
		jwt.WithAcceptableSkew(acceptableClockSkew),
		jwt.WithIssuer(jwtIssuer+"/"+s.auth.GetProviderName()),
		jwt.WithAudience(cfg.Hostname),
		jwt.WithKey(jwa.HS256, cfg.GetTokenSigningKey()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JWT: %w", err)
	}

	// Get the user profile from the claim
	claims, err := token.AsMap(c.Request.Context())
	if err != nil {
		return nil, fmt.Errorf("failed to get claims from JWT: %w", err)
	}
	profile, err = user.NewProfileFromClaims(claims)
	if err != nil {
		return nil, fmt.Errorf("failed to parse claims from JWT: %w", err)
	}
	if len(claims) > 0 {
		s.auth.PopulateAdditionalClaims(claims, profile.SetAdditionalClaim)
	}

	return profile, nil
}

func (s *Server) setSessionCookie(c *gin.Context, profile *user.Profile) error {
	cfg := config.Get()
	expiration := cfg.SessionLifetime

	// Claims for the JWT
	now := time.Now()
	builder := jwt.NewBuilder()
	profile.AppendClaims(builder)
	token, err := builder.
		Issuer(jwtIssuer + "/" + s.auth.GetProviderName()).
		Audience([]string{cfg.Hostname}).
		IssuedAt(now).
		// Add 1 extra second to synchronize with cookie expiry
		Expiration(now.Add(expiration + time.Second)).
		NotBefore(now).
		Build()
	if err != nil {
		return fmt.Errorf("failed to build JWT: %w", err)
	}

	// Generate the JWT
	cookieValue, err := jwt.NewSerializer().
		Sign(jwt.WithKey(jwa.HS256, cfg.GetTokenSigningKey())).
		Serialize(token)
	if err != nil {
		return fmt.Errorf("failed to serialize token: %w", err)
	}

	// Set the cookie
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(cfg.CookieName, string(cookieValue), int(expiration.Seconds()), "/", cfg.CookieDomain, !cfg.CookieInsecure, true)

	return nil
}

func (s *Server) deleteSessionCookie(c *gin.Context) {
	cfg := config.Get()

	if _, err := c.Cookie(cfg.CookieName); err != nil {
		// Cookie was not set in the request, nothing to unset
		return
	}

	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(cfg.CookieName, "", -1, "/", cfg.CookieDomain, !cfg.CookieInsecure, true)
}

func (s *Server) getStateCookie(c *gin.Context) (nonce string, returnURL string, err error) {
	cfg := config.Get()

	// Get the cookie
	cookieValue, err := c.Cookie(stateCookieName)
	if errors.Is(err, http.ErrNoCookie) {
		return "", "", nil
	} else if err != nil {
		return "", "", fmt.Errorf("failed to get cookie: %w", err)
	}
	if cookieValue == "" {
		return "", "", fmt.Errorf("cookie %s is empty", cfg.CookieName)
	}

	// Parse the JWT in the cookie
	token, err := jwt.Parse([]byte(cookieValue),
		jwt.WithAcceptableSkew(acceptableClockSkew),
		jwt.WithIssuer(jwtIssuer+"/"+s.auth.GetProviderName()),
		jwt.WithAudience(cfg.Hostname),
		jwt.WithKey(jwa.HS256, cfg.GetTokenSigningKey()),
	)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse JWT: %w", err)
	}

	// Get the nonce
	nonceAny, _ := token.Get("nonce")
	nonce, _ = nonceAny.(string)
	if nonce == "" {
		return "", "", errors.New("claim 'nonce' not found in JWT")
	}
	nonceBytes, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(nonceBytes) != nonceSize {
		return "", "", errors.New("claim 'nonce' not found in JWT")
	}

	// Get the return URL
	returnURLAny, _ := token.Get("return_url")
	returnURL, _ = returnURLAny.(string)
	if returnURL == "" {
		return "", "", errors.New("claim 'return_url' not found in JWT")
	}

	// Validate the signature inside the token
	expectSig := s.stateCookieSig(c, nonceBytes)
	sigAny, ok := token.Get("sig")
	if !ok {
		return "", "", errors.New("claim 'sig' not found in JWT")
	}
	sig, _ := sigAny.(string)
	if sig != expectSig {
		return "", "", errors.New("claim 'sig' invalid in JWT")
	}

	return nonce, returnURL, nil
}

func (s *Server) setStateCookie(c *gin.Context, returnURL string) (nonce string, err error) {
	cfg := config.Get()
	expiration := cfg.AuthenticationTimeout

	// Generate a nonce
	nonceBytes := make([]byte, nonceSize)
	_, err = io.ReadFull(rand.Reader, nonceBytes)
	if err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}
	nonce = base64.RawURLEncoding.EncodeToString(nonceBytes)

	// Computes a signature that includes certain properties from the request that are sufficiently stable
	sig := s.stateCookieSig(c, nonceBytes)

	// Claims for the JWT
	now := time.Now()
	token, err := jwt.NewBuilder().
		Issuer(jwtIssuer+"/"+s.auth.GetProviderName()).
		Audience([]string{cfg.Hostname}).
		IssuedAt(now).
		// Add 1 extra second to synchronize with cookie expiry
		Expiration(now.Add(expiration+time.Second)).
		NotBefore(now).
		Claim("nonce", nonce).
		Claim("sig", sig).
		Claim("return_url", returnURL).
		Build()
	if err != nil {
		return "", fmt.Errorf("failed to build JWT: %w", err)
	}

	// Generate the JWT
	cookieValue, err := jwt.NewSerializer().
		Sign(jwt.WithKey(jwa.HS256, cfg.GetTokenSigningKey())).
		Serialize(token)
	if err != nil {
		return "", fmt.Errorf("failed to serialize token: %w", err)
	}

	// Set the cookie
	host, _, _ := net.SplitHostPort(cfg.Hostname)
	if host == "" {
		host = cfg.Hostname
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(stateCookieName, string(cookieValue), int(expiration.Seconds()), "/", host, !cfg.CookieInsecure, true)

	// Return the nonce
	return nonce, nil
}

func (s *Server) deleteStateCookie(c *gin.Context) {
	cfg := config.Get()

	if _, err := c.Cookie(stateCookieName); err != nil {
		// Cookie was not set in the request, nothing to unset
		return
	}

	host, _, _ := net.SplitHostPort(cfg.Hostname)
	if host == "" {
		host = cfg.Hostname
	}

	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(stateCookieName, "", -1, "/", host, !cfg.CookieInsecure, true)
}

func (s *Server) stateCookieSig(c *gin.Context, nonce []byte) string {
	h := hmac.New(sha256.New224, nonce)
	h.Write([]byte("tfa-state-sig"))
	h.Write([]byte(c.GetHeader("User-Agent")))
	h.Write([]byte(c.GetHeader("Accept-Language")))
	h.Write([]byte(c.GetHeader("DNT")))
	h.Write([]byte(s.auth.GetProviderName()))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
