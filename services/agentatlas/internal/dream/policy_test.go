package dream

import (
	"testing"
	"time"
)

func validPolicy() Policy {
	return Policy{
		OrgScope: "department:研发一部", Schedule: "0 22 * * *",
		InputSources: []string{"work_briefs"}, VisibilityLevel: "members",
		MaskingRules:      []string{`1[3-9]\d{9}`},
		RiskSignalRules:   []string{`风险[:：]?\S+`},
		EvidenceRetention: "pointer_plus_display_summary",
		OutputSpaceID:     "spc_dept",
	}
}

func TestPolicyValidate(t *testing.T) {
	if err := validPolicy().Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	bad := validPolicy()
	bad.Schedule = "not-cron"
	if bad.Validate() == nil {
		t.Fatal("bad cron must fail")
	}
	bad = validPolicy()
	bad.VisibilityLevel = "everyone"
	if bad.Validate() == nil {
		t.Fatal("unknown visibility must fail")
	}
	bad = validPolicy()
	bad.MaskingRules = []string{"("}
	if bad.Validate() == nil {
		t.Fatal("bad masking regex must fail")
	}
	bad = validPolicy()
	bad.InputSources = []string{"telepathy"}
	if bad.Validate() == nil {
		t.Fatal("unknown input source must fail")
	}
}

func TestDueComputesWindow(t *testing.T) {
	p := validPolicy() // 22:00 daily
	now := time.Date(2026, 7, 6, 23, 0, 0, 0, time.UTC)

	// first run ever: window ends at today's 22:00
	start, end, due, err := Due(p, time.Time{}, now)
	if err != nil || !due {
		t.Fatalf("due: %v %v", due, err)
	}
	if end != time.Date(2026, 7, 6, 22, 0, 0, 0, time.UTC) {
		t.Fatalf("end = %v", end)
	}
	if start != end.Add(-24*time.Hour) {
		t.Fatalf("first window start = %v", start)
	}

	// already ran for today's firing: not due
	_, _, due, err = Due(p, end, now)
	if err != nil || due {
		t.Fatalf("must not be due again: %v %v", due, err)
	}

	// next evening: due with window [yesterday 22:00, today 22:00]
	nextNow := now.Add(24 * time.Hour)
	start2, end2, due2, _ := Due(p, end, nextNow)
	if !due2 || start2 != end || end2 != end.Add(24*time.Hour) {
		t.Fatalf("second window: %v %v due=%v", start2, end2, due2)
	}
}
