package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// Device returns the device token for the browser of a user after a login:
// token if it is valid for the user, or a new one. The dashboard keeps it in
// a cookie, and Login gives a browser with the token a rate limit of its own.
func (s *Service) Device(userID, token string) string {
	if _, ok := s.deviceID(userID, token); ok {
		return token
	}
	b := make([]byte, 16)
	rand.Read(b)
	id := base64.RawURLEncoding.EncodeToString(b)
	return id + "." + s.deviceMAC(userID, id)
}

// deviceID returns the ID of a device token that Device made for userID.
func (s *Service) deviceID(userID, token string) (string, bool) {
	id, mac, ok := strings.Cut(token, ".")
	if !ok || userID == "" || len(s.deviceKey) == 0 {
		return "", false
	}
	return id, hmac.Equal([]byte(mac), []byte(s.deviceMAC(userID, id)))
}

func (s *Service) deviceMAC(userID, id string) string {
	m := hmac.New(sha256.New, s.deviceKey)
	m.Write([]byte(userID + "." + id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
