package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() map[string]any {
	return map[string]any{"nodeUrl": "ws://127.0.0.1:35998", "chainId": 1, "delegations": []map[string]any{{"pillarName": "A", "follow": "B", "producerKeyFile": "producer", "passwordEnv": "AZ_TEST_SECRET"}}}
}
func TestConfigDefaultsAndValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]any)
		bad  bool
	}{
		{"defaults", func(map[string]any) {}, false},
		{"delay disabled", func(m map[string]any) { m["copyDelayMinutes"] = 0 }, false},
		{"delay enabled", func(m map[string]any) { m["copyDelayMinutes"] = 10080 }, false},
		{"negative delay", func(m map[string]any) { m["copyDelayMinutes"] = -1 }, true},
		{"excessive delay", func(m map[string]any) { m["copyDelayMinutes"] = 10081 }, true},
		{"fractional delay", func(m map[string]any) { m["copyDelayMinutes"] = 0.5 }, true},
		{"missing chain", func(m map[string]any) { delete(m, "chainId") }, true},
		{"bad scheme", func(m map[string]any) { m["nodeUrl"] = "file:///tmp/node" }, true},
		{"negative interval", func(m map[string]any) { m["pollIntervalSeconds"] = -1 }, true},
		{"overflow timeout", func(m map[string]any) { m["powTimeoutSeconds"] = 1e12 }, true},
		{"unknown field", func(m map[string]any) { m["password"] = "do-not-log" }, true},
		{"no delegations", func(m map[string]any) { m["delegations"] = nil }, true},
		{"state equals key", func(m map[string]any) { m["statePath"] = "producer" }, true},
		{"state equals config", func(m map[string]any) { m["statePath"] = "config.json" }, true},
		{"two password sources", func(m map[string]any) { m["delegations"].([]map[string]any)[0]["passwordFile"] = "password" }, true},
		{"duplicate owner", func(m map[string]any) {
			ds := m["delegations"].([]map[string]any)
			m["delegations"] = append(ds, ds[0])
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := validConfig()
			tc.edit(m)
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err = os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if (err != nil) != tc.bad {
				t.Fatalf("config=%+v error=%v", c, err)
			}
			if err != nil && strings.Contains(err.Error(), "do-not-log") {
				t.Fatal("secret leaked")
			}
			if !tc.bad && (c.PollIntervalSeconds != 30 || c.PowTimeoutSeconds != 300 || c.StatePath != filepath.Join(dir, "state.json") || c.Delegations[0].ProducerKeyFile != filepath.Join(dir, "producer")) {
				t.Fatal(c)
			}
		})
	}
}
func TestPasswordSources(t *testing.T) {
	t.Setenv("AZ_TEST_SECRET", " spaces remain ")
	v, err := Password(Delegation{PasswordEnv: "AZ_TEST_SECRET"})
	if err != nil || v != " spaces remain " {
		t.Fatal(v, err)
	}
	t.Setenv("AZ_TEST_SECRET", "")
	if _, err = Password(Delegation{PasswordEnv: "AZ_TEST_SECRET"}); err == nil {
		t.Fatal("empty password accepted")
	}
	path := filepath.Join(t.TempDir(), "secret")
	if err = os.WriteFile(path, []byte(" spaces remain \r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err = Password(Delegation{PasswordFile: path})
	if err != nil || v != " spaces remain " {
		t.Fatal(v, err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = Password(Delegation{PasswordFile: path}); err == nil {
		t.Fatal("public password accepted")
	}
	link := path + ".link"
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = Password(Delegation{PasswordFile: link}); err == nil {
		t.Fatal("symlink accepted")
	}
}
func TestFollowAddress(t *testing.T) {
	if _, ok := FollowAddress("z1qrw2vecdddmft89xh6yqencmjergsshhfmks0l"); !ok {
		t.Fatal("address not recognised")
	}
	for _, name := range []string{"TrustedPillar", "z1qrw2vecdddmft89xh6yqencmjergsshhfmks0x", ""} {
		if _, ok := FollowAddress(name); ok {
			t.Fatalf("%q taken for an address", name)
		}
	}
}
