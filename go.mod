module github.com/kodestar/audiosilo-server

go 1.26.0

toolchain go1.26.8

// Keep Go tooling (./... in build/vet/test/lint) out of the admin console's
// npm dependencies: some packages ship Go source (e.g. flatted/golang).
ignore ./admin-ui/node_modules

require (
	github.com/dhowden/tag v0.0.0-20240417053706-3d75831295e8
	github.com/kodestar/audiosilo-meta v0.0.0-00010101000000-000000000000
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	golang.org/x/crypto v0.57.0
	golang.org/x/image v0.46.0
	golang.org/x/sys v0.48.0
	golang.org/x/text v0.42.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.60.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	golang.org/x/net v0.58.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

replace github.com/kodestar/audiosilo-meta => /private/tmp/claude-501/-Users-chris-dev-audiosilo/e8965070-8cdf-4f6a-b88b-ade18d70d5c0/scratchpad/meta
