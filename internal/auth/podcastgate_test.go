package auth

import "testing"

func TestPodcastGate(t *testing.T) {
	old := PodcastPlayerMinVersion
	defer func() { PodcastPlayerMinVersion = old }()
	for _, min := range []string{"", "2.2.0"} {
		PodcastPlayerMinVersion = min
		for _, tc := range []struct {
			app, version string
			want         bool
		}{
			{"AudioSilo Admin", "", true}, {"AudioSilo Admin", "old", true}, {"AudioSilo", "dev", true},
			{"AudioSilo", "2.2.0", min != ""}, {"AudioSilo", "2.2.1", min != ""}, {"AudioSilo", "3.0.0", min != ""},
			{"AudioSilo", "2.2.0-rc.1", min != ""}, {"AudioSilo", "2.10.0-beta+build", min != ""},
			{"AudioSilo", "2.1.0", false}, {"AudioSilo", "2.1.99", false}, {"AudioSilo", "1.9.9", false},
			{"AudioSilo", "", false}, {"AudioSilo", "2.x.0", false}, {"AudioSilo", "-", false}, {"AudioSilo", "2.3", false}, {"AudioSilo", "+", false},
			{"AudioSilo Bridge", "dev", false}, {"AudioSilo Manager", "dev", false}, {"", "", false},
		} {
			if got := SeesPodcasts(ClientInfo{App: tc.app, Version: tc.version}); got != tc.want {
				t.Errorf("min=%q %s/%s=%t", min, tc.app, tc.version, got)
			}
		}
	}
}

// TestPodcastPlayerMinVersionShipped pins the shipped minimum: the first player
// with podcast views. TestPodcastGate covers the comparison at this value.
func TestPodcastPlayerMinVersionShipped(t *testing.T) {
	if PodcastPlayerMinVersion != "2.2.0" {
		t.Fatalf("PodcastPlayerMinVersion = %q, want 2.2.0", PodcastPlayerMinVersion)
	}
}
