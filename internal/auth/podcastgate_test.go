package auth

import "testing"

func TestPodcastGate(t *testing.T) {
	old := PodcastPlayerMinVersion
	defer func() { PodcastPlayerMinVersion = old }()
	for _, min := range []string{"", "2.3.0"} {
		PodcastPlayerMinVersion = min
		for _, tc := range []struct {
			app, version string
			want         bool
		}{
			{"AudioSilo Admin", "", true}, {"AudioSilo Admin", "old", true}, {"AudioSilo", "dev", true},
			{"AudioSilo", "2.3.0", min != ""}, {"AudioSilo", "2.10.0-beta+build", min != ""},
			{"AudioSilo", "2.1.0", false}, {"AudioSilo", "", false}, {"AudioSilo", "2.x.0", false},
			{"AudioSilo", "-", false}, {"AudioSilo", "2.3", false}, {"AudioSilo", "+", false},
			{"AudioSilo Bridge", "dev", false}, {"AudioSilo Manager", "dev", false}, {"", "", false},
		} {
			if got := SeesPodcasts(ClientInfo{App: tc.app, Version: tc.version}); got != tc.want {
				t.Errorf("min=%q %s/%s=%t", min, tc.app, tc.version, got)
			}
		}
	}
}
