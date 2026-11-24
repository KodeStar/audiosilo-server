module github.com/kodestar/audiosilo-server

go 1.26.0

toolchain go1.26.11

// Keep Go tooling (./... in build/vet/test/lint) out of the admin console's
// npm dependencies: some packages ship Go source (e.g. flatted/golang).
ignore ./admin-ui/node_modules

require (
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/dhowden/tag v0.0.0-20240417053706-3d75831295e8
	github.com/go-jose/go-jose/v4 v4.1.9
	github.com/kodestar/audiosilo-meta v0.27.1-0.20261124184937-df3a5311999e
	github.com/mattn/go-sqlite3 v1.14.53
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	golang.org/x/crypto v0.59.0
	golang.org/x/image v0.48.0
	golang.org/x/oauth2 v0.38.0
	golang.org/x/sys v0.50.0
	golang.org/x/text v0.44.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.62.0
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	golang.org/x/net v0.62.0 // indirect
	modernc.org/libc v1.80.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
