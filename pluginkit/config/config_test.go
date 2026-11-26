package config

import "testing"

func TestDecode(t *testing.T) {
	var cfg struct {
		Name string `yaml:"name"`
	}
	if errDecode := Decode([]byte(`{"config_yaml":"bmFtZTogZGVtbw=="}`), &cfg); errDecode != nil || cfg.Name != "demo" {
		t.Errorf("Decode = %+v, %v", cfg, errDecode)
	}
	cfg.Name = "kept"
	if errDecode := Decode(nil, &cfg); errDecode != nil || cfg.Name != "kept" {
		t.Errorf("empty Decode = %+v, %v", cfg, errDecode)
	}
	if errDecode := Decode([]byte("{"), &cfg); errDecode == nil {
		t.Error("bad request accepted")
	}
	if errDecode := Decode([]byte(`{"config_yaml":"Wwo="}`), &cfg); errDecode == nil {
		t.Error("bad YAML accepted")
	}
}

func TestURL(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{" https://example.test/api/ ", "https://example.test/api", true},
		{"ftp://example.test", "", false},
		{"example.test", "", false},
	}
	for _, c := range cases {
		got, errURL := URL("base_url", c.in)
		if got != c.want || (errURL == nil) != c.ok {
			t.Errorf("URL(%q) = %q, %v", c.in, got, errURL)
		}
	}
}

func TestModelPrefix(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{" /zed/ ", "zed", true},
		{"a b", "", false},
		{"a*", "", false},
	}
	for _, c := range cases {
		got, errPrefix := ModelPrefix("model_prefix", c.in)
		if got != c.want || (errPrefix == nil) != c.ok {
			t.Errorf("ModelPrefix(%q) = %q, %v", c.in, got, errPrefix)
		}
	}
}
