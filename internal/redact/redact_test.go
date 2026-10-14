package redact

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParams(t *testing.T) {
	var params map[string]any
	in := `{
		"settings": {"broker": "mqtt.lan", "username": "kvm", "password": "hunter2", "port": 1883},
		"state": {"mode": "custom", "certificate": "-----BEGIN CERTIFICATE-----", "privateKey": "-----BEGIN PRIVATE KEY-----"},
		"networks": [{"ssid": "home", "psk": "wifi-secret"}],
		"oldPassword": "a", "newPassword": "b", "token": "t", "OidcGoogle": "eyJ",
		"keys": [4, 5], "sshKey": "ssh-ed25519 AAAA",
		"url": "https://user:pw@images.lan/debian.iso", "mode": "CDROM", "email": "a@b.example"
	}`
	if err := json.Unmarshal([]byte(in), &params); err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	out := `{
		"settings": {"broker": "mqtt.lan", "username": "kvm", "password": "[redacted]", "port": 1883},
		"state": {"mode": "custom", "certificate": "-----BEGIN CERTIFICATE-----", "privateKey": "[redacted]"},
		"networks": [{"ssid": "home", "psk": "[redacted]"}],
		"oldPassword": "[redacted]", "newPassword": "[redacted]", "token": "[redacted]", "OidcGoogle": "[redacted]",
		"keys": [4, 5], "sshKey": "ssh-ed25519 AAAA",
		"url": "https://redacted@images.lan/debian.iso", "mode": "CDROM", "email": "a@b.example"
	}`
	if err := json.Unmarshal([]byte(out), &want); err != nil {
		t.Fatal(err)
	}

	got := Params(params)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Params =\n%v\nwant\n%v", got, want)
	}
	if params["settings"].(map[string]any)["password"] != "hunter2" {
		t.Fatal("Params modified its input")
	}
	if Params(nil) != nil {
		t.Fatal("Params(nil) is not nil")
	}
}
