package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// loadCursor returns the saved cursor, or 0 if path is empty or missing.
func loadCursor(path string) (uint64, error) {
	if path == "" {
		return 0, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// saveCursor writes the cursor atomically via a temp file + rename.
func saveCursor(path string, cursor uint64) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cursor-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strconv.FormatUint(cursor, 10) + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
