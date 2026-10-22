package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	keyCanary    = "PI_KEY_CANARY_2C3"
	secretCanary = "PI_SECRET_CANARY_2C3"
)

// Discovery is on by default, read from config.yaml or its variable, and has
// no console setting.
func TestPodcastDiscoveryEnabled(t *testing.T) {
	if !Default("/data").Podcasts.Discovery.Enabled {
		t.Fatal("discovery must default on")
	}
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("podcasts:\n  discovery:\n    enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(dir)
	if err != nil || cfg.Podcasts.Discovery.Enabled {
		t.Fatalf("yaml off = %v %v", cfg.Podcasts.Discovery.Enabled, err)
	}
	t.Setenv("AUDIOSILO_PODCAST_DISCOVERY_ENABLED", "true")
	cfg, _, err = Load(dir)
	if err != nil || !cfg.Podcasts.Discovery.Enabled {
		t.Fatalf("env on = %v %v", cfg.Podcasts.Discovery.Enabled, err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(Path(dir))
	if !strings.Contains(string(raw), "enabled: false") {
		t.Fatalf("the environment's value reached config.yaml:\n%s", raw)
	}
	for section, values := range cfg.Settings() {
		for name := range values {
			if strings.Contains(section+"."+name, "discovery") || strings.Contains(section+"."+name, "index") {
				t.Fatalf("console setting %s.%s", section, name)
			}
		}
	}
}

// The Podcast Index pair comes from the environment only and is never
// serialized: not by Save (config.yaml), JSON, the console's settings or their
// change list, whatever else is saved.
func TestPodcastIndexCredentialsNeverSerialized(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvPodcastIndexKey, keyCanary)
	t.Setenv(EnvPodcastIndexSecret, secretCanary)
	cfg, _, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, secret, ok, problem := cfg.Podcasts.Discovery.PodcastIndex()
	if !ok || key != keyCanary || secret != secretCanary || problem != "" {
		t.Fatalf("pair = %v %q", ok, problem)
	}
	next := cfg.Clone()
	next.Name = "Renamed"
	if err := next.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(Path(dir))
	js, _ := json.Marshal(next)
	ym, _ := yaml.Marshal(next.asSaved())
	settings, _ := json.Marshal(next.Settings())
	changes, _ := json.Marshal(ChangedSettings(cfg, next))
	for name, out := range map[string]string{"file": string(raw), "json": string(js), "yaml": string(ym), "settings": string(settings), "changes": string(changes), "printed": fmt.Sprintf("%v", next.Podcasts)} {
		if name == "printed" {
			// A plain %v of the struct would show the value; the server never
			// prints it, and this guards that the fields are where expected.
			if !strings.Contains(out, keyCanary) {
				t.Fatal("the pair is not where the test looks")
			}
			continue
		}
		if strings.Contains(out, keyCanary) || strings.Contains(out, secretCanary) {
			t.Fatalf("%s holds a credential", name)
		}
	}
	// A file that names them is ignored: yaml:"-".
	if err := os.WriteFile(Path(dir), []byte("podcasts:\n  discovery:\n    index_key: FILE_KEY\n    IndexKey: FILE_KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPodcastIndexKey, "")
	t.Setenv(EnvPodcastIndexSecret, "")
	cfg, _, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Podcasts.Discovery.IndexKey != "" {
		t.Fatal("config.yaml set the key")
	}
}

// An incomplete or malformed pair disables Podcast Index only, with a reason
// naming the variable and never its value; an absent pair is just unconfigured.
func TestPodcastIndexValidation(t *testing.T) {
	long := strings.Repeat("k", 257)
	for _, tc := range []struct {
		name, key, secret string
		ok                bool
		problem           string
	}{
		{"absent", "", "", false, ""},
		{"key only", keyCanary, "", false, EnvPodcastIndexSecret + " is not set"},
		{"secret only", "", secretCanary, false, EnvPodcastIndexKey + " is not set"},
		{"space", "a b" + keyCanary, secretCanary, false, EnvPodcastIndexKey + " must be"},
		{"long", long, secretCanary, false, EnvPodcastIndexKey + " must be"},
		{"non-ascii", keyCanary, "sé" + secretCanary, false, EnvPodcastIndexSecret + " must be"},
		{"control", keyCanary, "s\x01" + secretCanary, false, EnvPodcastIndexSecret + " must be"},
		{"max", strings.Repeat("k", 256), "!~" + secretCanary, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := PodcastDiscoveryConfig{IndexKey: tc.key, IndexSecret: tc.secret}
			key, secret, ok, problem := d.PodcastIndex()
			if ok != tc.ok || !strings.HasPrefix(problem, tc.problem) || (tc.problem == "") != (problem == "") {
				t.Fatalf("= %v %q", ok, problem)
			}
			if strings.Contains(problem, keyCanary) || strings.Contains(problem, secretCanary) {
				t.Fatal("the problem echoes a value")
			}
			if ok && (key != tc.key || secret != tc.secret) {
				t.Fatal("bytes changed")
			}
			if !ok && (key != "" || secret != "") {
				t.Fatal("a refused pair was returned")
			}
		})
	}
}
