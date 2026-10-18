package api

import "testing"

func TestInviteCodeRoundTrip(t *testing.T) {
	code := EncodeInviteCode("100.64.0.1:8765", "inv_abc-DEF_123")
	addr, secret, err := DecodeInviteCode("  " + code + "\n")
	if err != nil || addr != "100.64.0.1:8765" || secret != "inv_abc-DEF_123" {
		t.Fatalf("addr=%q secret=%q err=%v", addr, secret, err)
	}
}

func TestInviteCodeRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "mcs1.", "mcs1.!!!", "mcs2.e30", "inv_nudo", "mcs1.e30", EncodeInviteCode("", "x"), EncodeInviteCode("a", "")} {
		if _, _, err := DecodeInviteCode(bad); err == nil {
			t.Errorf("%q doveva essere rifiutato", bad)
		}
	}
}
