//go:build windows

package auth

import (
	"testing"
	"time"
)

func TestPlanLabel(t *testing.T) {
	cases := []struct {
		tier string
		sub  string
		want string
	}{
		{"default_claude_max_5x", "max", "Plan Max · 5x"},
		{"default_claude_max_20x", "max", "Plan Max · 20x"},
		{"", "max", "Plan Max"},
		{"", "pro", "Plan Pro"},
		{"", "team", "Plan Team"},
		{"", "enterprise", "Plan Enterprise"},
		{"", "", "Claude"},
		{"algo_raro", "otra_cosa", "Claude"},
	}
	for _, c := range cases {
		cr := &Credentials{RateLimitTier: c.tier, SubscriptionType: c.sub}
		if got := cr.PlanLabel(); got != c.want {
			t.Errorf("PlanLabel(tier=%q sub=%q) = %q, quiero %q", c.tier, c.sub, got, c.want)
		}
	}
}

func TestExpired(t *testing.T) {
	if (&Credentials{ExpiresAt: 0}).Expired() {
		t.Error("sin expiresAt no debe considerarse expirado")
	}
	past := time.Now().Add(-time.Minute).UnixMilli()
	if !(&Credentials{ExpiresAt: past}).Expired() {
		t.Error("un token vencido debe reportarse expirado")
	}
	future := time.Now().Add(5 * time.Minute).UnixMilli()
	if (&Credentials{ExpiresAt: future}).Expired() {
		t.Error("un token con 5 min de vida no debe reportarse expirado (margen es 60 s)")
	}
}
