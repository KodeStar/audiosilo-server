// Package jsonfile reads and writes the small JSON files the server keeps beside
// its database (the backups' restore marker and result, the metadata mirror's
// state). A write is all or nothing: a crash leaves the old file or the new one,
// never half of one.
package jsonfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// TempSuffix names the temporary file a write goes through (path+TempSuffix),
// so a folder's owner can recognise one a crash left behind.
const TempSuffix = ".tmp"

// Write stores v as JSON at path (owner-only): written to path+TempSuffix,
// synced to disk, then renamed over path. The sync comes before the rename so a
// power cut can't leave the new name pointing at a file whose bytes never
// reached the disk.
func Write(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + TempSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// Read reads the JSON file at path into a T: nil (and no error) when there is
// none, an error naming the file when it doesn't parse.
func Read[T any](path string) (*T, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return &v, nil
}
