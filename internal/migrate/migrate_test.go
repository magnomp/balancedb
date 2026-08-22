package migrate

import "testing"

// TestLoadMigrations checks the embedded set parses, orders, and includes the
// initial migration. No database required — this runs under `make test`.
func TestLoadMigrations(t *testing.T) {
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("expected at least one embedded migration")
	}

	// Ascending, gap-free-from-1 not required, but strictly increasing.
	for i := 1; i < len(migs); i++ {
		if migs[i].version <= migs[i-1].version {
			t.Fatalf("migrations not strictly ascending: %d then %d", migs[i-1].version, migs[i].version)
		}
	}
	if migs[0].version != 1 {
		t.Fatalf("first migration version = %d, want 1", migs[0].version)
	}
	if migs[0].sql == "" {
		t.Fatal("first migration has empty SQL body")
	}
}

func TestParseVersion(t *testing.T) {
	ok := map[string]int{
		"0001_init.sql":         1,
		"0042_add_thing.sql":    42,
		"10_x.sql":              10,
		"0003_multi_word_x.sql": 3,
	}
	for name, want := range ok {
		got, err := parseVersion(name)
		if err != nil {
			t.Errorf("parseVersion(%q): unexpected error %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("parseVersion(%q) = %d, want %d", name, got, want)
		}
	}

	bad := []string{
		"init.sql",      // no numeric prefix
		"_init.sql",     // empty prefix
		"abc_init.sql",  // non-numeric prefix
		"0000_init.sql", // non-positive
		"-1_init.sql",   // negative
	}
	for _, name := range bad {
		if _, err := parseVersion(name); err == nil {
			t.Errorf("parseVersion(%q): expected error, got nil", name)
		}
	}
}
