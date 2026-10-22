//go:build podcastdiscoverytest

package discovery

// testOriginBuild: this binary was built with the podcastdiscoverytest tag, so
// AUDIOSILO_TEST_PODCAST_DISCOVERY_ORIGIN is honoured (live verification
// against a local fixture server only; never a release build).
const testOriginBuild = true
