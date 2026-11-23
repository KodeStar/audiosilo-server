package store

// OpenBefore lets the store_test package exercise catalog startup work that
// follows a migration (migration_0094_test.go), which store cannot import.
var OpenBefore = openBefore
