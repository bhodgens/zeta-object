package s3

import (
	"crypto/hmac"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

// aliases.go — thin aliases over the stdlib used by auth_adapter.go so the
// moved verification code reads exactly like the pre-move source.

type timeTime = time.Time

const httpTimeFormat = http.TimeFormat

const httpStatusForbidden = http.StatusForbidden

const fifteenMinutes = 15 * time.Minute

func timeParse(layout, value string) (time.Time, error) { return time.Parse(layout, value) }

func timeSince(t time.Time) time.Duration { return time.Since(t) }

func timeNow() time.Time { return time.Now() }

func timeNowUTC() time.Time { return time.Now().UTC() }

func timeDuration(n int) time.Duration { return time.Duration(n) }

const timeSecond = time.Second

func strconvQuote(s string) string { return strconv.Quote(s) }

func hexEncode(b []byte) string { return hex.EncodeToString(b) }

func hmacEqual(a, b []byte) bool { return hmac.Equal(a, b) }
