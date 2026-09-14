package router

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// CookieKey is the identifier under which the route cookie is stored on the
// client across a transfer.
const CookieKey = "vecta:route"

// signRoute produces "v1|server|player|expiry|sig". Binding the player name
// stops a cookie from being reused by someone else.
func signRoute(key []byte, server, player string, expires time.Time) []byte {
	body := strings.Join([]string{"v1", server, strings.ToLower(player), strconv.FormatInt(expires.Unix(), 10)}, "|")
	return []byte(body + "|" + mac(key, body))
}

func verifyRoute(key []byte, cookie []byte, player string, now time.Time) (string, error) {
	parts := strings.Split(string(cookie), "|")
	if len(parts) != 5 || parts[0] != "v1" {
		return "", errors.New("malformed route cookie")
	}
	body := strings.Join(parts[:4], "|")
	if !hmac.Equal([]byte(parts[4]), []byte(mac(key, body))) {
		return "", errors.New("bad route cookie signature")
	}
	if parts[2] != strings.ToLower(player) {
		return "", errors.New("route cookie issued to another player")
	}
	exp, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || now.Unix() > exp {
		return "", errors.New("route cookie expired")
	}
	return parts[1], nil
}

func mac(key []byte, body string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
