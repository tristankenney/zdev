package main

// rigwatch_test.go — tests for the rigs.json reader + fsnotify wrapper
// (zd-l2t). Skips fsnotify-end-to-end (kqueue + Linux divergence) and
// exercises the pure parse/path functions which carry the bulk of the
// logic.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRigsJSON(t *testing.T) {
	t.Run("valid file → prefix map", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "rigs.json")
		body := `{
  "version": 1,
  "rigs": {
    "zdev": {"git_url": "ignored", "beads": {"prefix": "zd"}},
    "town": {"git_url": "ignored", "beads": {"prefix": "hq"}}
  }
}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := readRigsJSON(path)
		if err != nil {
			t.Fatalf("readRigsJSON: %v", err)
		}
		if got["zd"] != "zdev" {
			t.Errorf("got[zd] = %q; want zdev", got["zd"])
		}
		if got["hq"] != "town" {
			t.Errorf("got[hq] = %q; want town", got["hq"])
		}
		if len(got) != 2 {
			t.Errorf("len(got) = %d; want 2 (extra entries: %+v)", len(got), got)
		}
	})

	t.Run("missing prefix is skipped, not an error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "rigs.json")
		body := `{"rigs": {"zdev": {"beads": {"prefix": ""}}}}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := readRigsJSON(path)
		if err != nil {
			t.Fatalf("readRigsJSON: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got = %+v; want empty map", got)
		}
	})

	t.Run("missing file returns error", func(t *testing.T) {
		_, err := readRigsJSON(filepath.Join(t.TempDir(), "does-not-exist.json"))
		if err == nil {
			t.Error("readRigsJSON: want error for missing file, got nil")
		}
	})

	t.Run("malformed json returns error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "rigs.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := readRigsJSON(path); err == nil {
			t.Error("readRigsJSON: want error for malformed json, got nil")
		}
	})
}

func TestRigsJSONPath(t *testing.T) {
	t.Run("opt-out wins over GT_TOWN_ROOT", func(t *testing.T) {
		t.Setenv("GT_TOWN_ROOT", "/some/path")
		t.Setenv("ZDEV_GT_TOWN_ROOT", "off")
		if got := rigsJSONPath(); got != "" {
			t.Errorf("rigsJSONPath() = %q; want empty (ZDEV_GT_TOWN_ROOT=off)", got)
		}
	})

	t.Run("GT_TOWN_ROOT unset → empty", func(t *testing.T) {
		t.Setenv("GT_TOWN_ROOT", "")
		t.Setenv("ZDEV_GT_TOWN_ROOT", "")
		if got := rigsJSONPath(); got != "" {
			t.Errorf("rigsJSONPath() = %q; want empty (GT_TOWN_ROOT unset)", got)
		}
	})

	t.Run("GT_TOWN_ROOT set → <root>/rigs.json", func(t *testing.T) {
		t.Setenv("GT_TOWN_ROOT", "/home/me/gt")
		t.Setenv("ZDEV_GT_TOWN_ROOT", "")
		if got := rigsJSONPath(); got != "/home/me/gt/rigs.json" {
			t.Errorf("rigsJSONPath() = %q; want /home/me/gt/rigs.json", got)
		}
	})
}
