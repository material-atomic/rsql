package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/material-atomic/rsql/internal/store"
)

// Line editing, when there is somebody there to edit.
//
// Everything that decides anything is in complete.go and shell.go, where it is
// tested. What is here is wiring: put the terminal in raw mode, hand the
// keystrokes to code that has tests, put it back. A file this hard to test is
// a file that must not be worth testing.
//
// The plain path — a reader, a line at a time — is not a fallback that has
// rotted. It is how the shell runs under a pipe, in a script, and in every
// test in this package, so it is exercised far more than this is.

// lineReader is where the shell's next line comes from. It writes the prompt
// too, because a terminal in raw mode writes its own and nobody else may.
type lineReader func() (string, error)

// editing sets up whatever kind of input this is, and returns the reader and
// the way to put the terminal back.
func editing(in io.Reader, out io.Writer, prompt string, here store.Catalogue) (lineReader, func()) {
	file, isFile := in.(*os.File)
	if !isFile || !term.IsTerminal(int(file.Fd())) {
		return plain(in, out, prompt), func() {}
	}

	state, err := term.MakeRaw(int(file.Fd()))
	if err != nil {
		// A terminal that will not go into raw mode still reads lines.
		return plain(in, out, prompt), func() {}
	}
	restore := func() { _ = term.Restore(int(file.Fd()), state) }

	screen := term.NewTerminal(both{in: file, out: out}, prompt)
	screen.AutoCompleteCallback = func(line string, pos int, key rune) (string, int, bool) {
		if key != '\t' {
			return "", 0, false
		}
		return completing(line, pos, here, screen)
	}

	return func() (string, error) { return screen.ReadLine() }, restore
}

// completing is one press of tab.
//
// Split out from the callback so that what it decides is reachable without a
// terminal: one candidate finishes the word, several move as far as they agree
// on, and when they agree on nothing the list is printed rather than a choice
// being made.
func completing(line string, pos int, here store.Catalogue, screen io.Writer) (string, int, bool) {
	before, after := line[:pos], line[pos:]

	offered := suggest(before, here)
	if len(offered) == 0 {
		return "", 0, false
	}

	typed := lastWord(before)
	grown := shared(offered)
	if len(grown) > len(typed) {
		filled := before[:len(before)-len(typed)] + grown
		if len(offered) == 1 {
			filled += " "
		}
		return filled + after, len(filled), true
	}

	// Nothing to fill in, so show what the choices are. The terminal repaints
	// the prompt and the line afterwards, so this does not lose what was typed.
	fmt.Fprintf(screen, "\r\n%s\r\n", strings.Join(offered, "  "))
	return line, pos, true
}

// lastWord is the part being typed, which is nothing when the line ends in a
// space.
func lastWord(line string) string {
	if strings.HasSuffix(line, " ") || line == "" {
		return ""
	}
	words := strings.Fields(line)
	if len(words) == 0 {
		return ""
	}
	return words[len(words)-1]
}

// both is stdin and stdout as one thing, which is what a terminal is.
type both struct {
	in  io.Reader
	out io.Writer
}

func (b both) Read(p []byte) (int, error)  { return b.in.Read(p) }
func (b both) Write(p []byte) (int, error) { return b.out.Write(p) }

// plain reads lines from something that is not a terminal.
//
// The prompt is written here rather than by the caller, because a terminal
// writes its own — and a shell that printed one as well showed it twice, which
// is exactly the sort of thing that only turns up when somebody runs it.
func plain(in io.Reader, out io.Writer, prompt string) lineReader {
	lines := newScanner(in)
	return func() (string, error) {
		fmt.Fprint(out, prompt)
		if !lines.Scan() {
			if err := lines.Err(); err != nil {
				return "", err
			}
			return "", io.EOF
		}
		return lines.Text(), nil
	}
}
