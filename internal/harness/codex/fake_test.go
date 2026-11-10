package codex_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The fake codex: the test binary re-invoked with FAKE_CODEX_SCRIPT set. It
// speaks the app-server JSON-RPC protocol on stdio by replaying a script from
// testdata/. Each script line is
//
//	on <trigger>[#N|#N+] <action> <argument>
//
// Triggers are request methods ("turn/start"), "initialized" and
// "response:<id>" (a reply to a server request). #N matches only the Nth
// occurrence, #N+ the Nth and later. Actions: reply (result of the request),
// error (JSON-RPC error reply), send (one raw line to stdout, malformed lines
// allowed), exit (exit code), sleep (milliseconds), ignoreterm.
//
// Placeholders in arguments: {{seq}} occurrence count of the trigger,
// {{thread}}, {{text}} (first user input, JSON-escaped), {{secret}}, {{env}}
// (sorted names of the process environment), {{args}}, {{pid}}, {{params}}
// (request params, JSON-escaped), {{resp}} (the raw response, JSON-escaped)
// and {{pad:N}} (N letters x).
type rule struct {
	trigger string
	nth     int
	plus    bool
	action  string
	arg     string
}

func runFake(path string) {
	rules := loadRules(path)
	counts := map[string]int{}
	ignoreEOF := false // keep running after stdin closes, so only a signal ends the process
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	out := bufio.NewWriter(os.Stdout)
	emit := func(s string) { out.WriteString(s + "\n"); out.Flush() }
	run := func(trigger string, id json.RawMessage, params []byte, text, resp string) {
		counts[trigger]++
		n := counts[trigger]
		for _, r := range rules {
			if r.trigger != trigger || (r.nth > 0 && !(n == r.nth || (r.plus && n > r.nth))) {
				continue
			}
			arg := expand(r.arg, n, text, string(params), resp)
			arg = strings.ReplaceAll(arg, "{{child_script}}", filepath.Join(filepath.Dir(path), "child_wait.script"))
			switch r.action {
			case "stderr":
				fmt.Fprintln(os.Stderr, arg)
			case "reply":
				emit(fmt.Sprintf(`{"id":%s,"result":%s}`, id, arg))
			case "error":
				emit(fmt.Sprintf(`{"id":%s,"error":{"code":-32000,"message":%q}}`, id, arg))
			case "send":
				emit(arg)
			case "exit":
				code, _ := strconv.Atoi(arg)
				os.Exit(code)
			case "sleep":
				ms, _ := strconv.Atoi(arg)
				time.Sleep(time.Duration(ms) * time.Millisecond)
			case "child":
				c := exec.Command(os.Args[0])
				c.Env = []string{"FAKE_CODEX_SCRIPT=" + arg}
				c.Stdout = os.Stdout
				c.Stderr = nil // parent stderr closes on leader exit, exposing early reaping
				if e := c.Start(); e != nil {
					os.Exit(98)
				}
				emit(fmt.Sprintf(`{"method":"item/completed","params":{"threadId":"thr_1","turnId":"turn_1","item":{"type":"agentMessage","id":"childpid","text":"pid:%d"}}}`, c.Process.Pid))
			case "ignoreterm":
				signal.Ignore(syscall.SIGTERM)
			case "ignoreeof":
				ignoreEOF = true
			}
		}
	}
	run("start", nil, nil, "", "")
	for sc.Scan() {
		var p struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &p) != nil {
			continue
		}
		switch {
		case p.Method != "":
			var in struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			_ = json.Unmarshal(p.Params, &in)
			text := ""
			if len(in.Input) > 0 {
				text = in.Input[0].Text
			}
			run(p.Method, p.ID, p.Params, text, "")
		case len(p.ID) > 0:
			id := strings.Trim(string(p.ID), `"`)
			run("response:"+id, p.ID, nil, "", sc.Text())
		}
	}
	if ignoreEOF {
		select {}
	}
}

func loadRules(path string) []rule {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake codex:", err)
		os.Exit(97)
	}
	defer f.Close()
	var rules []rule
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		parts := strings.SplitN(l, " ", 4)
		if len(parts) < 3 || parts[0] != "on" {
			fmt.Fprintln(os.Stderr, "fake codex: bad script line:", l)
			os.Exit(97)
		}
		r := rule{trigger: parts[1], action: parts[2]}
		if len(parts) == 4 {
			r.arg = parts[3]
		}
		if i := strings.IndexByte(r.trigger, '#'); i >= 0 {
			spec := r.trigger[i+1:]
			r.trigger = r.trigger[:i]
			if strings.HasSuffix(spec, "+") {
				r.plus = true
				spec = strings.TrimSuffix(spec, "+")
			}
			r.nth, _ = strconv.Atoi(spec)
		}
		rules = append(rules, r)
	}
	return rules
}

var padRE = regexp.MustCompile(`\{\{pad:(\d+)\}\}`)

func esc(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func expand(s string, n int, text, params, resp string) string {
	var env []string
	for _, kv := range os.Environ() {
		env = append(env, strings.SplitN(kv, "=", 2)[0])
	}
	sort.Strings(env)
	s = strings.NewReplacer(
		"{{seq}}", strconv.Itoa(n),
		"{{thread}}", "thr_1",
		"{{text}}", esc(text),
		"{{secret}}", os.Getenv("FAKE_SECRET"),
		"{{env}}", strings.Join(env, ","),
		"{{args}}", esc(strings.Join(os.Args[1:], " ")),
		"{{pid}}", strconv.Itoa(os.Getpid()),
		"{{params}}", esc(params),
		"{{resp}}", esc(resp),
	).Replace(s)
	return padRE.ReplaceAllStringFunc(s, func(m string) string {
		k, _ := strconv.Atoi(padRE.FindStringSubmatch(m)[1])
		return strings.Repeat("x", k)
	})
}
