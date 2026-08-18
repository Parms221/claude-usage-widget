package api

import (
	"errors"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},      // lo que manda de verdad el endpoint: sin pista útil
		{"-5", 0},     // valor absurdo
		{"banana", 0}, // ni número ni fecha
		{"30", 30 * time.Second},
		{" 120 ", 2 * time.Minute},
		{"Mon, 17 Aug 2026 20:05:00 GMT", 5 * time.Minute},
		{"Mon, 17 Aug 2026 19:00:00 GMT", 0}, // fecha ya pasada
	}
	for _, c := range cases {
		if got := ParseRetryAfter(c.in, now); got != c.want {
			t.Errorf("ParseRetryAfter(%q) = %v, quiero %v", c.in, got, c.want)
		}
	}
}

func TestRateLimitErrorIs(t *testing.T) {
	err := error(&RateLimitError{RetryAfter: 90 * time.Second})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatal("errors.Is debería reconocer ErrRateLimited en el error tipado")
	}
	if errors.Is(err, ErrAuth) {
		t.Error("un 429 no debe confundirse con un fallo de autenticación")
	}

	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 90*time.Second {
		t.Errorf("errors.As no recuperó el Retry-After, dio %+v", rl)
	}
}
