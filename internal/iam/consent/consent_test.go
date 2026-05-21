package consent

import "testing"

func TestSubjectType_IsValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		s    SubjectType
		want bool
	}{
		{"user", SubjectUser, true},
		{"contact", SubjectContact, true},
		{"tenant", SubjectTenant, true},
		{"empty", SubjectType(""), false},
		{"bogus", SubjectType("admin"), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.s.IsValid(); got != tc.want {
				t.Errorf("SubjectType(%q).IsValid() = %v; want %v", tc.s, got, tc.want)
			}
		})
	}
}

func TestPurpose_IsValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    Purpose
		want bool
	}{
		{"tos", PurposeTermsOfService, true},
		{"privacy", PurposePrivacyPolicy, true},
		{"marketing", PurposeMarketing, true},
		{"cookies", PurposeCookiesAnalytics, true},
		{"empty", Purpose(""), false},
		{"bogus", Purpose("ai_consent"), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.p.IsValid(); got != tc.want {
				t.Errorf("Purpose(%q).IsValid() = %v; want %v", tc.p, got, tc.want)
			}
		})
	}
}

func TestSubject_IsValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		s    Subject
		want bool
	}{
		{"valid user", Subject{Type: SubjectUser, ID: "u-1"}, true},
		{"valid contact", Subject{Type: SubjectContact, ID: "c-1"}, true},
		{"valid tenant", Subject{Type: SubjectTenant, ID: "t-1"}, true},
		{"bad type", Subject{Type: SubjectType("admin"), ID: "x"}, false},
		{"blank id", Subject{Type: SubjectUser, ID: "  "}, false},
		{"empty id", Subject{Type: SubjectUser, ID: ""}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.s.IsValid(); got != tc.want {
				t.Errorf("Subject(%+v).IsValid() = %v; want %v", tc.s, got, tc.want)
			}
		})
	}
}
