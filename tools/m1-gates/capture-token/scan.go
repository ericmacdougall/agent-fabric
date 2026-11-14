// Command capture-token acquires the M1 subscription token (goodqa#15966
// AC-17). It runs `claude setup-token` on a 200-column pty, strips ANSI
// escapes, shows only the sign-in URL and the prompts, accepts the token
// only when a whole line matches its shape, and writes it as
// claude-oauth.v0 with claude-oauth hard-linked to it (credential.Capture,
// under the rotate lock). It prints only the token's length and an 8-hex
// sha256 prefix. Any unexpected output aborts it without writing.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	ansiRE = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)
	// tokenRE is the whole-line token shape.
	tokenRE = regexp.MustCompile(`^sk-ant-oat01-[A-Za-z0-9_-]{67,499}$`) // 80 or more characters; a shorter line is a truncation
	// skAnt finds any token-like run, for the abort rule.
	skAnt   = regexp.MustCompile(`sk-ant-`)
	urlRE   = regexp.MustCompile(`https://[^\s]+`)
	urlCont = regexp.MustCompile(`^[A-Za-z0-9\-._~:/?#\[\]@!$&'()*+,;=%]+$`)
	prompt  = regexp.MustCompile(`(?i)^(paste code here if prompted|press enter|opening browser|waiting for|browser didn't open|use the url below|sign in|log in|login)`)
	// quiet lines are expected and allowed but not shown (they sit next to
	// the token in the CLI's output).
	quiet = regexp.MustCompile(`(?i)(welcome to claude code|oauth token|valid for|store this token|keep it secure|securely|long-lived|authentication token|created|success|✓|^[\s─│╭╮╰╯═║╔╗╚╝-]*$)`)
)

// allowedURLHosts are the sign-in hosts whose URL may be shown.
var allowedURLHosts = map[string]bool{"claude.ai": true, "platform.claude.com": true, "console.anthropic.com": true}

// scanner classifies the CLI's output line by line.
type scanner struct {
	token   string
	partial string
	lineNo  int
	lastCR  bool
	url     string // a sign-in URL that may continue on the next lines (the CLI wraps)
	show    func(string)
	err     error
}

var errUnexpected = errors.New("capture-token: unexpected output; nothing written")

// Feed takes raw pty bytes.
func (s *scanner) Feed(b []byte) {
	s.partial += string(b)
	for {
		i := strings.IndexAny(s.partial, "\r\n")
		if i < 0 {
			// A prompt ending in '>' without a newline waits for input: show
			// it now so the person sees it.
			if p := strings.TrimSpace(ansiRE.ReplaceAllString(s.partial, "")); strings.HasSuffix(p, ">") && prompt.MatchString(p) && !skAnt.MatchString(p) {
				s.showPrompt(p)
				s.partial = ""
			}
			return
		}
		if i == 0 && s.partial[0] == '\n' && s.lastCR {
			s.partial = s.partial[1:] // the \n of a \r\n split across reads
			s.lastCR = false
			continue
		}
		line := s.partial[:i]
		s.lastCR = s.partial[i] == '\r'
		if s.lastCR && i+1 < len(s.partial) && s.partial[i+1] == '\n' {
			i++
			s.lastCR = false
		}
		s.partial = s.partial[i+1:]
		s.line(line)
	}
}

// Close classifies the trailing partial line and returns the token.
func (s *scanner) Close() (string, error) {
	if s.partial != "" {
		s.line(s.partial)
		s.partial = ""
	}
	s.flushURL()
	if s.err != nil {
		return "", s.err
	}
	if s.token == "" {
		return "", errors.New("capture-token: no token in the CLI output; nothing written")
	}
	return s.token, nil
}

func (s *scanner) fail(format string, a ...any) {
	if s.err == nil {
		s.err = fmt.Errorf("%w (%s)", errUnexpected, fmt.Sprintf(format, a...))
	}
}

func (s *scanner) showPrompt(p string) {
	if i := strings.Index(p, ">"); i >= 0 {
		p = p[:i+1] // never echo what was typed after the prompt
	}
	s.show(p)
}

func (s *scanner) flushURL() {
	if s.url == "" {
		return
	}
	u, err := url.Parse(s.url)
	s.url = ""
	if err != nil || !allowedURLHosts[u.Hostname()] {
		s.fail("line %d: a URL on an unexpected host", s.lineNo)
		return
	}
	s.show("sign-in URL: " + u.String())
}

func (s *scanner) line(raw string) {
	s.lineNo++
	l := strings.TrimSpace(ansiRE.ReplaceAllString(raw, ""))
	if s.url != "" && l != "" && urlCont.MatchString(l) && !skAnt.MatchString(l) && !strings.HasPrefix(l, "https://") {
		s.url += l // a wrapped continuation of the sign-in URL
		return
	}
	s.flushURL()
	switch {
	case l == "":
	case tokenRE.MatchString(l):
		if s.token != "" && s.token != l {
			s.fail("line %d: a second token", s.lineNo)
			return
		}
		s.token = l
	case skAnt.MatchString(l):
		// A token-like run that is not a whole token line: truncated,
		// wrapped or embedded. Its content is never printed.
		s.fail("line %d: a token-like value that is not a whole line, length %d", s.lineNo, len(l))
	case urlRE.MatchString(l):
		s.url = urlRE.FindString(l)
	case prompt.MatchString(l):
		s.showPrompt(l)
	case quiet.MatchString(l):
	default:
		s.fail("line %d of length %d is not an expected line", s.lineNo, len(l))
	}
}
