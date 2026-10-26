package config

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Group struct {
	Name    string
	Members []string
}

type User struct {
	Name         string
	Default      bool
	UID          *uint32
	Home         string
	Shell        string
	PrimaryGroup string
	Groups       []string
	System       bool
	NoCreateHome bool
	LockPasswd   *bool
	SSHKeys      []string
}

type Password struct {
	Name   string
	Value  string
	Hashed bool
}

var accountName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
var cryptHash = regexp.MustCompile(`^\$(1|2a|2b|2y|5|6|y|gy)\$[A-Za-z0-9./=,-]+\$[A-Za-z0-9./$=,-]+$`)

func ValidAccountName(name string) bool { return accountName.MatchString(name) }

func options(n *yaml.Node, where string, allowed ...string) (map[string]*yaml.Node, error) {
	f, err := mapping(n, where)
	if err != nil {
		return nil, err
	}
	for key := range f {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("line %d: unsupported %s option", n.Line, where)
		}
	}
	return f, nil
}

func names(n *yaml.Node, where string) ([]string, error) {
	var result []string
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		result = strings.Split(n.Value, ",")
	} else if n.Kind == yaml.SequenceNode {
		for _, v := range n.Content {
			s, err := text(v, where)
			if err != nil {
				return nil, err
			}
			result = append(result, s)
		}
	} else {
		return nil, fmt.Errorf("%s must be a name list or comma-separated string", where)
	}
	for i, s := range result {
		result[i] = strings.TrimSpace(s)
		if !ValidAccountName(result[i]) {
			return nil, fmt.Errorf("%s has an invalid account name", where)
		}
	}
	return result, nil
}

