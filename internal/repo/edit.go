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
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: expected a mapping at the top level", path)
	}
	keepBlankLines(&doc, strings.Split(string(data), "\n"))
	seq, err := sequenceAt(doc.Content[0], key)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var add yaml.Node
	if err := add.Encode(item); err != nil {
		return fmt.Errorf("encode %s entry: %w", key, err)
	}
	seq.Content = append(seq.Content, &add)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	enc.Close()
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// sequenceAt returns the value node of key as a block sequence. An absent key is
// appended; a null or flow-style value is converted in place, which keeps the
// comment on a documented but still empty "projects:".
func sequenceAt(mapping *yaml.Node, key string) (*yaml.Node, error) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		v := mapping.Content[i+1]
		switch v.Kind {
		case yaml.SequenceNode:
			v.Style = 0
			return v, nil
		case yaml.ScalarNode:
			if v.Tag != "!!null" {
				return nil, fmt.Errorf("%q is not a list", key)
			}
			*v = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			return v, nil
		default:
			return nil, fmt.Errorf("%q is not a list", key)
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	mapping.Content = append(mapping.Content, k, v)
	return v, nil
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
