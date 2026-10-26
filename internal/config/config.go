// Package config parses the explicitly supported cloud-config subset.
// No input is executed or written while parsing and validating a document.
package config

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	MaxDocumentBytes = 4 << 20
	MaxFileBytes     = 16 << 20
)

// Key describes the selected compatibility scope, not complete upstream modules.
type Key struct {
	Name        string
	Implemented bool
}

// Keys returns a fresh list so callers cannot change the parser's capabilities.
func Keys() []Key {
	return []Key{
		{"bootcmd", true}, {"write_files", true}, {"groups", false},
		{"users", false}, {"chpasswd", false}, {"ssh_authorized_keys", false},
		{"ca_certs", false}, {"timezone", false}, {"package_update", true},
		{"package_upgrade", true}, {"packages", true}, {"runcmd", true},
	}
}

type Config struct {
	BootCmd        []Command
	WriteFiles     []File
	PackageUpdate  bool
	PackageUpgrade bool
	Packages       []Package
	RunCmd         []Command
}

type Command struct {
	// Shell distinguishes a shell string from an argv list, including an empty string.
	Shell bool
	Text  string
	Args  []string
	Skip  bool
}

func (c Command) Argv() []string {
	if c.Shell {
		return []string{"/bin/sh", "-c", c.Text}
	}
	return append([]string(nil), c.Args...)
}

type File struct {
	Path        string
	Content     []byte
	Owner       string
	Permissions uint32
	Append      bool
	Defer       bool
}

type Package struct {
	Name    string
	Version string
}

// Parse requires one #cloud-config mapping. Unknown, duplicate, aliased, merged,
// pending and incorrectly typed configuration is rejected before provisioning.
// Errors identify locations, never the document's scalar values.
func Parse(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read user-data: %w", err)
	}
	if len(data) > MaxDocumentBytes {
		return nil, errors.New("user-data exceeds 4 MiB")
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	first, _, _ := bytes.Cut(data, []byte("\n"))
	if strings.TrimSpace(string(first)) != "#cloud-config" {
		return nil, errors.New("user-data must begin with #cloud-config")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("invalid cloud-config YAML")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("user-data must contain exactly one YAML document")
	}
	if len(document.Content) != 1 {
		return nil, errors.New("cloud-config must be a mapping")
	}
	root := document.Content[0]
	if err := checkTree(root); err != nil {
		return nil, err
	}
	fields, err := mapping(root, "cloud-config")
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(root.Content); i += 2 {
		key := root.Content[i].Value
		known := false
		for _, capability := range Keys() {
			if capability.Name == key {
				known = true
				if !capability.Implemented {
					return nil, fmt.Errorf("line %d: %s is planned but not implemented", root.Content[i].Line, key)
				}
			}
		}
		if !known {
			return nil, fmt.Errorf("line %d: unsupported top-level key", root.Content[i].Line)
		}
	}
	c := &Config{}
	if node := fields["bootcmd"]; node != nil {
		c.BootCmd, err = commands(node, "bootcmd", false)
		if err != nil {
			return nil, err
		}
	}
	if node := fields["runcmd"]; node != nil {
		c.RunCmd, err = commands(node, "runcmd", true)
		if err != nil {
			return nil, err
		}
	}
	if node := fields["write_files"]; node != nil {
		c.WriteFiles, err = files(node)
		if err != nil {
			return nil, err
		}
	}
	for _, option := range []struct {
		name string
		dst  *bool
	}{{"package_update", &c.PackageUpdate}, {"package_upgrade", &c.PackageUpgrade}} {
		if node := fields[option.name]; node != nil {
			*option.dst, err = boolean(node, option.name)
			if err != nil {
				return nil, err
			}
		}
	}
	if node := fields["packages"]; node != nil {
		if node.Kind != yaml.SequenceNode {
			return nil, errors.New("packages must be a list")
		}
		for i, entry := range node.Content {
			where := fmt.Sprintf("packages[%d]", i)
			var p Package
			if entry.Kind == yaml.SequenceNode {
				if len(entry.Content) != 2 {
					return nil, fmt.Errorf("%s must be a name or a [name, version] pair", where)
				}
				p.Name, err = text(entry.Content[0], where+" name")
				if err == nil {
					p.Version, err = text(entry.Content[1], where+" version")
				}
				if err == nil && !packageVersion.MatchString(p.Version) {
					err = fmt.Errorf("%s has an invalid version", where)
				}
			} else {
				p.Name, err = text(entry, where)
			}
			if err != nil {
				return nil, err
			}
			if !packageName.MatchString(p.Name) {
				return nil, fmt.Errorf("%s has an invalid package name", where)
			}
			c.Packages = append(c.Packages, p)
		}
	}
	return c, nil
}

var (
	packageName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:-]*$`)
	packageVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:~%-]*$`)
)

func commands(node *yaml.Node, where string, allowNull bool) ([]Command, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be a list", where)
	}
	result := make([]Command, 0, len(node.Content))
	for i, entry := range node.Content {
		at := fmt.Sprintf("%s[%d]", where, i)
		var c Command
		switch {
		case allowNull && entry.Tag == "!!null":
			c.Skip = true
		case entry.Kind == yaml.ScalarNode && entry.Tag == "!!str":
			c.Shell, c.Text = true, entry.Value
			if strings.ContainsRune(c.Text, 0) {
				return nil, fmt.Errorf("%s contains a NUL byte", at)
			}
		case entry.Kind == yaml.SequenceNode && len(entry.Content) > 0:
			for _, arg := range entry.Content {
				s, err := text(arg, at+" argument")
				if err != nil {
					return nil, err
				}
				c.Args = append(c.Args, s)
			}
			if c.Args[0] == "" {
				return nil, fmt.Errorf("%s requires a nonempty executable", at)
			}
		default:
			return nil, fmt.Errorf("%s must be a shell string or nonempty argv list", at)
		}
		result = append(result, c)
	}
	return result, nil
}

