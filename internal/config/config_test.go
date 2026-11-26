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

func TestPollCards(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want bool
		err  string
	}{
		{name: "unset", want: false},
		{name: "true", env: map[string]string{"SD2NAS_POLL_CARDS": "true"}, want: true},
		{name: "one", env: map[string]string{"SD2NAS_POLL_CARDS": "1"}, want: true},
		{name: "false", env: map[string]string{"SD2NAS_POLL_CARDS": "false"}, want: false},
		{name: "zero", env: map[string]string{"SD2NAS_POLL_CARDS": "0"}, want: false},
		{name: "empty", env: map[string]string{"SD2NAS_POLL_CARDS": ""}, want: false},
		{name: "bad", env: map[string]string{"SD2NAS_POLL_CARDS": "maybe"}, err: `SD2NAS_POLL_CARDS "maybe" is not true or false`},
		{name: "flag beats env false", args: []string{"--poll-cards"}, env: map[string]string{"SD2NAS_POLL_CARDS": "false"}, want: true},
		{name: "flag false beats env true", args: []string{"--poll-cards=false"}, env: map[string]string{"SD2NAS_POLL_CARDS": "true"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(tc.args, env(tc.env))
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.PollCards != tc.want {
				t.Errorf("PollCards = %v, want %v", c.PollCards, tc.want)
			}
		})
	}
}

func TestLoadBadFlag(t *testing.T) {
	if _, err := Load([]string{"--nope"}, env(nil)); err == nil {
		t.Error("expected an error for an unknown flag")
	}
}

func TestTokenEnv(t *testing.T) {
	c, err := Load(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "" {
		t.Errorf("unset: Token = %q, want empty", c.Token)
	}
	for _, tok := range []string{"abc123", "0f3a9c41d2e8b7a6f5c4d3e2b1a0f9e8d7c6b5a4f3e2d1c0", "A-Z_a.z~+/="} {
		c, err := Load(nil, env(map[string]string{"SD2NAS_TOKEN": tok}))
		if err != nil {
			t.Errorf("%q: %v", tok, err)
			continue
		}
		if c.Token != tok {
			t.Errorf("Token = %q, want %q", c.Token, tok)
		}
	}
	const want = "SD2NAS_TOKEN may use only letters, digits and . _ ~ + / = -"
	for _, tok := range []string{"abc def", "a$b", "a'b", "a;b", "abc\x7f", "é"} {
		_, err := Load(nil, env(map[string]string{"SD2NAS_TOKEN": tok}))
		if err == nil || err.Error() != want {
			t.Errorf("%q: err = %v, want %q", tok, err, want)
		}
	}
}
