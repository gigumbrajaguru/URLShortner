package main

import "testing"

// setEnv sets every config variable, using "" (treated as unset) for any
// key not in vars.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, k := range []string{"PORT", "BASE_URL", "STORE_PATH", "AD_COUNTDOWN", "TIMEZONE",
		"MONGODB_URI", "MONGODB_DB", "MONGODB_COLLECTION"} {
		t.Setenv(k, vars[k])
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	setEnv(t, nil)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "8182" || cfg.StorePath != "urls.json" || cfg.AdCountdown != 5 ||
		cfg.BaseURLOverride || cfg.MongoURI != "" || cfg.MongoDB != "urlshortner" ||
		cfg.MongoCollection != "urls" || cfg.Location.String() != "Asia/Colombo" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	setEnv(t, map[string]string{
		"PORT": "9000", "BASE_URL": "https://sho.rt/", "STORE_PATH": "/data/urls.json",
		"AD_COUNTDOWN": "8", "TIMEZONE": "UTC", "MONGODB_URI": "mongodb://db:27017",
		"MONGODB_DB": "links", "MONGODB_COLLECTION": "short",
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "9000" || cfg.BaseURL != "https://sho.rt" || !cfg.BaseURLOverride ||
		cfg.StorePath != "/data/urls.json" || cfg.AdCountdown != 8 || cfg.Location.String() != "UTC" ||
		cfg.MongoURI != "mongodb://db:27017" || cfg.MongoDB != "links" || cfg.MongoCollection != "short" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadConfigInvalidCountdownFallsBack(t *testing.T) {
	for _, v := range []string{"0", "-3", "abc"} {
		setEnv(t, map[string]string{"AD_COUNTDOWN": v})
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AdCountdown != 5 {
			t.Errorf("AD_COUNTDOWN=%q: got %d, want default 5", v, cfg.AdCountdown)
		}
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	for _, vars := range []map[string]string{
		{"BASE_URL": "sho.rt"},
		{"BASE_URL": "ftp://sho.rt"},
		{"BASE_URL": "https://"},
		{"TIMEZONE": "Mars/Olympus_Mons"},
	} {
		setEnv(t, vars)
		if _, err := loadConfig(); err == nil {
			t.Errorf("%v: want error", vars)
		}
	}
}