func accountConfig(c *Config, f map[string]*yaml.Node) error {
	if n := f["groups"]; n != nil {
		if n.Kind != yaml.SequenceNode {
			return errors.New("groups must be a list")
		}
		seen := map[string]bool{}
		for i, entry := range n.Content {
			where := fmt.Sprintf("groups[%d]", i)
			var g Group
			if entry.Kind == yaml.ScalarNode && entry.Tag == "!!str" {
				g.Name = entry.Value
			} else {
				m, err := mapping(entry, where)
				if err != nil || len(m) != 1 {
					return fmt.Errorf("%s must be a name or single-name membership mapping", where)
				}
				for name, members := range m {
					g.Name = name
					g.Members, err = names(members, where+" members")
					if err != nil {
						return err
					}
				}
			}
			if !ValidAccountName(g.Name) || seen[g.Name] {
				return fmt.Errorf("%s has an invalid or repeated group name", where)
			}
			seen[g.Name] = true
			c.Groups = append(c.Groups, g)
		}
	}
	if n := f["users"]; n != nil {
		if n.Kind != yaml.SequenceNode {
			return errors.New("users must be a list")
		}
		seen := map[string]bool{}
		for i, entry := range n.Content {
			where := fmt.Sprintf("users[%d]", i)
			u := User{}
			if entry.Kind == yaml.ScalarNode && entry.Tag == "!!str" {
				u.Name, u.Default = entry.Value, entry.Value == "default"
			} else {
				m, err := options(entry, where, "name", "uid", "homedir", "shell", "primary_group", "groups", "system", "no_create_home", "lock_passwd", "ssh_authorized_keys")
				if err != nil {
					return err
				}
				for key, dst := range map[string]*string{"name": &u.Name, "homedir": &u.Home, "shell": &u.Shell, "primary_group": &u.PrimaryGroup} {
					if v := m[key]; v != nil {
						*dst, err = text(v, where+"."+key)
						if err != nil {
							return err
						}
					}
				}
				for key, dst := range map[string]*bool{"system": &u.System, "no_create_home": &u.NoCreateHome} {
					if v := m[key]; v != nil {
						*dst, err = boolean(v, where+"."+key)
						if err != nil {
							return err
						}
					}
				}
				if v := m["lock_passwd"]; v != nil {
					b, e := boolean(v, where+".lock_passwd")
					if e != nil {
						return e
					}
					u.LockPasswd = &b
				}
				if v := m["uid"]; v != nil {
					id, e := strconv.ParseUint(v.Value, 10, 32)
					if e != nil || v.Tag != "!!int" || id == 0 || id == 1<<32-1 {
						return fmt.Errorf("%s.uid must be a positive decimal integer", where)
					}
					uid := uint32(id)
					u.UID = &uid
				}
				if v := m["groups"]; v != nil {
					u.Groups, err = names(v, where+".groups")
					if err != nil {
						return err
					}
				}
				if v := m["ssh_authorized_keys"]; v != nil {
					u.SSHKeys, err = sshKeys(v, where+".ssh_authorized_keys")
					if err != nil {
						return err
					}
				}
			}
			if !ValidAccountName(u.Name) || seen[u.Name] {
				return fmt.Errorf("%s has an invalid or repeated user name", where)
			}
			if u.PrimaryGroup != "" && !ValidAccountName(u.PrimaryGroup) {
				return fmt.Errorf("%s.primary_group is invalid", where)
			}
			for _, path := range []string{u.Home, u.Shell} {
				if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\r\n:")) {
					return fmt.Errorf("%s requires clean absolute home/shell paths", where)
				}
			}
			if len(u.SSHKeys) > 0 && u.NoCreateHome {
				return fmt.Errorf("%s cannot combine SSH keys with no_create_home", where)
			}
			seen[u.Name] = true
			c.Users = append(c.Users, u)
		}
	}
	if n := f["chpasswd"]; n != nil {
		m, err := options(n, "chpasswd", "users", "expire")
		if err != nil {
			return err
		}
		c.ExpirePasswords = true
		if v := m["expire"]; v != nil {
			c.ExpirePasswords, err = boolean(v, "chpasswd.expire")
			if err != nil {
				return err
			}
		}
		if v := m["users"]; v != nil {
			if v.Kind != yaml.SequenceNode {
				return errors.New("chpasswd.users must be a list")
			}
			seen := map[string]bool{}
			for i, entry := range v.Content {
				where := fmt.Sprintf("chpasswd.users[%d]", i)
				fields, e := options(entry, where, "name", "password", "type")
				if e != nil {
					return e
				}
				p := Password{Hashed: true}
				if fields["name"] == nil || fields["password"] == nil {
					return fmt.Errorf("%s requires name and password", where)
				}
				p.Name, err = text(fields["name"], where+".name")
				if err != nil {
					return err
				}
				p.Value, err = text(fields["password"], where+".password")
				if err != nil {
					return err
				}
				if t := fields["type"]; t != nil {
					kind, e := text(t, where+".type")
					if e != nil {
						return e
					}
					if kind != "hash" && kind != "text" {
						return fmt.Errorf("%s.type must be hash or text", where)
					}
					p.Hashed = kind == "hash"
				}
				if !ValidAccountName(p.Name) || seen[p.Name] || p.Value == "" || strings.ContainsAny(p.Value, "\r\n:") {
					return fmt.Errorf("%s has an invalid or repeated user/password", where)
				}
				if p.Hashed && !cryptHash.MatchString(p.Value) {
					return fmt.Errorf("%s.password requires a modular crypt hash", where)
				}
				seen[p.Name] = true
				c.Passwords = append(c.Passwords, p)
			}
		}
	}
	if n := f["ssh_authorized_keys"]; n != nil {
		var err error
		c.SSHKeys, err = sshKeys(n, "ssh_authorized_keys")
		if err != nil {
			return err
		}
	}
	if n := f["ca_certs"]; n != nil {
		m, err := options(n, "ca_certs", "trusted", "remove_defaults")
		if err != nil {
			return err
		}
		if v := m["remove_defaults"]; v != nil {
			b, e := boolean(v, "ca_certs.remove_defaults")
			if e != nil {
				return e
			}
			if b {
				return errors.New("ca_certs.remove_defaults: true is not supported")
			}
		}
		if v := m["trusted"]; v != nil {
			if v.Kind != yaml.SequenceNode {
				return errors.New("ca_certs.trusted must be a list")
			}
			for _, entry := range v.Content {
				value, e := text(entry, "ca_certs.trusted certificate")
				if e != nil {
					return e
				}
				block, rest := pem.Decode([]byte(value))
				if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) > 0 || strings.TrimSpace(string(rest)) != "" {
					return errors.New("ca_certs.trusted entries require one PEM certificate")
				}
				if _, e = x509.ParseCertificate(block.Bytes); e != nil {
					return errors.New("ca_certs.trusted contains an invalid certificate")
				}
				c.CACerts = append(c.CACerts, value)
			}
		}
	}
	if n := f["timezone"]; n != nil {
		zone, err := text(n, "timezone")
		if err != nil {
			return err
		}
		if zone == "" || filepath.IsAbs(zone) || filepath.Clean(zone) != zone || zone == "." || strings.HasPrefix(zone, "../") || strings.ContainsAny(zone, "\r\n\\") {
			return errors.New("timezone must be a relative IANA zone name without traversal")
		}
		c.Timezone = zone
	}
	return nil
}

func sshKeys(n *yaml.Node, where string) ([]string, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be a list", where)
	}
	var result []string
	for _, entry := range n.Content {
		key, err := text(entry, where+" key")
		if err != nil {
			return nil, err
		}
		parts := strings.Fields(key)
		if strings.ContainsAny(key, "\r\n") || len(parts) < 2 {
			return nil, fmt.Errorf("%s requires single-line OpenSSH public keys", where)
		}
		kind := parts[0]
		if kind != "ssh-ed25519" && kind != "ssh-rsa" && !strings.HasPrefix(kind, "ecdsa-sha2-") && kind != "sk-ssh-ed25519@openssh.com" && kind != "sk-ecdsa-sha2-nistp256@openssh.com" {
			return nil, fmt.Errorf("%s has an unsupported public key type", where)
		}
		blob, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil || !validSSHBlob(kind, blob) {
			return nil, fmt.Errorf("%s has an invalid public key", where)
		}
		result = append(result, strings.TrimSpace(key))
	}
	return result, nil
}
