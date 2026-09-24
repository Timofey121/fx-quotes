package config

import (
	"net/url"
	"testing"
)

func TestRemoteDatabaseComponentsVerifyServerByDefault(t *testing.T) {
	for _, host := range []string{"db.example", "postgres", "10.0.0.1", "localhost", "127.0.0.1.example", "::ffff:192.0.2.1"} {
		t.Run(host, func(t *testing.T) {
			cfg, err := LoadFromLookup(func(key string) (string, bool) { return host, key == "FX_QUOTES_DATABASE_HOST" })
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(cfg.DatabaseURL)
			if err != nil || u.Query().Get("sslmode") != "verify-full" {
				t.Fatal("remote database does not require verified TLS")
			}
		})
	}
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "::1", "::ffff:127.0.0.1"} {
		t.Run(host, func(t *testing.T) {
			_, err := LoadFromLookup(func(key string) (string, bool) { return host, key == "FX_QUOTES_DATABASE_HOST" })
			if err != nil {
				t.Fatalf("literal loopback rejected: %v", err)
			}
		})
	}
}

func TestDatabaseTransportOverride(t *testing.T) {
	for _, tt := range []struct{ host, trusted, mode string }{
		{"postgres", "postgres", "disable"},
		{"db.example", "postgres", "verify-full"},
		{"postgres.example", "postgres", "verify-full"},
		{"db.example", "*", "verify-full"},
		{"db.example", "", "verify-full"},
	} {
		t.Run(tt.host+"/"+tt.trusted, func(t *testing.T) {
			cfg, err := LoadFromLookup(func(key string) (string, bool) {
				switch key {
				case "FX_QUOTES_DATABASE_HOST":
					return tt.host, true
				case "FX_QUOTES_DATABASE_INSECURE_HOST":
					return tt.trusted, true
				default:
					return "", false
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(cfg.DatabaseURL)
			if u.Query().Get("sslmode") != tt.mode {
				t.Fatal("explicit transport policy ignored")
			}
		})
	}
}

func TestDatabaseComponentsPreserveSpecialCharacters(t *testing.T) {
	values := map[string]string{
		"FX_QUOTES_DATABASE_HOST": "::1", "FX_QUOTES_DATABASE_PORT": "55448",
		"FX_QUOTES_DATABASE_USER": "user@name", "FX_QUOTES_DATABASE_PASSWORD": "local/test?pass#%$'",
		"FX_QUOTES_DATABASE_NAME": "quotes/name",
	}
	cfg, err := LoadFromLookup(func(key string) (string, bool) { v, ok := values[key]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if u.Host != "[::1]:55448" || u.User.Username() != "user@name" || password != "local/test?pass#%$'" || u.Path != "/quotes/name" || u.Query().Get("sslmode") != "disable" || u.Fragment != "" {
		t.Fatal("database components were not preserved")
	}
	values["FX_QUOTES_DATABASE_URL"] = "postgres://explicit/database?sslmode=require"
	cfg, err = LoadFromLookup(func(key string) (string, bool) { v, ok := values[key]; return v, ok })
	if err != nil || cfg.DatabaseURL != values["FX_QUOTES_DATABASE_URL"] {
		t.Fatal("explicit database URL did not take precedence")
	}
}

func TestDatabaseComponentsRejectInvalidAddress(t *testing.T) {
	for key, values := range map[string][]string{
		"FX_QUOTES_DATABASE_HOST": {"", "host/path", "host?query", "[::1]"},
		"FX_QUOTES_DATABASE_PORT": {"", "0", "65536", "bad"},
		"FX_QUOTES_DATABASE_USER": {""},
		"FX_QUOTES_DATABASE_NAME": {""},
	} {
		for _, value := range values {
			t.Run(key+"="+value, func(t *testing.T) {
				_, err := LoadFromLookup(func(k string) (string, bool) { return value, k == key })
				if err == nil {
					t.Fatal("invalid database component accepted")
				}
			})
		}
	}
}
