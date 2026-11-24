// Package config holds the process configuration that comes from flags and
// environment variables. User-editable settings live in the catalog (later).
package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
)

// Config is the process configuration. Paths are container paths; on Unraid
// they are bind mounts set up by the template.
type Config struct {
	Addr   string // listen address, e.g. ":8080"
	Cards  string // where Unassigned Devices mounts cards (/mnt/disks on the host)
	Photos string // the photos share: Archive/, Staging/, Management/
	Data   string // database, thumbnail cache (/config in the container)
	Umask  *int   // applied at startup when set (UMASK env, octal)

	PollCards bool // import cards that appear under Cards without the host script
}

// Load reads configuration from env first, then lets flags override it.
// getenv is os.Getenv in production and a map lookup in tests.
func Load(args []string, getenv func(string) string) (Config, error) {
	c := Config{
		Addr:   envOr(getenv, "SD2NAS_ADDR", ":8080"),
		Cards:  envOr(getenv, "SD2NAS_CARDS", "/cards"),
		Photos: envOr(getenv, "SD2NAS_PHOTOS", "/photos"),
		Data:   envOr(getenv, "SD2NAS_CONFIG", "/config"),
	}

	// Parsed before the flags so that --poll-cards overrides it.
	if s := getenv("SD2NAS_POLL_CARDS"); s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return Config{}, fmt.Errorf("SD2NAS_POLL_CARDS %q is not true or false", s)
		}
		c.PollCards = b
	}

	fs := flag.NewFlagSet("sd2nas", flag.ContinueOnError)
	fs.SetOutput(os.Stderr) // usage and parse errors
	fs.StringVar(&c.Addr, "addr", c.Addr, "listen address (env SD2NAS_ADDR)")
	fs.StringVar(&c.Cards, "cards", c.Cards, "card mount root (env SD2NAS_CARDS)")
	fs.StringVar(&c.Photos, "photos", c.Photos, "photos share root (env SD2NAS_PHOTOS)")
	fs.StringVar(&c.Data, "config", c.Data, "database and cache directory (env SD2NAS_CONFIG)")
	fs.BoolVar(&c.PollCards, "poll-cards", c.PollCards, "import cards that appear under the cards folder without the host script (env SD2NAS_POLL_CARDS)")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if s := getenv("UMASK"); s != "" {
		m, err := strconv.ParseUint(s, 8, 32)
		if err != nil || m > 0o777 {
			return Config{}, fmt.Errorf("UMASK %q is not an octal value between 000 and 777", s)
		}
		v := int(m)
		c.Umask = &v
	}
	return c, nil
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}
