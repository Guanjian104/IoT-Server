package sfu

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

func sign(secret []byte, resource, identity, role string, exp time.Time) (string, error) {
	if strings.ContainsAny(resource, ".\n") || strings.ContainsAny(identity, ".\n") || strings.ContainsAny(role, ".\n") {
		return "", errors.New("invalid token field")
	}
	payload := resource + "\n" + identity + "\n" + role + "\n" + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func openToken(secret []byte, token string) (resource, identity, role string, exp time.Time, err error) {
	encodedPayload, encodedMAC, ok := strings.Cut(token, ".")
	if !ok {
		return "", "", "", time.Time{}, errors.New("invalid token")
	}
	payload, errPayload := base64.RawURLEncoding.DecodeString(encodedPayload)
	mac, errMAC := base64.RawURLEncoding.DecodeString(encodedMAC)
	sum := hmac.New(sha256.New, secret)
	_, _ = sum.Write(payload)
	if errPayload != nil || errMAC != nil || !hmac.Equal(mac, sum.Sum(nil)) {
		return "", "", "", time.Time{}, errors.New("invalid token")
	}
	parts := strings.Split(string(payload), "\n")
	if len(parts) != 4 {
		return "", "", "", time.Time{}, errors.New("invalid token")
	}
	unix, convErr := strconv.ParseInt(parts[3], 10, 64)
	if convErr != nil {
		return "", "", "", time.Time{}, errors.New("invalid token")
	}
	return parts[0], parts[1], parts[2], time.Unix(unix, 0), nil
}
