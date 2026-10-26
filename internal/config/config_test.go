package config

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseSupportedForms(t *testing.T) {
	c, err := Parse(strings.NewReader(`#cloud-config
bootcmd:
  - [printf, '%s', 'a b', '']
write_files:
  - path: /run/example/settings
    content: aGVsbG8K
    encoding: base64
    permissions: '0640'
    owner: '1000:1000'
    append: true
    defer: true
package_update: false
package_upgrade: true
packages:
  - curl
  - [example, '1.2-3']
runcmd:
  - 'printf "%s" "hello world"'
  - [printf, '%s', 'literal $HOME']
  - null
`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.BootCmd[0].Argv(), []string{"printf", "%s", "a b", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("argv changed: %#v", got)
	}
	f := c.WriteFiles[0]
	if string(f.Content) != "hello\n" || f.Permissions != 0640 || f.Owner != "1000:1000" || !f.Append || !f.Defer {
		t.Fatalf("unexpected file: %#v", f)
	}
	if !c.PackageUpgrade || c.PackageUpdate || c.Packages[1].Version != "1.2-3" {
		t.Fatalf("unexpected package config: %#v", c)
	}
	if !c.RunCmd[0].Shell || c.RunCmd[1].Shell || !c.RunCmd[2].Skip {
		t.Fatalf("unexpected command forms: %#v", c.RunCmd)
	}
}

func TestRejectUnsupportedAndMalformedBeforeExecution(t *testing.T) {
	cases := map[string]string{
		"missing header":    "runcmd: []",
		"empty document":    "#cloud-config\n",
		"sequence root":     "#cloud-config\n- runcmd",
		"multiple docs":     "#cloud-config\nruncmd: []\n---\nruncmd: []\n",
		"duplicate":         "#cloud-config\nruncmd: []\nruncmd: []\n",
		"unknown":           "#cloud-config\ndisk_setup: {}\n",
		"password type":     "#cloud-config\nchpasswd: {users: [{name: root, password: do-not-print-me, type: random}]}\n",
		"alias":             "#cloud-config\nbootcmd: &cmd [echo]\nruncmd: *cmd\n",
		"merge":             "#cloud-config\n<<: {runcmd: []}\n",
		"invalid bool":      "#cloud-config\npackage_update: 'true'\n",
		"null bool":         "#cloud-config\npackage_update: null\n",
		"command type":      "#cloud-config\nruncmd: [true]\n",
		"empty argv":        "#cloud-config\nruncmd: [[]]\n",
		"empty executable":  "#cloud-config\nruncmd: [['', x]]\n",
		"numeric arg":       "#cloud-config\nruncmd: [[echo, 123]]\n",
		"null bootcmd":      "#cloud-config\nbootcmd: [null]\n",
		"nul command":       "#cloud-config\nruncmd: [" + `"bad\0value"` + "]\n",
		"unknown file key":  "#cloud-config\nwrite_files: [{path: /run/x, source: {uri: https://example.com}}]\n",
		"file relative":     "#cloud-config\nwrite_files: [{path: relative}]\n",
		"file root":         "#cloud-config\nwrite_files: [{path: /}]\n",
		"missing file path": "#cloud-config\nwrite_files: [{content: hello}]\n",
		"invalid owner":     "#cloud-config\nwrite_files: [{path: /run/x, owner: root}]\n",
		"numeric mode":      "#cloud-config\nwrite_files: [{path: /run/x, permissions: 0644}]\n",
		"invalid mode":      "#cloud-config\nwrite_files: [{path: /run/x, permissions: '0999'}]\n",
		"invalid encoding":  "#cloud-config\nwrite_files: [{path: /run/x, encoding: secret-encoding}]\n",
		"bad base64":        "#cloud-config\nwrite_files: [{path: /run/x, encoding: b64, content: do-not-print-me}]\n",
		"duplicate nested":  "#cloud-config\nwrite_files: [{path: /run/x, content: a, content: b}]\n",
		"package option":    "#cloud-config\npackages: ['--allow-unauthenticated']\n",
		"package object":    "#cloud-config\npackages: [{snap: [x]}]\n",
		"package version":   "#cloud-config\npackages: [[curl, '--option']]\n",
		"unquoted version":  "#cloud-config\npackages: [[curl, 123]]\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(input))
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
			for _, secret := range []string{"do-not-print-me", "secret-encoding"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error disclosed scalar content: %v", err)
				}
			}
		})
	}
}

func TestSelectedTwelveKeysAreImplemented(t *testing.T) {
	if got := len(Keys()); got != 12 {
		t.Fatalf("expected selected 12-key scope, got %d", got)
	}
	for _, key := range Keys() {
		if !key.Implemented {
			t.Fatalf("selected key remains unimplemented: %s", key.Name)
		}
	}
}

func TestGzipContentAndLimits(t *testing.T) {
	compressed := func(text string) string {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		if _, err := w.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(b.Bytes())
	}
	for _, encoding := range []string{"gz+base64", "gzip+base64", "gz+b64", "gzip+b64"} {
		input := fmt.Sprintf("#cloud-config\nwrite_files:\n  - path: /run/x\n    encoding: %s\n    content: %s\n", encoding, compressed("hello\x00world"))
		c, err := Parse(strings.NewReader(input))
		if err != nil || string(c.WriteFiles[0].Content) != "hello\x00world" {
			t.Fatalf("%s: decode: %v", encoding, err)
		}
	}
	input := "#cloud-config\nwrite_files:\n  - path: /run/x\n    encoding: gzip\n    content: !!binary |\n      " + compressed("binary gzip") + "\n"
	c, err := Parse(strings.NewReader(input))
	if err != nil || string(c.WriteFiles[0].Content) != "binary gzip" {
		t.Fatalf("binary gzip decode: %v", err)
	}
	_, err = Parse(strings.NewReader("#cloud-config\n#" + strings.Repeat("x", MaxDocumentBytes)))
	if err == nil || !strings.Contains(err.Error(), "4 MiB") {
		t.Fatalf("document size limit: %v", err)
	}
	input = "#cloud-config\nwrite_files:\n  - path: /run/x\n    encoding: gzip+base64\n    content: " + compressed(strings.Repeat("x", MaxFileBytes+1)) + "\n"
	_, err = Parse(strings.NewReader(input))
	if err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Fatalf("decoded size limit: %v", err)
	}
}
