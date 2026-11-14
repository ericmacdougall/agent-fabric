package main

import (
	"crypto/rand"
	"strings"
	"testing"
)

func synthToken() string {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	b := make([]byte, 95)
	rand.Read(b)
	out := []byte("sk-" + "ant-oat01-")
	for _, c := range b {
		out = append(out, alpha[int(c)&63])
	}
	return string(out)
}

func scanAll(t *testing.T, output string) (shown []string, tok string, err error) {
	t.Helper()
	s := &scanner{show: func(l string) { shown = append(shown, l) }}
	for i := 0; i < len(output); i += 7 { // odd chunking: lines split across reads
		e := i + 7
		if e > len(output) {
			e = len(output)
		}
		s.Feed([]byte(output[i:e]))
	}
	tok, err = s.Close()
	return shown, tok, err
}

func cliOutput(tok, tokenLine string) string {
	return "\x1b[2J\x1b[1mWelcome\x1b[0m to Claude Code\r\n" +
		"Browser didn't open? Use the url below to sign in:\r\n\r\n" +
		"https://claude.ai/oauth/authorize?code=true&client_id=abc&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com\r\n" +
		"%2Foauth%2Fcode%2Fcallback&scope=user%3Ainference&state=xyz\r\n\r\n" +
		"Paste code here if prompted > \r\n" +
		"\x1b[32m✓\x1b[0m Long-lived authentication token created successfully!\r\n\r\n" +
		"Your OAuth token (valid for 1 year):\r\n\r\n" + tokenLine + "\r\n\r\n" +
		"Store this token securely. You won't be able to see it again.\r\n"
}

func TestScanAcceptsWholeTokenAndShowsOnlyURLAndPrompts(t *testing.T) {
	tok := synthToken()
	shown, got, err := scanAll(t, cliOutput(tok, tok))
	if err != nil || got != tok {
		t.Fatalf("err %v, token matched %v", err, got == tok)
	}
	joined := strings.Join(shown, "\n")
	if strings.Contains(joined, tok[:20]) || strings.Contains(joined, "sk-ant") {
		t.Fatal("the token was shown")
	}
	if !strings.Contains(joined, "sign-in URL: https://claude.ai/oauth/authorize?") || !strings.Contains(joined, "state=xyz") ||
		!strings.Contains(joined, "Paste code here if prompted >") {
		t.Fatalf("URL or prompt not shown: %q", joined)
	}
}

// NEG-14 and the abort rule: any unexpected or token-bearing line aborts
// without a token, and the error never carries the token.
func TestScanAbortsOnUnexpectedOutput(t *testing.T) {
	tok := synthToken()
	for name, out := range map[string]string{
		"embedded token":  cliOutput(tok, "export CLAUDE_CODE_OAUTH_TOKEN="+tok),
		"truncated token": cliOutput(tok, tok[:30]),
		"wrapped token":   cliOutput(tok, tok[:60]+"\r\n"+tok[60:]),
		"two tokens":      cliOutput(tok, tok+"\r\n"+synthToken()),
		"unknown line":    cliOutput(tok, tok) + "something nobody expected\r\n",
		"foreign URL":     strings.Replace(cliOutput(tok, tok), "https://claude.ai/", "https://evil.example/", 1),
		"no token":        cliOutput(tok, ""),
	} {
		shown, got, err := scanAll(t, out)
		if err == nil || got != "" {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), tok[13:30]) || strings.Contains(strings.Join(shown, "\n"), tok[13:30]) {
			t.Fatalf("%s: token bytes in the error or output", name)
		}
	}
}
