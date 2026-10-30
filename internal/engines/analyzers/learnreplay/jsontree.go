package learnreplay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// node is one JSON value of a recording, held so that writing it back gives
// the bytes it was read from: an object keeps its keys in the order they
// were written, and a number keeps its text. The recorder writes a struct's
// fields in declaration order and a map's keys sorted (learnrec.Marshal), and
// neither survives a round trip through map[string]any.
type node struct {
	kind nodeKind
	// text is a string's value, a number's text, or "true"/"false".
	text string
	// keys and vals are an object's members in order; vals alone are a
	// list's items.
	keys []string
	vals []*node
}

type nodeKind uint8

const (
	nodeNull nodeKind = iota
	nodeBool
	nodeNumber
	nodeString
	nodeList
	nodeObject
)

// parseNode reads one JSON value.
func parseNode(data []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := readNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("learnreplay: trailing data after a JSON value")
	}
	return n, nil
}

func readNode(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case nil:
		return &node{kind: nodeNull}, nil
	case bool:
		if t {
			return &node{kind: nodeBool, text: "true"}, nil
		}
		return &node{kind: nodeBool, text: "false"}, nil
	case json.Number:
		return &node{kind: nodeNumber, text: string(t)}, nil
	case string:
		return &node{kind: nodeString, text: t}, nil
	case json.Delim:
		switch t {
		case '[':
			n := &node{kind: nodeList}
			for dec.More() {
				v, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.vals = append(n.vals, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		case '{':
			n := &node{kind: nodeObject}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("learnreplay: an object key is %T", kt)
				}
				v, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k)
				n.vals = append(n.vals, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
	}
	return nil, fmt.Errorf("learnreplay: unexpected JSON token %v", tok)
}

// encode writes n as learnrec.Marshal would have: strings escaped by
// encoding/json, numbers as their text, no space.
func (n *node) encode(buf *bytes.Buffer) {
	switch n.kind {
	case nodeNull:
		buf.WriteString("null")
	case nodeBool, nodeNumber:
		buf.WriteString(n.text)
	case nodeString:
		writeJSONString(buf, n.text)
	case nodeList:
		buf.WriteByte('[')
		for i, v := range n.vals {
			if i > 0 {
				buf.WriteByte(',')
			}
			v.encode(buf)
		}
		buf.WriteByte(']')
	case nodeObject:
		buf.WriteByte('{')
		for i, k := range n.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, k)
			buf.WriteByte(':')
			n.vals[i].encode(buf)
		}
		buf.WriteByte('}')
	}
}

// bytes is n encoded.
func (n *node) bytes() []byte {
	var buf bytes.Buffer
	n.encode(&buf)
	return buf.Bytes()
}

func writeJSONString(buf *bytes.Buffer, s string) {
	b, _ := json.Marshal(s) // a string always encodes
	buf.Write(b)
}

// get is an object's member named key, nil when n is not an object or has
// no such member.
func (n *node) get(key string) *node {
	if n == nil || n.kind != nodeObject {
		return nil
	}
	for i, k := range n.keys {
		if k == key {
			return n.vals[i]
		}
	}
	return nil
}

// without is a copy of object n with the members named in drop left out.
func (n *node) without(drop ...string) *node {
	out := &node{kind: nodeObject}
	for i, k := range n.keys {
		skip := false
		for _, d := range drop {
			if k == d {
				skip = true
			}
		}
		if !skip {
			out.keys = append(out.keys, k)
			out.vals = append(out.vals, n.vals[i])
		}
	}
	return out
}

// sortKeys orders an object's members by key, as learnrec writes a map.
func (n *node) sortKeys() {
	idx := make([]int, len(n.keys))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return n.keys[idx[a]] < n.keys[idx[b]] })
	keys := make([]string, len(idx))
	vals := make([]*node, len(idx))
	for i, j := range idx {
		keys[i], vals[i] = n.keys[j], n.vals[j]
	}
	n.keys, n.vals = keys, vals
}
