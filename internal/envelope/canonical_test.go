package envelope

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// RFC 8785 Appendix B: IEEE 754 bit patterns and their required text.
var numberVectors = []struct {
	bits uint64
	want string
}{
	{0x0000000000000000, "0"},
	{0x8000000000000000, "0"},
	{0x0000000000000001, "5e-324"},
	{0x8000000000000001, "-5e-324"},
	{0x7fefffffffffffff, "1.7976931348623157e+308"},
	{0xffefffffffffffff, "-1.7976931348623157e+308"},
	{0x4340000000000000, "9007199254740992"},
	{0xc340000000000000, "-9007199254740992"},
	{0x4430000000000000, "295147905179352830000"},
	{0x44b52d02c7e14af5, "9.999999999999997e+22"},
	{0x44b52d02c7e14af6, "1e+23"},
	{0x44b52d02c7e14af7, "1.0000000000000001e+23"},
	{0x444b1ae4d6e2ef4e, "999999999999999700000"},
	{0x444b1ae4d6e2ef4f, "999999999999999900000"},
	{0x444b1ae4d6e2ef50, "1e+21"},
	{0x3eb0c6f7a0b5ed8c, "9.999999999999997e-7"},
	{0x3eb0c6f7a0b5ed8d, "0.000001"},
	{0x41b3de4355555553, "333333333.3333332"},
	{0x41b3de4355555554, "333333333.33333325"},
	{0x41b3de4355555555, "333333333.3333333"},
	{0x41b3de4355555556, "333333333.3333334"},
	{0x41b3de4355555557, "333333333.33333343"},
	{0xbecbf647612f3696, "-0.0000033333333333333333"},
	{0x43143ff3c1cb0959, "1424953923781206.2"},
}

func TestFormatNumberRFC8785Vectors(t *testing.T) {
	for _, v := range numberVectors {
		if got := FormatNumber(math.Float64frombits(v.bits)); got != v.want {
			t.Errorf("%016x: got %s, want %s", v.bits, got, v.want)
		}
	}
}

