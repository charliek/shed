package main

import (
	"encoding/pem"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/sshconfig"
)

// The rule every message holds: a command it suggests (`shed …` or `ssh…`)
// holds no shell metacharacter outside single quotes, so pasted as shown it
// runs the command it names and nothing else. A placeholder is a plain word
// (`/path/to/key`, NAME), never `<key>`: pasted unchanged, `<key>` is a
// redirection.
//
// TestNoSuggestedCommandHoldsShellSyntax holds every message of
// internal/config and cmd/shed to it, two ways:
//
//   - THE SOURCE. Every string literal in those packages' non-test files is
//     read (a run of literals joined with + is read as one string; an operand
//     that is not a literal reads as a plain word), except the help text a
//     cobra.Command composite literal carries in Use, Short, Long and Example
//     (only that type's fields: a field of the same name in any other literal
//     is read) and the usage of a flag, which are reference synopses and keep
//     the conventional `<name>`. Each suggested command in them is checked.
//   - THE MESSAGES BUILT AT RUN TIME. Every EntryRefusedError kind, every
//     reason IdentityPublicKey and the identity-file checks give (from real
//     files), and the enrolment refusal (keysRefusedError) are built and their
//     text checked, so a command whose words come from a constant or a value
//     is checked as it is printed.
//
// TestRenderedSuggestionsQuoteTheirValues holds every function that builds a
// suggested command from a value (a shed, server, entry or image name, a host
// or a path) to it with hostile values: each value is one shell word in the
// command, or the function suggests no command.
//
// What it cannot see: a command assembled at run time from pieces that are
// not literals and not in the messages built here (it sees each literal piece
// alone), and a message outside these two packages (the SDK's own, which
// sdk/bootstrap's tests hold, and the server's).

// suggestionMeta is every character a shell reads as syntax that a suggested
// command may not hold outside single quotes.
const suggestionMeta = "<>|&;$`"

// suggestionsIn returns each suggested command in s: the text inside a pair
// of backquotes, inside parentheses (to the matching one), a line of its own,
// or the text after ": " to the line's end, when it starts with `shed ` or
// `ssh`.
func suggestionsIn(s string) []string {
	var out []string
	keep := func(seg string) {
		if seg = strings.TrimSpace(seg); looksLikeCommand(seg) {
			out = append(out, seg)
		}
	}
	// Backquoted.
	for rest := s; ; {
		i := strings.IndexByte(rest, '`')
		if i < 0 {
			break
		}
		j := strings.IndexByte(rest[i+1:], '`')
		if j < 0 {
			break
		}
		keep(rest[i+1 : i+1+j])
		rest = rest[i+1+j+1:]
	}
	// Parenthesised, nesting included.
	for i := 0; i < len(s); i++ {
		if s[i] != '(' {
			continue
		}
		depth := 0
		for j := i; j < len(s); j++ {
			switch s[j] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				keep(s[i+1 : j])
				break
			}
		}
	}
	// A line of its own, and the text after a colon to the end of the line.
	for _, line := range strings.Split(s, "\n") {
		keep(line)
		for rest := line; ; {
			i := strings.Index(rest, ": ")
			if i < 0 {
				break
			}
			keep(rest[i+2:])
			rest = rest[i+2:]
		}
	}
	return out
}

// commandWord matches a subcommand or a command's name: lower-case letters and
// hyphens.
var commandWord = regexp.MustCompile(`^[a-z][a-z-]*$`)

// looksLikeCommand reports whether seg starts as a command does: `shed`
// followed by a subcommand word, or a word `ssh` or `ssh-…`. A sentence about
// a shed ("shed %q has …", "shed roost-provider: ignoring …") is not one.
func looksLikeCommand(seg string) bool {
	words := strings.Fields(seg)
	if len(words) == 0 {
		return false
	}
	switch {
	case words[0] == "shed":
		return len(words) > 1 && commandWord.MatchString(words[1])
	case words[0] == "ssh", strings.HasPrefix(words[0], "ssh-"):
		return commandWord.MatchString(words[0])
	}
	return false
}

// shellSyntaxOutsideQuotes returns every character of cmd in suggestionMeta
// that is outside single quotes.
func shellSyntaxOutsideQuotes(cmd string) string {
	var found strings.Builder
	quoted := false
	for _, c := range cmd {
		switch {
		case c == '\'':
			quoted = !quoted
		case !quoted && strings.ContainsRune(suggestionMeta, c):
			found.WriteRune(c)
		}
	}
	return found.String()
}