func checkTree(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return fmt.Errorf("line %d: YAML aliases and anchors are not supported", n.Line)
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Tag != "!!str" || key.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: mapping keys must be strings; merges are not supported", key.Line)
			}
			if seen[key.Value] {
				return fmt.Errorf("line %d: duplicate mapping key", key.Line)
			}
			seen[key.Value] = true
		}
	}
	for _, child := range n.Content {
		if err := checkTree(child); err != nil {
			return err
		}
	}
	return nil
}

func mapping(n *yaml.Node, where string) (map[string]*yaml.Node, error) {
	if n.Kind != yaml.MappingNode || n.Tag != "!!map" {
		return nil, fmt.Errorf("%s must be a mapping", where)
	}
	result := map[string]*yaml.Node{}
	for i := 0; i < len(n.Content); i += 2 {
		result[n.Content[i].Value] = n.Content[i+1]
	}
	return result, nil
}

func text(n *yaml.Node, where string) (string, error) {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return "", fmt.Errorf("%s must be a string", where)
	}
	if strings.ContainsRune(n.Value, 0) {
		return "", fmt.Errorf("%s contains a NUL byte", where)
	}
	return n.Value, nil
}

func boolean(n *yaml.Node, where string) (bool, error) {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" {
		return false, fmt.Errorf("%s must be a boolean", where)
	}
	return strings.EqualFold(n.Value, "true"), nil
}

func files(node *yaml.Node) ([]File, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, errors.New("write_files must be a list")
	}
	result := make([]File, 0, len(node.Content))
	for i, entry := range node.Content {
		where := fmt.Sprintf("write_files[%d]", i)
		fields, err := mapping(entry, where)
		if err != nil {
			return nil, err
		}
		f := File{Owner: "root:root", Permissions: 0644}
		content, encoding := "", "text/plain"
		for j := 0; j < len(entry.Content); j += 2 {
			key, value := entry.Content[j].Value, entry.Content[j+1]
			var dst *string
			switch key {
			case "path":
				dst = &f.Path
			case "content":
				// yaml !!binary already represents base64-encoded binary data.
				if value.Kind == yaml.ScalarNode && value.Tag == "!!binary" {
					binary, decodeErr := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(value.Value), ""))
					if decodeErr != nil {
						return nil, fmt.Errorf("%s.content has invalid binary encoding", where)
					}
					content = string(binary)
					continue
				}
				dst = &content
			case "owner":
				dst = &f.Owner
			case "encoding":
				dst = &encoding
			case "permissions":
				mode, e := text(value, where+".permissions")
				if e != nil {
					return nil, e
				}
				parsed, e := strconv.ParseUint(strings.TrimPrefix(mode, "0o"), 8, 32)
				if e != nil || parsed > 0777 || mode == "" {
					return nil, fmt.Errorf("%s.permissions must be an octal string between 0000 and 0777", where)
				}
				f.Permissions = uint32(parsed)
			case "append", "defer":
				v, e := boolean(value, where+"."+key)
				if e != nil {
					return nil, e
				}
				if key == "append" {
					f.Append = v
				} else {
					f.Defer = v
				}
			default:
				return nil, fmt.Errorf("line %d: unsupported write_files option", entry.Content[j].Line)
			}
			if dst != nil {
				*dst, err = text(value, where+"."+key)
				if err != nil {
					return nil, err
				}
			}
		}
		if fields["path"] == nil || !filepath.IsAbs(f.Path) || filepath.Clean(f.Path) == "/" {
			return nil, fmt.Errorf("%s.path must be an absolute file path", where)
		}
		owner := strings.Split(f.Owner, ":")
		if len(owner) != 2 || owner[0] == "" || owner[1] == "" {
			return nil, fmt.Errorf("%s.owner must be user:group (names or numeric IDs)", where)
		}
		f.Content, err = decodeContent(content, encoding)
		if err != nil {
			return nil, fmt.Errorf("%s.content: %w", where, err)
		}
		result = append(result, f)
	}
	return result, nil
}

func decodeContent(content, encoding string) ([]byte, error) {
	data := []byte(content)
	switch encoding {
	case "text/plain":
	case "b64", "base64", "gz+base64", "gzip+base64", "gz+b64", "gzip+b64":
		var err error
		data, err = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(content), ""))
		if err != nil {
			return nil, errors.New("invalid base64 encoding")
		}
	case "gz", "gzip":
	default:
		return nil, errors.New("unsupported encoding")
	}
	if strings.HasPrefix(encoding, "gz") {
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, errors.New("invalid gzip encoding")
		}
		defer func() { _ = reader.Close() }()
		data, err = io.ReadAll(io.LimitReader(reader, MaxFileBytes+1))
		if err != nil {
			return nil, errors.New("invalid gzip content")
		}
	}
	if len(data) > MaxFileBytes {
		return nil, errors.New("decoded file exceeds 16 MiB")
	}
	return data, nil
}
