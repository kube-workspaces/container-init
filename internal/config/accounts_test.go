package config

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
)

func TestSSHSupportedWireFormats(t *testing.T) {
	wire := func(fields ...[]byte) []byte {
		var out []byte
		for _, field := range fields {
			out = binary.BigEndian.AppendUint32(out, uint32(len(field)))
			out = append(out, field...)
		}
		return out
	}
	cases := map[string][]byte{
		"ssh-ed25519":                wire([]byte("ssh-ed25519"), make([]byte, 32)),
		"ssh-rsa":                    wire([]byte("ssh-rsa"), []byte{1, 0, 1}, []byte{0, 0x81, 1}),
		"sk-ssh-ed25519@openssh.com": wire([]byte("sk-ssh-ed25519@openssh.com"), make([]byte, 32), []byte("ssh:test")),
	}
	for name, curve := range map[string]ecdh.Curve{"nistp256": ecdh.P256(), "nistp384": ecdh.P384(), "nistp521": ecdh.P521()} {
		key, err := curve.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		kind := "ecdsa-sha2-" + name
		cases[kind] = wire([]byte(kind), []byte(name), key.PublicKey().Bytes())
		if name == "nistp256" {
			kind = "sk-ecdsa-sha2-nistp256@openssh.com"
			cases[kind] = wire([]byte(kind), []byte(name), key.PublicKey().Bytes(), []byte("ssh:test"))
		}
	}
	for kind, blob := range cases {
		if !validSSHBlob(kind, blob) {
			t.Fatalf("supported wire format rejected: %s", kind)
		}
		if validSSHBlob(kind, append(blob, 0)) {
			t.Fatalf("trailing data accepted: %s", kind)
		}
	}
}

func TestSSHRejectsTruncatedAndUnsupportedWireKeys(t *testing.T) {
	for _, kind := range []string{"ssh-ed25519", "ssh-rsa", "ecdsa-sha2-invalid", "sk-ssh-ed25519@openssh.com"} {
		blob := binary.BigEndian.AppendUint32(nil, uint32(len(kind)))
		blob = append(blob, kind...)
		blob = append(blob, 0)
		input := "#cloud-config\nssh_authorized_keys: ['" + kind + " " + base64.StdEncoding.EncodeToString(blob) + " do-not-print-me']\n"
		if _, err := Parse(strings.NewReader(input)); err == nil || strings.Contains(err.Error(), "do-not-print-me") {
			t.Fatal("invalid wire key accepted or leaked")
		}
	}
}

func TestAccountOptionsAndValidation(t *testing.T) {
	c, err := Parse(strings.NewReader(`#cloud-config
groups: [developers, {observers: [root]}]
users:
  - default
  - name: developer
    uid: 1234
    homedir: /home/developer
    shell: /bin/sh
    primary_group: developers
    groups: 'developers,observers'
    system: false
    no_create_home: false
    lock_passwd: true
chpasswd:
  expire: false
  users:
    - {name: developer, password: do-not-print-me, type: text}
timezone: Etc/UTC
ca_certs: {remove_defaults: false, trusted: []}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Groups) != 2 || len(c.Users) != 2 || !c.Users[0].Default || *c.Users[1].UID != 1234 || len(c.Users[1].Groups) != 2 || c.Passwords[0].Hashed || c.ExpirePasswords || c.Timezone != "Etc/UTC" {
		t.Fatal("account configuration was not preserved")
	}
	for _, body := range []string{
		"users: [{name: '--option'}]", "users: [{name: test, uid: 0}]", "users: [{name: test, uid: '123'}]",
		"users: [{name: test, sudo: ALL}]", "users: [{name: test, homedir: /home/../root}]", "users: [{name: test, lock_passwd: 'false'}]",
		"users: [test, test]", "groups: [test, test]", "groups: [{test: ['--option']}]",
		"chpasswd: {list: do-not-print-me}", "chpasswd: {users: [{name: test, password: do-not-print-me}]}",
		"chpasswd: {users: [{name: test, password: 'do-not-print-me:bad', type: text}]}",
		"chpasswd: {users: [{name: test, password: do-not-print-me, type: RANDOM}]}",
		"ca_certs: {remove_defaults: true}", "ca_certs: {trusted: [do-not-print-me]}",
		"timezone: ../etc/passwd", "timezone: /etc/passwd", "timezone: ''",
		"ssh_authorized_keys: ['do-not-print-me']", "ssh_authorized_keys: ['ssh-ed25519 bad']",
		"users: [{name: test, ssh_authorized_keys: [], unknown: do-not-print-me}]",
	} {
		_, err := Parse(strings.NewReader("#cloud-config\n" + body + "\n"))
		if err == nil {
			t.Fatalf("accepted invalid account configuration: %s", body)
		}
		if strings.Contains(err.Error(), "do-not-print-me") {
			t.Fatal("scalar content leaked")
		}
	}
}
