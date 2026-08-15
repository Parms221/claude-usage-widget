package api

import "testing"

func TestConvert(t *testing.T) {
	if convert(nil) != nil {
		t.Error("convert(nil) debería ser nil")
	}

	ts := "2026-08-14T10:59:59.513630+00:00" // formato real del endpoint
	b := convert(&rawBucket{Utilization: 24, ResetsAt: &ts})
	if b.Utilization != 24 {
		t.Errorf("Utilization = %v, quiero 24", b.Utilization)
	}
	if b.ResetsAt == nil {
		t.Fatal("ResetsAt no parseó el timestamp con offset +00:00")
	}
	if b.ResetsAt.UTC().Hour() != 10 || b.ResetsAt.UTC().Minute() != 59 {
		t.Errorf("ResetsAt = %v, hora inesperada", b.ResetsAt)
	}

	bad := "no es una fecha"
	if got := convert(&rawBucket{ResetsAt: &bad}); got.ResetsAt != nil {
		t.Error("un timestamp inválido debería dejar ResetsAt en nil, no fallar")
	}

	if got := convert(&rawBucket{Utilization: 15}); got.ResetsAt != nil {
		t.Error("sin resets_at el bucket debe quedar con ResetsAt nil")
	}
}
