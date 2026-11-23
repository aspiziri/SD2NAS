package config

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8080" || c.Cards != "/cards" || c.Photos != "/photos" || c.Data != "/config" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.Umask != nil {
		t.Errorf("Umask should be unset, got %o", *c.Umask)
	}
}

func TestLoadFlagsOverrideEnv(t *testing.T) {
	c, err := Load(
		[]string{"--photos", "./tmp/photos"},
		env(map[string]string{"SD2NAS_PHOTOS": "/env/photos", "SD2NAS_CARDS": "/env/cards"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.Photos != "./tmp/photos" {
		t.Errorf("flag should win, got %q", c.Photos)
	}
	if c.Cards != "/env/cards" {
		t.Errorf("env should apply when no flag, got %q", c.Cards)
	}
}

func TestLoadUmask(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"000", 0, true},
		{"022", 0o22, true},
		{"0002", 0o2, true},
		{"999", 0, false},
		{"1000", 0, false},
		{"abc", 0, false},
	} {
		c, err := Load(nil, env(map[string]string{"UMASK": tc.in}))
		if tc.ok != (err == nil) {
			t.Errorf("UMASK=%q: err=%v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && *c.Umask != tc.want {
			t.Errorf("UMASK=%q: got %o, want %o", tc.in, *c.Umask, tc.want)
		}
	}
}

func TestLoadBadFlag(t *testing.T) {
	if _, err := Load([]string{"--nope"}, env(nil)); err == nil {
		t.Error("expected an error for an unknown flag")
	}
}
