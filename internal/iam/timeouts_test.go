package iam

import (
	"errors"
	"testing"
	"time"
)

func TestTimeoutsForRole(t *testing.T) {
	cases := []struct {
		role     Role
		wantIdle time.Duration
		wantHard time.Duration
		wantErr  error
	}{
		{RoleMaster, 15 * time.Minute, 4 * time.Hour, nil},
		{RoleTenantGerente, 30 * time.Minute, 8 * time.Hour, nil},
		{RoleTenantAtendente, 60 * time.Minute, 12 * time.Hour, nil},
		{RoleTenantCommon, 30 * time.Minute, 8 * time.Hour, nil},
		{Role(""), 0, 0, ErrUnknownRole},
		{Role("admin"), 0, 0, ErrUnknownRole},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			got, err := TimeoutsForRole(tc.role)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got.Idle != tc.wantIdle {
				t.Fatalf("Idle = %v, want %v", got.Idle, tc.wantIdle)
			}
			if got.Hard != tc.wantHard {
				t.Fatalf("Hard = %v, want %v", got.Hard, tc.wantHard)
			}
		})
	}
}

func TestCheckActivity(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	master, _ := TimeoutsForRole(RoleMaster) // 15m / 4h

	cases := []struct {
		name         string
		role         Role
		createdAt    time.Time
		lastActivity time.Time
		now          time.Time
		want         error
	}{
		{
			name:         "fresh-session-passes",
			role:         RoleMaster,
			createdAt:    t0,
			lastActivity: t0,
			now:          t0.Add(time.Minute),
			want:         nil,
		},
		{
			name:         "idle-edge-just-under-passes",
			role:         RoleMaster,
			createdAt:    t0,
			lastActivity: t0,
			now:          t0.Add(master.Idle - time.Nanosecond),
			want:         nil,
		},
		{
			name:         "idle-edge-exact-rejects",
			role:         RoleMaster,
			createdAt:    t0,
			lastActivity: t0,
			now:          t0.Add(master.Idle),
			want:         ErrSessionIdleTimeout,
		},
		{
			name:         "hard-edge-exact-rejects",
			role:         RoleMaster,
			createdAt:    t0,
			lastActivity: t0.Add(2 * time.Hour), // recent activity, doesn't matter
			now:          t0.Add(master.Hard),
			want:         ErrSessionHardTimeout,
		},
		{
			name:         "hard-wins-over-idle-when-both-trip",
			role:         RoleMaster,
			createdAt:    t0,
			lastActivity: t0,
			now:          t0.Add(master.Hard + time.Hour),
			want:         ErrSessionHardTimeout,
		},
		{
			name:         "atendente-60m-idle-tolerates-31min",
			role:         RoleTenantAtendente,
			createdAt:    t0,
			lastActivity: t0,
			now:          t0.Add(31 * time.Minute),
			want:         nil,
		},
		{
			name:         "gerente-30m-idle-rejects-31min",
			role:         RoleTenantGerente,
			createdAt:    t0,
			lastActivity: t0,
			now:          t0.Add(31 * time.Minute),
			want:         ErrSessionIdleTimeout,
		},
		{
			name:         "unknown-role-fails-closed",
			role:         Role("admin"),
			createdAt:    t0,
			lastActivity: t0,
			now:          t0.Add(time.Second),
			want:         ErrUnknownRole,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckActivity(tc.role, tc.createdAt, tc.lastActivity, tc.now)
			if !errors.Is(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
