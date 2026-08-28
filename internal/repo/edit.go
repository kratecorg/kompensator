package repo

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AppendToSequence appends item to the sequence stored under key in the YAML
// file at path. It edits the document tree rather than re-marshalling a struct,
// so the comments in a hand-written env.yml or stack.yml survive. The key is
// created, or an empty/null value converted, when the sequence is not there yet.
func AppendToSequence(path, key string, item any) error {
	doc, err := loadDocument(path)
	if err != nil {
		return err
	}
	seq, err := sequenceAt(doc.Content[0], key)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var add yaml.Node
	if err := add.Encode(item); err != nil {
		return fmt.Errorf("encode %s entry: %w", key, err)
	}
	seq.Content = append(seq.Content, &add)
	return writeDocument(path, doc)
}

// RemoveFromSequence drops the entry called name from the sequence under key,
// matching either a bare scalar ("- carimco") or a mapping with that name. It
// reports whether an entry was there to remove.
func RemoveFromSequence(path, key, name string) (bool, error) {
	doc, err := loadDocument(path)
	if err != nil {
		return false, err
	}
	seq := valueAt(doc.Content[0], key)
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return false, nil
	}
	for i, item := range seq.Content {
		if entryName(item) != name {
			continue
		}
		seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
		return true, writeDocument(path, doc)
	}
	return false, nil
}

// SetStateImage points a service's desired image and tag in a state file,
// creating the project or service entry when it is not there yet. Keys the
// caller did not name (oneShot) and the file's comments are left alone.
func SetStateImage(path, project, service, image, tag string) error {
	doc, err := loadDocument(path)
	if err != nil {
		return err
	}
	proj, err := mappingAt(doc.Content[0], project)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	svc, err := mappingAt(proj, service)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	setScalar(svc, "image", image)
	setScalar(svc, "tag", tag)
	return writeDocument(path, doc)
}

// loadDocument parses a YAML file into a document tree whose comments and blank
// lines survive a re-encode.
func loadDocument(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: expected a mapping at the top level", path)
	}
	keepBlankLines(&doc, strings.Split(string(data), "\n"))
	return &doc, nil
}

func writeDocument(path string, doc *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	enc.Close()
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// entryName is the name a sequence entry goes by: its own value when it is a
// bare scalar, its "name" field when it is a mapping.
func entryName(item *yaml.Node) string {
	if item.Kind == yaml.ScalarNode {
		return item.Value
	}
	if n := valueAt(item, "name"); n != nil {
		return n.Value
	}
	return ""
}

// valueAt returns the value node of key, or nil when the mapping has no such
// key.
func valueAt(mapping *yaml.Node, key string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// mappingAt returns the value node of key as a block mapping, creating it (or
// converting a null placeholder) when needed.
func mappingAt(mapping *yaml.Node, key string) (*yaml.Node, error) {
	v := valueAt(mapping, key)
	if v == nil {
		k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
		v = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapping.Content = append(mapping.Content, k, v)
		return v, nil
	}
	switch {
	case v.Kind == yaml.MappingNode:
		v.Style = 0
		return v, nil
	case v.Kind == yaml.ScalarNode && v.Tag == "!!null":
		*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		return v, nil
	default:
		return nil, fmt.Errorf("%q is not a mapping", key)
	}
}

// setScalar sets key to a string value, appending the key when it is absent.
func setScalar(mapping *yaml.Node, key, value string) {
	if v := valueAt(mapping, key); v != nil {
		v.Kind, v.Tag, v.Value, v.Style = yaml.ScalarNode, "!!str", value, 0
		v.Content = nil
		return
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

// sequenceAt returns the value node of key as a block sequence. An absent key is
// appended; a null or flow-style value is converted in place, which keeps the
// comment on a documented but still empty "projects:".
func sequenceAt(mapping *yaml.Node, key string) (*yaml.Node, error) {
	v := valueAt(mapping, key)
	if v == nil {
		k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
		v = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		mapping.Content = append(mapping.Content, k, v)
		return v, nil
	}
	switch {
	case v.Kind == yaml.SequenceNode:
		v.Style = 0
		return v, nil
	case v.Kind == yaml.ScalarNode && v.Tag == "!!null":
		*v = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		return v, nil
	default:
		return nil, fmt.Errorf("%q is not a list", key)
	}
}

// keepBlankLines restores the blank lines yaml.v3 drops on re-encode. Every
// mapping key and sequence entry that the original file separated from what
// precedes it by an empty line gets that line folded into its head comment,
// which the encoder writes back out as a blank line. lines is the original
// file, split on newlines.
func keepBlankLines(root *yaml.Node, lines []string) {
	marked := map[int]bool{}
	mark := func(n *yaml.Node) {
		if marked[n.Line] {
			return
		}
		height := 0
		if n.HeadComment != "" {
			height = strings.Count(n.HeadComment, "\n") + 1
		}
		before := n.Line - height - 1
		if before < 1 || before > len(lines) || strings.TrimSpace(lines[before-1]) != "" {
			return
		}
		marked[n.Line] = true
		n.HeadComment = "\n" + n.HeadComment
	}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				mark(n.Content[i])
				walk(n.Content[i+1])
			}
		case yaml.SequenceNode:
			for _, c := range n.Content {
				mark(c)
				walk(c)
			}
		default:
			for _, c := range n.Content {
				walk(c)
			}
		}
	}
	walk(root)
}
