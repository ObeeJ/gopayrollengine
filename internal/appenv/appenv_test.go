package appenv

import "testing"

func TestAllowsInsecureDefaults(t *testing.T) {
	cases := map[string]bool{
		"development": true,
		"test":        true,
		"staging":     false,
		"production":  false,
		"prod":        false, // a typo must fail closed, not fall back to dev
		"Production":  false,
		"":            true, // only because this is a go test binary
	}
	for env, want := range cases {
		t.Setenv("APP_ENV", env)
		if got := AllowsInsecureDefaults(); got != want {
			t.Errorf("APP_ENV=%q: AllowsInsecureDefaults() = %v, want %v", env, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, env := range []string{"development", "test", "staging", "production"} {
		t.Setenv("APP_ENV", env)
		if err := Validate(); err != nil {
			t.Errorf("APP_ENV=%q: unexpected error %v", env, err)
		}
	}
	for _, env := range []string{"", "prod", "dev", "PRODUCTION"} {
		t.Setenv("APP_ENV", env)
		if err := Validate(); err == nil {
			t.Errorf("APP_ENV=%q: expected an error", env)
		}
	}
}
