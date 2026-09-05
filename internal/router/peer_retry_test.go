package router

import "testing"

func TestPeer_NextDiscoveryDelay(t *testing.T) {
	tests := []struct {
		name        string
		refresh     int
		retry       int
		failures    int
		wantDelay   int64 // seconds; checked exactly unless wantMinDelay/wantMaxDelay is set
		wantKeep    bool
		wantAtLeast int64 // seconds; when > 0, wantDelay is ignored and only a lower bound is checked
		wantAtMost  int64 // seconds; when > 0, wantDelay is ignored and only an upper bound is checked
	}{
		{name: "success refresh 300", refresh: 300, retry: 15, failures: 0, wantDelay: 300, wantKeep: true},
		{name: "success refresh 0 stops polling", refresh: 0, retry: 15, failures: 0, wantDelay: 0, wantKeep: false},

		{name: "retry disabled falls back to refresh", refresh: 300, retry: 0, failures: 1, wantDelay: 300, wantKeep: true},
		{name: "retry disabled and refresh 0 stops polling", refresh: 0, retry: 0, failures: 1, wantDelay: 0, wantKeep: false},

		{name: "first failure", refresh: 3600, retry: 15, failures: 1, wantDelay: 15, wantKeep: true},
		{name: "second failure doubles", refresh: 3600, retry: 15, failures: 2, wantDelay: 30, wantKeep: true},
		{name: "third failure doubles again", refresh: 3600, retry: 15, failures: 3, wantDelay: 60, wantKeep: true},
		{name: "fourth failure", refresh: 3600, retry: 15, failures: 4, wantDelay: 120, wantKeep: true},
		{name: "fifth failure", refresh: 3600, retry: 15, failures: 5, wantDelay: 240, wantKeep: true},

		{name: "backoff caps at refresh", refresh: 300, retry: 15, failures: 6, wantDelay: 300, wantKeep: true},
		{name: "backoff stays capped well past reaching it", refresh: 300, retry: 15, failures: 7, wantDelay: 300, wantKeep: true},
		{name: "backoff stays capped for a huge failure count", refresh: 300, retry: 15, failures: 100, wantDelay: 300, wantKeep: true},

		{name: "refresh 0 caps at maxDiscoveryRetryInterval", refresh: 0, retry: 15, failures: 5, wantDelay: 240, wantKeep: true},
		{name: "refresh 0 reaches the fallback ceiling", refresh: 0, retry: 15, failures: 6, wantDelay: 300, wantKeep: true},
		{name: "refresh 0 stays at the fallback ceiling", refresh: 0, retry: 15, failures: 50, wantDelay: 300, wantKeep: true},

		{name: "retry larger than refresh clamps immediately", refresh: 1, retry: 15, failures: 1, wantDelay: 1, wantKeep: true},
		{name: "retry larger than refresh stays clamped", refresh: 1, retry: 15, failures: 9, wantDelay: 1, wantKeep: true},

		{name: "huge refresh does not overflow low failure count", refresh: 9223372036, retry: 15, failures: 1, wantAtLeast: 1, wantAtMost: 9223372036, wantKeep: true},
		{name: "huge refresh does not overflow mid failure count", refresh: 9223372036, retry: 15, failures: 30, wantAtLeast: 1, wantAtMost: 9223372036, wantKeep: true},
		{name: "huge refresh does not overflow high failure count", refresh: 9223372036, retry: 15, failures: 40, wantAtLeast: 1, wantAtMost: 9223372036, wantKeep: true},
		{name: "huge refresh does not overflow extreme failure count", refresh: 9223372036, retry: 15, failures: 1000, wantAtLeast: 1, wantAtMost: 9223372036, wantKeep: true},

		{name: "out of range refresh is clamped defensively", refresh: 10000000000, retry: 15, failures: 1, wantAtLeast: 1, wantAtMost: 9223372036, wantKeep: true},

		{name: "negative failure count behaves as success", refresh: 300, retry: 15, failures: -1, wantDelay: 300, wantKeep: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDelay, gotKeep := nextDiscoveryDelay(tt.refresh, tt.retry, tt.failures)

			if gotKeep != tt.wantKeep {
				t.Errorf("keepPolling = %v, want %v", gotKeep, tt.wantKeep)
			}

			gotSeconds := int64(gotDelay.Seconds())
			if tt.wantAtLeast > 0 || tt.wantAtMost > 0 {
				if gotDelay <= 0 {
					t.Errorf("delay = %v, want a positive duration", gotDelay)
				}
				if tt.wantAtLeast > 0 && gotSeconds < tt.wantAtLeast {
					t.Errorf("delay = %ds, want >= %ds", gotSeconds, tt.wantAtLeast)
				}
				if tt.wantAtMost > 0 && gotSeconds > tt.wantAtMost {
					t.Errorf("delay = %ds, want <= %ds", gotSeconds, tt.wantAtMost)
				}
				return
			}

			if gotSeconds != tt.wantDelay {
				t.Errorf("delay = %ds, want %ds", gotSeconds, tt.wantDelay)
			}
		})
	}
}