// unsafeSuggestions returns each suggested command in s that holds shell
// syntax outside single quotes.
func unsafeSuggestions(s string) []string {
	var bad []string
	for _, cmd := range suggestionsIn(s) {
		if shellSyntaxOutsideQuotes(cmd) != "" {
			bad = append(bad, cmd)
		}
	}
	return bad
}

// helpField reports whether a composite literal's key names one of a cobra
// command's help fields.
func helpField(key ast.Expr) bool {
	id, ok := key.(*ast.Ident)
	return ok && (id.Name == "Use" || id.Name == "Short" || id.Name == "Long" || id.Name == "Example")
}

// cobraCommandLiteral reports whether lit is a cobra.Command composite
// literal (`cobra.Command{…}`, also as the operand of `&`): only its help
// fields are exempt, never a field of the same name in any other type.
func cobraCommandLiteral(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Command" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "cobra"
}

// flagDefinition reports whether call defines a flag (pflag's StringVar,
// BoolVarP, Int, …), whose last argument is the flag's usage.
func flagDefinition(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	recv, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	inner, ok := recv.Fun.(*ast.SelectorExpr)
	return ok && (inner.Sel.Name == "Flags" || inner.Sel.Name == "PersistentFlags")
}

// stringOf reads a string expression: a literal, or literals joined with +,
// with every operand that is not a literal read as the plain word X. ok is
// false when no part of it is a string literal.
func stringOf(e ast.Expr) (s string, ok bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		u, err := strconv.Unquote(v.Value)
		return u, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, lok := stringOf(v.X)
		r, rok := stringOf(v.Y)
		if !lok && !rok {
			return "", false
		}
		if !lok {
			l = "X"
		}
		if !rok {
			r = "X"
		}
		return l + r, true
	case *ast.ParenExpr:
		return stringOf(v.X)
	}
	return "", false
}

// sourceStrings returns every string of the non-test Go files in dir, outside
// help text and flag usage, keyed by its position.
func sourceStrings(t *testing.T, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		skip := map[ast.Node]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil || skip[n] {
				return false
			}
			switch v := n.(type) {
			case *ast.CompositeLit:
				if cobraCommandLiteral(v) {
					for _, elt := range v.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok && helpField(kv.Key) {
							skip[kv.Value] = true
						}
					}
				}
			case *ast.CallExpr:
				if flagDefinition(v) && len(v.Args) > 0 {
					skip[v.Args[len(v.Args)-1]] = true
				}
			case *ast.BinaryExpr, *ast.BasicLit:
				if s, ok := stringOf(v.(ast.Expr)); ok {
					out[fset.Position(n.Pos()).String()] = s
					return false // the whole run, read once
				}
			}
			return true
		})
	}
	return out
}