func TestCanonicalRFCExamples(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"section 3.2.3 sorting",
			`{"\u20ac":"Euro Sign","\r":"Carriage Return","\ufb33":"Hebrew Letter Dalet With Dagesh","1":"One","\ud83d\ude00":"Emoji: Grinning Face","\u0080":"Control","\u00f6":"Latin Small Letter O With Diaeresis"}`,
			"{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"\u00f6\":\"Latin Small Letter O With Diaeresis\",\"\u20ac\":\"Euro Sign\",\"\U0001F600\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}"},
		{"section 3.2.1 example",
			`{"numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001],
			  "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
			  "literals": [null, true, false]}`,
			"{\"literals\":[null,true,false],\"numbers\":[333333333.3333333,1e+30,4.5,0.002,1e-27],\"string\":\"\u20ac$\\u000f\\nA'B\\\"\\\\\\\\\\\"/\"}"},
		{"empty containers and nesting", ` { "b" : [ ] , "a" : { } , "c":[[],{}] } `, `{"a":{},"b":[],"c":[[],{}]}`},
		{"negative zero and exponent forms", `[-0, 0.0, 1E2, 1e+2, 1.50e1]`, `[0,0,100,100,15]`},
		{"control characters", "\"\\u0001\\u001f\\u007f\\b\\f\"", "\"\\u0001\\u001f\u007f\\b\\f\""},
	}
	for _, c := range cases {
		got, err := Canonicalize([]byte(c.in))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

func TestCanonicalRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"duplicate key", `{"a":1,"a":2}`, ErrDuplicateKey},
		{"duplicate key via escape", `{"a":1,"\u0061":2}`, ErrDuplicateKey},
		{"lone high surrogate", `"\ud800"`, ErrUTF8},
		{"lone low surrogate", `"\udc00"`, ErrUTF8},
		{"high then non-low", `"\ud800\u0041"`, ErrUTF8},
		{"invalid utf-8 byte", "\"\xff\"", ErrUTF8},
		{"overlong utf-8", "\"\xc0\x80\"", ErrUTF8},
		{"number overflow", `1e999`, ErrNumber},
		{"integer beyond 2^53", `9007199254740993`, ErrNumber},
		{"integer beyond int64", `99999999999999999999`, ErrNumber},
		{"negative integer beyond 2^53", `-9007199254740993`, ErrNumber},
		{"trailing data", `1 2`, ErrTrailingBytes},
		{"trailing comma", `[1,]`, ErrSyntax},
		{"bare word", `nul`, ErrSyntax},
		{"leading zero", `01`, ErrSyntax},
		{"plus sign", `+1`, ErrSyntax},
		{"unquoted key", `{a:1}`, ErrSyntax},
		{"raw control in string", "\"a\tb\"", ErrSyntax},
		{"NaN literal", `NaN`, ErrSyntax},
		{"empty input", ``, ErrSyntax},
		{"unterminated", `{"a":[1,2`, ErrSyntax},
		{"bad escape", `"\q"`, ErrSyntax},
		{"too deep", strings.Repeat("[", MaxDepth+2) + strings.Repeat("]", MaxDepth+2), ErrTooDeep},
	}
	for _, c := range cases {
		_, err := Canonicalize([]byte(c.in))
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := Canonicalize(make([]byte, MaxBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
	if _, err := Canonicalize([]byte(`9007199254740992`)); err != nil {
		t.Errorf("2^53 itself must be accepted: %v", err)
	}
}

type ev struct {
	B string  `json:"b"`
	A int     `json:"a"`
	C []int   `json:"c"`
	D *string `json:"d,omitempty"`
}

type evReordered struct {
	D *string `json:"d,omitempty"`
	C []int   `json:"c"`
	A int     `json:"a"`
	B string  `json:"b"`
}

// The hash must not depend on Go field order or map order.
func TestMarshalIndependentOfFieldAndMapOrder(t *testing.T) {
	x, err := Marshal(ev{B: "x", A: 1, C: []int{3, 1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	y, err := Marshal(evReordered{B: "x", A: 1, C: []int{3, 1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	z, err := Marshal(map[string]any{"c": []int{3, 1, 2}, "b": "x", "a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(x) != string(y) || string(x) != string(z) {
		t.Fatalf("differ:\n%s\n%s\n%s", x, y, z)
	}
	const want = `{"a":1,"b":"x","c":[3,1,2]}`
	if string(x) != want {
		t.Fatalf("got %s want %s", x, want)
	}
	// A fixed digest so a change to the byte form is caught as a change of
	// the irreversible choice, not as a refactor.
	sum := sha256.Sum256(x)
	const wantSum = "5fa7b4f307ac8ef73371d9f500b02a67171fac85e2390147a96507db41793c57"
	if got := hex.EncodeToString(sum[:]); got != wantSum {
		t.Fatalf("digest of %s = %s, want %s", x, got, wantSum)
	}
}

// encoding/json escapes <, > and & as \u003c etc. Canonical form must not.
func TestMarshalUndoesGoHTMLEscaping(t *testing.T) {
	b, err := Marshal(map[string]string{"k": "<a&b>"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"k":"<a&b>"}` {
		t.Fatalf("got %s", b)
	}
}

// Seeded property: for random documents, shuffling key order and inserting
// whitespace never changes the canonical bytes, and Canonicalize is idempotent.
func TestCanonicalProperty(t *testing.T) {
	const seed = 20261108
	r := rand.New(rand.NewSource(seed))
	for i := 0; i < 400; i++ {
		v := randValue(r, 0)
		a := render(v, rand.New(rand.NewSource(int64(i)*2+1)), false)
		b := render(v, rand.New(rand.NewSource(int64(i)*2+2)), true)
		ca, err := Canonicalize([]byte(a))
		if err != nil {
			t.Fatalf("seed %d case %d: %v\n%s", seed, i, err, a)
		}
		cb, err := Canonicalize([]byte(b))
		if err != nil {
			t.Fatalf("seed %d case %d: %v\n%s", seed, i, err, b)
		}
		if string(ca) != string(cb) {
			t.Fatalf("seed %d case %d: key order or whitespace changed the bytes\n%s\n%s", seed, i, ca, cb)
		}
		again, err := Canonicalize(ca)
		if err != nil || string(again) != string(ca) {
			t.Fatalf("seed %d case %d: not idempotent: %v\n%s\n%s", seed, i, err, ca, again)
		}
	}
}

type node struct {
	kind int // 0 null 1 bool 2 int 3 float 4 string 5 array 6 object
	s    string
	n    float64
	b    bool
	arr  []node
	keys []string
	vals []node
}

var alphabet = []rune("abXY1 _\"\\\n\t\u00e9\u20ac\U0001F600\u0001")

func randString(r *rand.Rand) string {
	n := r.Intn(6)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteRune(alphabet[r.Intn(len(alphabet))])
	}
	return sb.String()
}

func randValue(r *rand.Rand, depth int) node {
	k := r.Intn(7)
	if depth > 3 && k > 4 {
		k = r.Intn(5)
	}
	switch k {
	case 0:
		return node{kind: 0}
	case 1:
		return node{kind: 1, b: r.Intn(2) == 0}
	case 2:
		return node{kind: 2, n: float64(r.Int63n(1<<40) - 1<<39)}
	case 3:
		return node{kind: 3, n: math.Float64frombits(r.Uint64()&^(0x7ff<<52) | uint64(r.Intn(0x7fe))<<52)}
	case 4:
		return node{kind: 4, s: randString(r)}
	case 5:
		n := node{kind: 5}
		for i := r.Intn(4); i > 0; i-- {
			n.arr = append(n.arr, randValue(r, depth+1))
		}
		return n
	default:
		n := node{kind: 6}
		seen := map[string]bool{}
		for i := r.Intn(5); i > 0; i-- {
			k := randString(r)
			if seen[k] {
				continue
			}
			seen[k] = true
			n.keys = append(n.keys, k)
			n.vals = append(n.vals, randValue(r, depth+1))
		}
		return n
	}
}

func jsonString(s string, escapeAll bool) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, c := range s {
		switch {
		case c == '"':
			sb.WriteString(`\"`)
		case c == '\\':
			sb.WriteString(`\\`)
		case c < 0x20 || (escapeAll && c > 0x7e):
			if c > 0xffff {
				c -= 0x10000
				fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xd800+(c>>10), 0xdc00+(c&0x3ff))
			} else {
				fmt.Fprintf(&sb, `\u%04x`, c)
			}
		default:
			sb.WriteRune(c)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// render writes n as JSON with shuffled member order and random whitespace;
// alt switches number and string spelling (escaped non-ASCII, exponent form).
func render(n node, r *rand.Rand, alt bool) string {
	ws := func() string { return strings.Repeat(" ", r.Intn(3)) }
	switch n.kind {
	case 0:
		return "null"
	case 1:
		return strconv.FormatBool(n.b)
	case 2:
		if alt {
			return strconv.FormatFloat(n.n, 'f', 1, 64)
		}
		return strconv.FormatFloat(n.n, 'f', -1, 64)
	case 3:
		if alt {
			return strconv.FormatFloat(n.n, 'e', -1, 64)
		}
		return strconv.FormatFloat(n.n, 'g', -1, 64)
	case 4:
		return jsonString(n.s, alt)
	case 5:
		parts := make([]string, len(n.arr))
		for i, a := range n.arr {
			parts[i] = ws() + render(a, r, alt) + ws()
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		idx := r.Perm(len(n.keys))
		parts := make([]string, 0, len(idx))
		for _, i := range idx {
			parts = append(parts, ws()+jsonString(n.keys[i], alt)+ws()+":"+ws()+render(n.vals[i], r, alt)+ws())
		}
		if alt {
			sort.Strings(parts) // a different, but still arbitrary, member order
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
}
