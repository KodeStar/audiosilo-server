//go:build !podcastdiscoverytest

package discovery

// testOriginBuild: an ordinary build ignores
// AUDIOSILO_TEST_PODCAST_DISCOVERY_ORIGIN and always asks the fixed directory
// origins.
const testOriginBuild = false