// TestNoSuggestedCommandHoldsShellSyntax: see the comment at the top of this
// file.
func TestNoSuggestedCommandHoldsShellSyntax(t *testing.T) {
	// THE SOURCE.
	read := 0
	for _, dir := range []string{".", filepath.Join("..", "..", "internal", "config")} {
		strs := sourceStrings(t, dir)
		if len(strs) < 100 {
			t.Fatalf("read only %d strings in %s", len(strs), dir)
		}
		for pos, s := range strs {
			read++
			for _, cmd := range unsafeSuggestions(s) {
				t.Errorf("%s: a suggested command holds shell syntax: %q", pos, cmd)
			}
		}
	}

	// THE MESSAGES BUILT AT RUN TIME.
	var msgs []string

	// Every EntryRefusedError kind, each field set. The kinds are numbered
	// from 1; the first number past the last kind gives the default text.
	defaultText := (&config.EntryRefusedError{Entry: "shed"}).Error()
	kinds := 0
	for k := config.RefusalKind(1); ; k++ {
		e := &config.EntryRefusedError{Entry: "shed", Kind: k, CertUser: "admin", EntryUser: "pat",
			Cause: errors.New("a cause"), Host: "build-1.example.test", IdentityFile: "/home/pat/.ssh/id_work",
			EntryKey: "SHA256:entry", IssuedKey: "SHA256:issued"}
		msg := e.Error()
		if msg == defaultText {
			break
		}
		kinds++
		msgs = append(msgs, msg)
	}
	if kinds < int(config.RefusedStoredTokenForKey) {
		t.Fatalf("built %d refusal kinds, want every kind through RefusedStoredTokenForKey (%d)",
			kinds, config.RefusedStoredTokenForKey)
	}

	// Every reason an identity file is refused for, from real files.
	dir := t.TempDir()
	writeLegacyEncrypted := func(path string) {
		block := &pem.Block{Type: "RSA PRIVATE KEY",
			Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00112233445566778899AABBCCDDEEFF"},
			Bytes:   []byte("encrypted bytes that only the passphrase opens")}
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	noPub := filepath.Join(dir, "legacy")
	writeLegacyEncrypted(noPub)
	badPub := filepath.Join(dir, "legacy-badpub")
	writeLegacyEncrypted(badPub)
	write(badPub+".pub", "not a public key\n")
	notAKey := filepath.Join(dir, "notes")
	write(notAKey, "just some text\n")
	mismatched, other := filepath.Join(dir, "mismatched"), filepath.Join(dir, "other")
	writeKeyAt(t, mismatched, nil)
	writeKeyAt(t, other, nil)
	otherPub, err := config.IdentityPublicKey("", other)
	if err != nil {
		t.Fatal(err)
	}
	write(mismatched+".pub", string(gossh.MarshalAuthorizedKey(otherPub)))
	identityErrs := 0
	for _, path := range []string{noPub, badPub, notAKey, mismatched, filepath.Join(dir, "missing"), dir} {
		if _, err := config.IdentityPublicKey("shed", path); err != nil {
			identityErrs++
			msgs = append(msgs, err.Error())
		}
	}
	for _, value := range []string{"relative/key", " /lead", "/trail ", "   ", "/a\"b", "/a\\b", "/a#b", "/a%b", "/a$b", "/a\nb"} {
		if _, err := config.IdentityFileFromFlag("shed", value); err != nil {
			identityErrs++
			msgs = append(msgs, err.Error())
		}
	}
	if identityErrs < 16 {
		t.Fatalf("only %d identity-file values were refused, want all 16", identityErrs)
	}

	// The enrolment refusal, with and without an identity file.
	msgs = append(msgs,
		keysRefusedError("build-1.example.test", "", errors.New("ssh exited 255")).Error(),
		keysRefusedError("build-1.example.test", "/home/pat/.ssh/id_work", errors.New("ssh exited 255")).Error())

	for _, msg := range msgs {
		for _, cmd := range unsafeSuggestions(msg) {
			t.Errorf("a message suggests a command with shell syntax %q: %q", cmd, msg)
		}
	}
	t.Logf("read %d source strings and %d built messages", read, len(msgs))
}

// hostileValues are values a producer of a suggested command may be handed: a
// name from the command line, config.yaml or a server, a host, a path. Each
// either must be one shell word in the command, or no command is suggested.
var hostileValues = []string{
	"web", "a;b", "$(id)", "two words", "it's", `a"b`, "a|b", "a`id`b", "a&b", "a<b", "a\nshed x; id", "-rf",
	"a\x1b[2Jb",
}

// renderedProducer is a function that builds a suggested command from a
// value: render builds its text from the value; prefix is the command text
// that comes right before the value ("" when the value is never placed in a
// command); arg turns the value into what the command would carry (nil: the
// value itself); word gives that as the one word the command may carry, and
// false when it may carry none (nil: config.ShellWord); always means a
// pasteable value must draw the command.
type renderedProducer struct {
	name   string
	render func(v string) string
	prefix string
	arg    func(v string) string
	word   func(a string) (string, bool)
	always bool
}

// renderedProducers is every function in cmd/shed and internal/config that
// builds a suggested command, or a message around one, from a value. See the
// summary of this commit for the producers that are not here and why.
func renderedProducers(t *testing.T) []renderedProducer {
	cause := errors.New("ssh exited 255")
	producers := []renderedProducer{
		{name: "tunnelPortFlagHint", render: func(v string) string { return tunnelPortFlagHint("start", v) },
			prefix: "shed tunnels start ", always: true},
		{name: "imageUseHint (finishImageBuild)", render: imageUseHint, prefix: "shed create mydev --image ", always: true},
		{name: "consoleHint (create)", render: consoleHint, prefix: "shed console ", always: true},
		{name: "createShedHint (findShedServer)", render: createShedHint, prefix: "shed create ", always: true},
		{name: "snapshotLowerWarning (snapshot name)", render: func(v string) string { return snapshotLowerWarning(v, "ghcr.io/example/base:1") },
			prefix: "shed create NAME --from-snapshot ", always: true},
		{name: "snapshotLowerWarning (source image)", render: func(v string) string { return snapshotLowerWarning("snap", v) }},
		{name: "sshAliasHint (ssh-config --install)", render: func(v string) string { return sshAliasHint("shed-" + v) },
			prefix: "ssh ", arg: func(v string) string { return "shed-" + v },
			word: func(a string) (string, bool) { return a, sshconfig.PlainAlias(a) }, always: true},
		{name: "keysRefusedError (host)", render: func(v string) string { return keysRefusedError(v, "", cause).Error() }},
		{name: "keysRefusedError (identity file)", render: func(v string) string {
			return keysRefusedError("build-1.example.test", "/home/pat/"+v, cause).Error()
		}},
		{name: "unscannedHostKeyHint (home directory)", render: func(v string) string {
			t.Setenv("HOME", "/home/"+v)
			return unscannedHostKeyHint(pinnedHostKey{host: "build-1.example.test", port: 2222})
		}},
		{name: "IdentityFileFromFlag", render: func(v string) string {
			_, err := config.IdentityFileFromFlag("shed", "/home/pat/"+v)
			if err == nil {
				return ""
			}
			return err.Error()
		}},
		{name: "IdentityPublicKey (missing file)", render: func(v string) string {
			_, err := config.IdentityPublicKey("shed", filepath.Join(t.TempDir(), v))
			if err == nil {
				return ""
			}
			return err.Error()
		}},
	}
	// suggestUpdate, through every EntryRefusedError kind, with the entry's
	// name the value.
	defaultText := (&config.EntryRefusedError{Entry: "shed"}).Error()
	for k := config.RefusalKind(1); ; k++ {
		if (&config.EntryRefusedError{Entry: "shed", Kind: k}).Error() == defaultText {
			break
		}
		kind := k
		producers = append(producers, renderedProducer{
			name: fmt.Sprintf("EntryRefusedError kind %d (suggestUpdate)", kind),
			render: func(v string) string {
				return (&config.EntryRefusedError{Entry: v, Kind: kind, CertUser: "admin", EntryUser: "pat",
					Cause: errors.New("a cause"), Host: "build-1.example.test", IdentityFile: "/home/pat/.ssh/id_work",
					EntryKey: "SHA256:entry", IssuedKey: "SHA256:issued"}).Error()
			},
			prefix: "shed server update ",
		})
	}
	return producers
}

// checkRendered returns what is wrong with msg, producer p's text for the
// hostile value v: a value that is not one shell word in its command, a
// command drawn for a value no pasted word can carry, a raw control
// character from the value, or a suggested command with shell syntax.
func checkRendered(p renderedProducer, v, msg string) []string {
	var bad []string
	arg := v
	if p.arg != nil {
		arg = p.arg(v)
	}
	word, ok := config.ShellWord(arg)
	if p.word != nil {
		word, ok = p.word(arg)
	}
	if p.prefix != "" {
		switch {
		case ok && p.always && !strings.Contains(msg, p.prefix+word):
			bad = append(bad, fmt.Sprintf("no command %q", p.prefix+word))
		case ok && word != arg && strings.Contains(msg, p.prefix+arg):
			bad = append(bad, fmt.Sprintf("the value raw in the command %q", p.prefix+arg))
		case !ok && strings.Contains(msg, p.prefix+arg):
			bad = append(bad, fmt.Sprintf("a command for a value no word can carry: %q", p.prefix+arg))
		}
	}
	if strings.ContainsAny(arg, "\n\x1b") && strings.Contains(msg, arg) {
		bad = append(bad, "the value's control characters reach the text raw")
	}
	// A value no pasted word can carry is named escaped (%q) in prose, which
	// the line-based reading of suggestionsIn can take for a command when a
	// sentence starts with "ssh" ("ssh offered a key …"); its command check is
	// the prefix check above.
	if ok || p.prefix == "" {
		for _, cmd := range unsafeSuggestions(msg) {
			bad = append(bad, fmt.Sprintf("a suggested command holds shell syntax: %q", cmd))
		}
	}
	return bad
}

// TestRenderedSuggestionsQuoteTheirValues: every producer of a suggested
// command, rendered with each hostile value, gives the value as one shell word
// in its command or suggests no command.
func TestRenderedSuggestionsQuoteTheirValues(t *testing.T) {
	for _, p := range renderedProducers(t) {
		for _, v := range hostileValues {
			msg := p.render(v)
			for _, problem := range checkRendered(p, v, msg) {
				t.Errorf("%s(%q): %s, in %q", p.name, v, problem, msg)
			}
		}
	}
}

// TestRenderedCheckCatchesARawValue is that check's negative control: a
// producer that puts the value into its command raw fails it.
func TestRenderedCheckCatchesARawValue(t *testing.T) {
	raw := renderedProducer{name: "raw", render: func(v string) string {
		return "Or use -t flag: shed tunnels start " + v + " -t 3000"
	}, prefix: "shed tunnels start ", always: true}
	for _, v := range []string{"a;b", "$(id)", "two words", "a\nshed x; id", "-rf"} {
		if problems := checkRendered(raw, v, raw.render(v)); len(problems) == 0 {
			t.Errorf("checkRendered(%q) found nothing in %q", v, raw.render(v))
		}
	}
	if problems := checkRendered(raw, "web", raw.render("web")); len(problems) != 0 {
		t.Errorf("checkRendered(web) = %q, want none", problems)
	}
}

// TestHelpExemptionIsCobraOnly: the source scan skips the help fields of a
// cobra.Command literal, and reads a field of the same name in any other
// literal.
func TestHelpExemptionIsCobraOnly(t *testing.T) {
	dir := t.TempDir()
	src := "package main\n\n" +
		"import \"github.com/spf13/cobra\"\n\n" +
		"type refusal struct{ Long, Use string }\n\n" +
		"var help = &cobra.Command{Use: \"cobra-use shed x <name>\", Long: \"cobra-long shed x <name>\"}\n" +
		"var plain = cobra.Command{Example: \"cobra-example shed x <name>\"}\n" +
		"var hidden = refusal{Long: \"not-help: shed server update <name> --refetch\", Use: \"also-not-help: shed x <name>\"}\n" +
		"var hiddenPtr = &refusal{Long: \"pointer-not-help: shed y <name>\"}\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, s := range sourceStrings(t, dir) {
		all = append(all, s)
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{"not-help:", "also-not-help:", "pointer-not-help:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("a %q field outside a cobra.Command literal was not read: %q", want, all)
		}
	}
	for _, skipped := range []string{"cobra-use", "cobra-long", "cobra-example"} {
		if strings.Contains(joined, skipped) {
			t.Errorf("a cobra.Command help field was read: %q", skipped)
		}
	}
}

