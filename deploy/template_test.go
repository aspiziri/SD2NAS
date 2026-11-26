package deploy

import (
	"encoding/xml"
	"os"
	"slices"
	"strings"
	"testing"
)

type templateConfig struct {
	Name     string `xml:"Name,attr"`
	Target   string `xml:"Target,attr"`
	Default  string `xml:"Default,attr"`
	Mode     string `xml:"Mode,attr"`
	Type     string `xml:"Type,attr"`
	Display  string `xml:"Display,attr"`
	Required string `xml:"Required,attr"`
	Mask     string `xml:"Mask,attr"`
	Value    string `xml:",chardata"`

	Description string `xml:"Description,attr"`
}

type template struct {
	Repository  string           `xml:"Repository"`
	TemplateURL string           `xml:"TemplateURL"`
	Icon        string           `xml:"Icon"`
	ExtraParams string           `xml:"ExtraParams"`
	Privileged  string           `xml:"Privileged"`
	WebUI       string           `xml:"WebUI"`
	Requires    string           `xml:"Requires"`
	Overview    string           `xml:"Overview"`
	Configs     []templateConfig `xml:"Config"`
}

func TestUnraidTemplate(t *testing.T) {
	raw, err := os.ReadFile("unraid/sd2nas.xml")
	if err != nil {
		t.Fatal(err)
	}
	var tpl template
	if err := xml.Unmarshal(raw, &tpl); err != nil {
		t.Fatalf("parse template: %v", err)
	}

	const raw_ = "https://raw.githubusercontent.com/aspiziri/SD2NAS/main/"
	equal := map[string][2]string{
		"Repository":  {tpl.Repository, "ghcr.io/aspiziri/sd2nas:edge"},
		"TemplateURL": {tpl.TemplateURL, raw_ + "deploy/unraid/sd2nas.xml"},
		"Icon":        {tpl.Icon, raw_ + "web/public/icon-512.png"},
		"ExtraParams": {tpl.ExtraParams, "--restart=unless-stopped"},
		"Privileged":  {tpl.Privileged, "false"},
		"WebUI":       {tpl.WebUI, "http://[IP]:[PORT:8080]/"},
	}
	for name, v := range equal {
		if v[0] != v[1] {
			t.Errorf("%s = %q, want %q", name, v[0], v[1])
		}
	}
	if _, err := os.Stat("../web/public/icon-512.png"); err != nil {
		t.Errorf("icon file: %v", err)
	}
	for _, want := range []string{"Unassigned Devices", "User Scripts"} {
		if !strings.Contains(tpl.Requires, want) {
			t.Errorf("Requires %q does not contain %q", tpl.Requires, want)
		}
	}
	if !strings.Contains(tpl.Overview, "https://github.com/aspiziri/SD2NAS#readme") {
		t.Errorf("Overview does not link the README: %q", tpl.Overview)
	}

	var targets []string
	byTarget := map[string]templateConfig{}
	for _, c := range tpl.Configs {
		targets = append(targets, c.Target)
		byTarget[c.Target] = c
		if c.Value != c.Default {
			t.Errorf("%s: value %q != default %q", c.Target, c.Value, c.Default)
		}
	}
	wantTargets := []string{"8080", "/cards", "/photos", "/config", "PUID", "PGID", "UMASK", "SD2NAS_POLL_CARDS", "SD2NAS_TOKEN"}
	if !slices.Equal(targets, wantTargets) {
		t.Fatalf("config targets = %v, want %v", targets, wantTargets)
	}

	check := func(target, typ, mode, def, display string) {
		c := byTarget[target]
		if c.Type != typ {
			t.Errorf("%s: Type %q, want %q", target, c.Type, typ)
		}
		if c.Mode != mode {
			t.Errorf("%s: Mode %q, want %q", target, c.Mode, mode)
		}
		if c.Default != def {
			t.Errorf("%s: Default %q, want %q", target, c.Default, def)
		}
		if display != "" && c.Display != display {
			t.Errorf("%s: Display %q, want %q", target, c.Display, display)
		}
	}
	check("8080", "Port", "tcp", "8080", "")
	check("/cards", "Path", "rw,slave", "/mnt/disks", "")
	check("/photos", "Path", "rw", "/mnt/user/photos", "")
	check("/config", "Path", "rw", "/mnt/user/appdata/sd2nas", "")
	check("PUID", "Variable", "", "99", "")
	check("PGID", "Variable", "", "100", "")
	check("UMASK", "Variable", "", "000", "")
	check("SD2NAS_POLL_CARDS", "Variable", "", "false", "advanced")
	check("SD2NAS_TOKEN", "Variable", "", "", "advanced")

	tok := byTarget["SD2NAS_TOKEN"]
	if tok.Mask != "true" {
		t.Errorf("SD2NAS_TOKEN: Mask %q, want true", tok.Mask)
	}
	if tok.Name != "Password (SD2NAS_TOKEN)" {
		t.Errorf("SD2NAS_TOKEN: Name %q", tok.Name)
	}
	for _, want := range []string{"sd2nas.cfg", "openssl rand -hex 24"} {
		if !strings.Contains(tok.Description, want) {
			t.Errorf("SD2NAS_TOKEN: description %q does not mention %q", tok.Description, want)
		}
	}
}
