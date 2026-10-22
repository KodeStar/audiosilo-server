# Phase 2c-3 shared contract fixtures

All values are synthetic. The canaries and signing credentials are test strings.
See ../plan/plan.md for the normative API, normalization, safety and verification rules.
JSON objects are compared structurally; export-expected.opml is byte-exact UTF-8 with LF endings.
The provider samples model successful upstream responses plus invalid and duplicate rows.
Numeric provider ids must be decoded losslessly, including 9007199254740993.
The 1,048,577-byte huge.opml exceeds the import cap by one byte. Never expand entity fixtures.
The duplicate import golden assumes import-context.json and one queued private request numbered 81.
The queue-cap sample assumes no existing shows or jobs: first five rows queue; rows six and seven are limited.
Copy this directory byte-identical to server internal/podcast/testdata/phase2c3/ and docs testdata/phase2c3/.
The Bridge has no copy and no change in this phase. The player takes the wire samples in 2d.
manifest.json records every other file's length and SHA-256; it does not hash itself.
