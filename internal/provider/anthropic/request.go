package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Msg is one conversation message. Content blocks are raw JSON: assistant
// blocks are the exact stored bytes of the earlier response, never rebuilt.
type Msg struct {
	Role    string
	Content []json.RawMessage
}

// RequestSpec is what the loop decides; everything else is the provider's.
type RequestSpec struct {
	Model     string // from the grant, required
	MaxTokens int
	System    string
	Tools     []json.RawMessage // tool definitions, already JSON
	Messages  []Msg
	// Extra holds top-level fields from the grant (thinking, output_config,
	// tool_choice ...) as raw JSON. "model", "messages", "stream", "system",
	// "tools" and "max_tokens" may not appear here.
	Extra map[string]json.RawMessage
}

// BuildRequest renders the Messages request body. Content blocks are copied
// verbatim into the output so a request that replays history is byte-stable
// in exactly those regions.
func BuildRequest(s RequestSpec) ([]byte, error) {
	if s.Model == "" {
		return nil, errors.New("anthropic: no model (the grant names it)")
	}
	if s.MaxTokens <= 0 {
		return nil, errors.New("anthropic: max_tokens must be positive")
	}
	if len(s.Messages) == 0 {
		return nil, errors.New("anthropic: no messages")
	}
	for k := range s.Extra {
		switch k {
		case "model", "messages", "stream", "system", "tools", "max_tokens":
			return nil, fmt.Errorf("anthropic: extra field %q is reserved", k)
		}
	}
	var b bytes.Buffer
	str := func(v string) { b.Write(enc(v)) }
	b.WriteString(`{"model":`)
	str(s.Model)
	fmt.Fprintf(&b, `,"max_tokens":%d,"stream":true`, s.MaxTokens)
	if s.System != "" {
		b.WriteString(`,"system":`)
		str(s.System)
	}
	if len(s.Tools) > 0 {
		b.WriteString(`,"tools":[`)
		for i, t := range s.Tools {
			if i > 0 {
				b.WriteByte(',')
			}
			if !json.Valid(t) {
				return nil, errors.New("anthropic: invalid tool definition JSON")
			}
			b.Write(t)
		}
		b.WriteByte(']')
	}
	// Sorted for a stable request hash.
	keys := make([]string, 0, len(s.Extra))
	for k := range s.Extra {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		if !json.Valid(s.Extra[k]) {
			return nil, fmt.Errorf("anthropic: extra %q is not JSON", k)
		}
		b.WriteByte(',')
		str(k)
		b.WriteByte(':')
		b.Write(s.Extra[k])
	}
	b.WriteString(`,"messages":[`)
	for i, m := range s.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, fmt.Errorf("anthropic: bad role %q", m.Role)
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":`)
		str(m.Role)
		b.WriteString(`,"content":[`)
		for j, c := range m.Content {
			if j > 0 {
				b.WriteByte(',')
			}
			if !json.Valid(c) {
				return nil, errors.New("anthropic: invalid content block JSON")
			}
			b.Write(c)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]}`)
	return b.Bytes(), nil
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