// TestSuggestionCheckCatchesAPlaceholder is the check's negative control: the
// forms it must catch are caught, and the safe forms pass.
func TestSuggestionCheckCatchesAPlaceholder(t *testing.T) {
	for _, s := range []string{
		"use a key in the OpenSSH format (ssh-keygen -p -f <key> rewrites it)",
		"Enrol it again: shed server update <name> --refetch",
		"run `shed tunnels stop <shed>` first",
		"put -- before it: shed source use -- <name>",
		"(shed exec x -- a | b)",
		"(ssh x $HOME)",
		"pin its key there first:\n    ssh-keyscan -p <port> <hostname> >> /home/pat/.shed/known_hosts\n",
	} {
		if len(unsafeSuggestions(s)) == 0 {
			t.Errorf("unsafeSuggestions(%q) found nothing", s)
		}
	}
	for _, s := range []string{
		"use a key in the OpenSSH format (ssh-keygen -p -f /path/to/key rewrites it)",
		"Enrol it again: shed server update 'a;b' --refetch",
		"(shed runs ssh with BatchMode=yes: ssh-add it)",
		"run `shed tunnels stop web` first",
		"the server is (not shed) <here>",
	} {
		if bad := unsafeSuggestions(s); len(bad) != 0 {
			t.Errorf("unsafeSuggestions(%q) = %q, want none", s, bad)
		}
	}
}
