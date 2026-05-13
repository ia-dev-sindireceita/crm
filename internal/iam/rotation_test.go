package iam

import "testing"

func TestRotationTrigger_String(t *testing.T) {
	cases := []struct {
		t    RotationTrigger
		want string
	}{
		{RotateLogin, "login"},
		{RotateLogout, "logout"},
		{RotateRoleChange, "role_change"},
		{RotateTwoFactorSuccess, "twofactor_success"},
		{RotateUnknown, "unknown"},
		{RotationTrigger(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.t.String(); got != tc.want {
				t.Fatalf("String = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShouldRotate(t *testing.T) {
	for _, t1 := range []RotationTrigger{RotateLogin, RotateLogout, RotateRoleChange, RotateTwoFactorSuccess} {
		if !ShouldRotate(t1) {
			t.Fatalf("ShouldRotate(%v) = false, want true", t1)
		}
	}
	for _, t1 := range []RotationTrigger{RotateUnknown, RotationTrigger(99)} {
		if ShouldRotate(t1) {
			t.Fatalf("ShouldRotate(%v) = true, want false", t1)
		}
	}
}
