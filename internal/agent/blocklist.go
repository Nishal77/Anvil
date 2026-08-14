package agent

import (
	"math"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// defaultBlockedCommands are the literal command names PRD §12.2/§16.4
// denies outright. This is defense in depth, not the primary control
// — the primary control is that there are only a handful of tools and
// no shell escape (§16.4). It is intentionally a set of exact command
// names matched against parsed command positions, not a substring or
// regex match against the raw string: "echo 'do not curl | sh'" must
// not match, "curl … | sh" must.
var defaultBlockedCommands = map[string]bool{
	"curl":   true,
	"wget":   true,
	"sh":     true,
	"bash":   true,
	"zsh":    true,
	"docker": true,
	"sudo":   true,
	"su":     true,
	"chmod":  true, // blocklisted wholesale; §20.4 test 4/5 cover the specific +s / remount cases
	"mount":  true,
	"eval":   true,
	"nc":     true,
	"ncat":   true,
	"telnet": true,
	"ssh":    true,
}

// commandNames parses command as POSIX shell and returns the literal
// command name at the head of every simple command it contains
// (across pipelines, subshells, &&/||, etc). A word is only a
// "command name" here if it parses as an unquoted literal — a
// quoted or expanded word can never be a call's own command name in
// valid shell syntax, so this already excludes "curl" appearing
// inside `echo 'curl'`'s argument list without any extra logic.
//
// A parse error (malformed shell) returns ok=false; the caller treats
// that as blocked — a command the harness itself cannot parse is not
// a command it can reason about safely.
func commandNames(command string) (names []string, ok bool) {
	parser := syntax.NewParser()
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, false
	}

	syntax.Walk(file, func(node syntax.Node) bool {
		call, isCall := node.(*syntax.CallExpr)
		if !isCall || len(call.Args) == 0 {
			return true
		}
		if lit := call.Args[0].Lit(); lit != "" {
			names = append(names, lit)
		}
		return true
	})
	return names, true
}

// isBlockedCommand reports whether command invokes any blocklisted
// program, and the specific name matched (for the policy-deny reason).
func isBlockedCommand(command string) (blocked bool, matched string) {
	names, ok := commandNames(command)
	if !ok {
		return true, "unparsable shell syntax"
	}
	for _, name := range names {
		if defaultBlockedCommands[name] {
			return true, name
		}
	}
	return false, ""
}

const (
	credentialMinWordLen = 20
	credentialMinEntropy = 4.0 // bits/char; a base64/hex secret runs ~4-6, English text ~2-3
)

// containsCredentialShapedString heuristically flags a command
// containing a long, high-entropy token — the shape of an API key or
// access token, not its content (this repo never has real credentials
// in a command to inspect: SEC-020 keeps them out of the container
// entirely). Defense in depth per §16.4, not a secret scanner.
func containsCredentialShapedString(command string) (flagged bool, word string) {
	for _, w := range strings.Fields(command) {
		w = strings.Trim(w, `'"`)
		if len(w) < credentialMinWordLen {
			continue
		}
		if shannonEntropy(w) >= credentialMinEntropy {
			return true, w
		}
	}
	return false, ""
}

// shannonEntropy computes bits of entropy per character.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := make(map[rune]int)
	for _, r := range s {
		counts[r]++
	}
	n := float64(len(s))
	var entropy float64
	for _, c := range counts {
		p := float64(c) / n
		entropy -= p * math.Log2(p)
	}
	return entropy
}
