package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCommunityFieldsPassThrough: community_description (kept apart from the
// CC0 description) and the matched recording's chapter_count reach the
// envelope, and the attribution credits the CC BY-SA layer with the work's own
// metadata-site page.
func TestCommunityFieldsPassThrough(t *testing.T) {
	m := fullMock()
	m.workJSON = strings.Replace(martianWork, `"description":"Stranded on Mars.",`,
		`"description":"Stranded on Mars.","community_description":{"text":"A botanist improvises.","license":"CC-BY-SA-4.0"},`, 1)
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	env, err := svc.Enrich(context.Background(), "B00FLIJJSY", "")
	if err != nil {
		t.Fatal(err)
	}
	if env.Work.Description != "Stranded on Mars." {
		t.Fatalf("description = %q", env.Work.Description)
	}
	cd := env.Work.CommunityDescription
	if cd == nil || cd.Text != "A botanist improvises." || cd.License != "CC-BY-SA-4.0" {
		t.Fatalf("community_description = %+v", cd)
	}
	if env.Recording.ChapterCount != 12 {
		t.Fatalf("chapter_count = %d, want rec2's 12", env.Recording.ChapterCount)
	}
	want := MetaAttribution{
		Credit:     "AudioSilo Meta community contributors",
		License:    "CC BY-SA 4.0",
		LicenseURL: "https://creativecommons.org/licenses/by-sa/4.0/",
		SourceURL:  srv.URL + "/work?id=the-martian",
	}
	if a := env.Work.Attribution; a == nil || *a != want {
		t.Fatalf("attribution = %+v, want %+v", a, want)
	}
	if a := env.Work.Attribution; a.SourceURL != env.WebURL {
		t.Fatalf("source_url %q != web_url %q", a.SourceURL, env.WebURL)
	}
	body, _ := json.Marshal(env)
	for _, s := range []string{`"community_description":{"text":"A botanist improvises.","license":"CC-BY-SA-4.0"}`, `"chapter_count":12`, `"attribution":{"credit":"AudioSilo Meta community contributors","license":"CC BY-SA 4.0","license_url":"https://creativecommons.org/licenses/by-sa/4.0/","source_url":"`} {
		if !strings.Contains(string(body), s) {
			t.Fatalf("envelope missing %s: %s", s, body)
		}
	}

	// The work-id lookup carries the same fields (one toWork).
	w, err := svc.Work(context.Background(), "the-martian")
	if err != nil || w.Attribution == nil || w.CommunityDescription == nil {
		t.Fatalf("Work = %+v, %v", w, err)
	}
}

// TestAttributionPresence: the credit is there iff the work carries any CC
// BY-SA content, each kind on its own enough; a work with only CC0 core fields
// (and a blank community description) carries none, and neither do the unknown
// chapter_count nor the attribution reach the JSON then.
func TestAttributionPresence(t *testing.T) {
	const core = `"id":"w","title":"W","authors":[],"language":"en","description":"Core.","recordings":[{"id":"r","narrators":[]}]`
	for name, tc := range map[string]struct {
		extra string
		want  bool
	}{
		"core only":                   {"", false},
		"blank community description": {`,"community_description":{"text":"  "}`, false},
		"characters":                  {`,"characters":[{"id":"c","name":"C","reveal":{"chapter":1}}]`, true},
		"recaps":                      {`,"recaps":[{"through":{"chapter":1},"text":"t"}]`, true},
		"recap summary":               {`,"recap_summary":{"in_short":"s"}`, true},
		"community description":       {`,"community_description":{"text":"d"}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{` + core + tc.extra + `}`))
			})
			mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"work":{"id":"w","title":"W","authors":[]},"recording_id":"r"}`))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			svc := NewService(srv.URL, nil)
			env, err := svc.Enrich(context.Background(), "B0W", "")
			if err != nil {
				t.Fatal(err)
			}
			if got := env.Work.Attribution != nil; got != tc.want {
				t.Fatalf("attribution present = %v, want %v", got, tc.want)
			}
			body, _ := json.Marshal(env)
			if !tc.want && (strings.Contains(string(body), `"attribution"`) || strings.Contains(string(body), `"community_description"`)) {
				t.Fatalf("unexpected keys: %s", body)
			}
			if strings.Contains(string(body), `"chapter_count"`) {
				t.Fatalf("an unknown chapter_count reached the JSON: %s", body)
			}
		})
	}
}
