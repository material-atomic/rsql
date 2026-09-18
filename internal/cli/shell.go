package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/material-atomic/rsql/internal/connection"
	"github.com/material-atomic/rsql/internal/signing"
	"github.com/material-atomic/rsql/internal/store"
	"github.com/material-atomic/rsql/internal/wire"
)

// The shell an operator types at.
//
// What somebody can type here is a list of keywords in fixed positions. That
// is not a simplification of something richer that comes later — it is the
// whole design. A shell with an expression language is a second interface into
// the database, more powerful than the one the application uses and reachable
// by anyone who reaches the port, and the industry's own answer to having
// built one is a configuration flag that turns it off in production.
//
// So: a document by its key, a stretch of a declared index, a count of one,
// and a list of what is here. Values are written as JSON, because that is the
// one notation where "12" and 12 are visibly different things, which matters
// when a key might be either.
//
// Every session ends the same way: `declare` prints the operation that would
// do what you just did. Exploring that leaves nothing behind becomes a habit
// of exploring; exploring that ends in a declaration becomes a schema.

const shellHelp = `  ls                                 what this database holds
  get <collection> <key>             one document by its key
  scan <collection> [index] [...]    a stretch of an index
  count <collection> [index] [...]   how many are in that stretch
  declare [name]                     the operation that would do the last thing
  help                               this
  exit                               leave

  A scan or a count takes, in any order:
    from <value>...       where to start   (after <value>... to exclude it)
    to <value>...         where to stop    (before <value>... to exclude it)
    limit <n>             how many rows at most
    fields <a> <b>...     which fields to show

  Values are JSON: "a string", 42, true, null.
`

// Looking is the part of a connection the shell uses. An interface because the
// shell is worth testing without a server, and because what it needs from a
// connection is exactly this much.
type Looking interface {
	WhatIsHere() (store.Catalogue, error)
	Explore(store.Access) (wire.Explored, error)
}

// Shell reads lines and runs them until the input ends.
func Shell(look Looking, dbname string, in io.Reader, out io.Writer) error {
	// Asked for once, at the start, because completion needs it and because an
	// operator's first question is always what is here. Best effort: a shell
	// that could not fetch it still works, it just has nothing to offer, and
	// guessing names would be worse than silence.
	here, _ := look.WhatIsHere()

	prompt := dbname + "> "
	next, restore := editing(in, out, prompt, here)
	defer restore()

	// The last draft, which is what `declare` prints. Kept rather than
	// recomputed so that what is printed is what ran, not what a second pass
	// through the parser thinks ran.
	var drafted *store.Operation

	fmt.Fprintf(out, "rsql %s — type help, or exit when you are done\n", dbname)

	for {
		line, err := next()
		if err != nil {
			fmt.Fprintln(out)
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		done, draft, err := one(look, line, drafted, out)
		if err != nil {
			// A mistake is not the end of a session. An operator who mistypes
			// a bound at two in the morning should see why and try again.
			fmt.Fprintf(out, "  %v\n", err)
		}
		if draft != nil {
			drafted = draft
		}
		if done {
			return nil
		}
	}
}

// newScanner is the line reader the plain path uses, with a buffer big enough
// for a pasted line.
func newScanner(in io.Reader) *bufio.Scanner {
	lines := bufio.NewScanner(in)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	return lines
}

// one runs a single line, and says whether the session is over and what draft
// it produced.
func one(look Looking, line string, drafted *store.Operation, out io.Writer) (bool, *store.Operation, error) {
	words := strings.Fields(line)

	// Extra words are refused everywhere, not only where they could have
	// meant something. `ls something` ran as `ls` until somebody typed it by
	// accident: a shell that ignores what it does not understand does a
	// different thing from the one that was typed, and says nothing about it.
	if most, counted := taken[words[0]]; counted && len(words) > most {
		return false, nil, fmt.Errorf("%q takes nothing after it, and got %q", words[0], words[1])
	}

	switch words[0] {
	case "exit", "quit":
		return true, nil, nil

	case "help", "?":
		fmt.Fprint(out, shellHelp)
		return false, nil, nil

	case "ls":
		here, err := look.WhatIsHere()
		if err != nil {
			return false, nil, err
		}
		showCatalogue(here, out)
		return false, nil, nil

	case "declare":
		if drafted == nil {
			return false, nil, fmt.Errorf("nothing has been looked at yet, so there is nothing to declare")
		}
		named := *drafted
		if len(words) > 1 {
			named.Name = words[1]
		}
		named.Version = 0
		encoded, err := json.MarshalIndent(named, "", "  ")
		if err != nil {
			return false, nil, err
		}
		fmt.Fprintln(out, string(encoded))
		return false, nil, nil

	case "get", "scan", "count":
		access, err := access(words)
		if err != nil {
			return false, nil, err
		}
		answer, err := look.Explore(access)
		if err != nil {
			return false, nil, err
		}
		showResult(answer.Draft.Action, answer.Result, out)
		return false, &answer.Draft, nil
	}

	return false, nil, fmt.Errorf("there is no %q here; type help", words[0])
}

// taken is how many words each command may be, for the ones that are not a
// whole little grammar of their own. A command missing from here takes as many
// as it likes and checks them itself.
var taken = map[string]int{
	"exit": 1, "quit": 1, "help": 1, "?": 1, "ls": 1, "declare": 2,
}

// access turns a typed line into the access it asks for.
//
// Everything this does not understand is refused by name. A shell that
// silently ignores a word it does not know is one that runs a different query
// from the one somebody typed.
func access(words []string) (store.Access, error) {
	asked := store.Access{Kind: words[0]}

	if len(words) < 2 {
		return store.Access{}, fmt.Errorf("%s what? name a collection", words[0])
	}
	asked.Collection = words[1]

	rest := words[2:]

	if asked.Kind == "get" {
		if len(rest) != 1 {
			return store.Access{}, fmt.Errorf("get takes one key, written as JSON: get %s \"some-id\"", asked.Collection)
		}
		key, err := literal(rest[0])
		if err != nil {
			return store.Access{}, err
		}
		asked.Key = key
		return asked, nil
	}

	// A bare word straight after the collection is the index. Anything else is
	// a keyword, and the clustered index is what you get by saying nothing.
	//
	// Which means a line that starts like another database's query language —
	// "scan books where shelf = ..." — has its first wrong word read as an
	// index name, and the complaint lands on the second one. So a failure says
	// what the line was understood to be. A parser that reports the wrong word
	// sends somebody looking at the wrong half of what they typed.
	read := asked.Kind + " " + asked.Collection
	if len(rest) > 0 && !keyword(rest[0]) {
		asked.Index = rest[0]
		rest = rest[1:]
		read += ", index " + quoted(asked.Index)
	}

	for len(rest) > 0 {
		word := rest[0]
		rest = rest[1:]

		switch word {
		case "from", "after", "to", "before":
			values, remaining, err := until(rest)
			if err != nil {
				return store.Access{}, fmt.Errorf("%s: %w", word, err)
			}
			if len(values) == 0 {
				return store.Access{}, fmt.Errorf("%s what? give it at least one value", word)
			}
			bound := &store.Bound{Values: values, Exclusive: word == "after" || word == "before"}
			if word == "from" || word == "after" {
				asked.From = bound
			} else {
				asked.To = bound
			}
			rest = remaining

		case "limit":
			if len(rest) == 0 {
				return store.Access{}, fmt.Errorf("limit what? give it a number")
			}
			value, err := literal(rest[0])
			if err != nil {
				return store.Access{}, err
			}
			count, isNumber := value.(float64)
			if !isNumber || count != float64(int(count)) || count < 1 {
				return store.Access{}, fmt.Errorf("limit takes a whole number of rows, not %s", rest[0])
			}
			asked.Limit = int(count)
			rest = rest[1:]

		case "fields":
			for len(rest) > 0 && !keyword(rest[0]) {
				asked.Projection = append(asked.Projection, rest[0])
				rest = rest[1:]
			}
			if len(asked.Projection) == 0 {
				return store.Access{}, fmt.Errorf("fields what? name at least one")
			}

		default:
			return store.Access{}, fmt.Errorf("there is no %q in a %s — this was read as: %s; type help",
				word, asked.Kind, read)
		}
	}

	return asked, nil
}

// until reads JSON values up to the next keyword.
func until(words []string) ([]any, []string, error) {
	values := []any{}
	for len(words) > 0 && !keyword(words[0]) {
		value, err := literal(words[0])
		if err != nil {
			return nil, nil, err
		}
		values = append(values, value)
		words = words[1:]
	}
	return values, words, nil
}

// quoted is a word as it should appear inside a message about itself.
func quoted(word string) string { return "\"" + word + "\"" }

func keyword(word string) bool {
	switch word {
	case "from", "after", "to", "before", "limit", "fields":
		return true
	}
	return false
}

// literal reads one value the way it was written.
//
// JSON rather than bare words, so that a key of "12" and a key of 12 are
// different things on the screen as well as in the store — which is exactly
// the confusion somebody debugging at two in the morning does not need.
func literal(word string) (any, error) {
	var value any
	if err := json.Unmarshal([]byte(word), &value); err != nil {
		return nil, fmt.Errorf("%s is not a value; write a string in quotes, or a number, true, false or null", word)
	}
	return value, nil
}

func showResult(action string, result store.Result, out io.Writer) {
	if action == store.ActionCount {
		fmt.Fprintf(out, "  %d\n", result.Count)
	}
	if action != store.ActionCount && len(result.Rows) == 0 {
		// Not "0". A count answers with a number and a scan answers with rows,
		// and printing a number for an empty scan reads as though the question
		// asked for one.
		fmt.Fprintln(out, "  nothing")
	}
	for _, row := range result.Rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			fmt.Fprintf(out, "  %v\n", err)
			continue
		}
		fmt.Fprintf(out, "  %s\n", encoded)
	}
	if result.Truncated {
		fmt.Fprintf(out, "  ... and more: this stopped at the limit\n")
	}
}

func showCatalogue(here store.Catalogue, out io.Writer) {
	for _, spec := range here.Collections {
		fmt.Fprintf(out, "  collection %s (key %s %s)\n", spec.Name, spec.Key.Path, spec.Key.Type)
		for _, index := range spec.Indexes {
			fields := make([]string, 0, len(index.Fields))
			for _, field := range index.Fields {
				fields = append(fields, field.Path)
			}
			fmt.Fprintf(out, "    index %s (%s)\n", index.Name, strings.Join(fields, ", "))
		}
	}
	for _, operation := range here.Operations {
		fmt.Fprintf(out, "  operation %s v%d %s %s\n",
			operation.Name, operation.Version, operation.Action, operation.Collection)
	}
}

// connect makes this tool's own connection string and opens it.
//
// Nothing is typed and nothing is passed as an argument. The tool already
// holds the secret — that is what makes it the operator — so it signs a string
// for itself with a password it just generated, which lives for one
// connection. A password on a command line is visible to anyone who can run
// ps, and a shell that asked for one would be teaching the habit of pasting
// credentials into a terminal.
func connect(opts options, address string, insecure bool) (*wire.Client, error) {
	password, err := makePassword()
	if err != nil {
		return nil, err
	}
	signature, err := signing.Sign(
		signing.Parts{AccountID: opts.account, Password: password, DBName: opts.db},
		opts.secret, opts.label,
	)
	if err != nil {
		return nil, err
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not host:port", ErrUsage, address)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a port", ErrUsage, port)
	}

	client, err := wire.Dial(connection.Connection{
		Account: opts.account, Password: password, Host: host, Port: number,
		DBName: opts.db, Signature: signature,
	}, wire.Options{Insecure: insecure})
	if err != nil {
		return nil, err
	}

	// Being able to reach a database is not being allowed to explore it. This
	// proves the secret over a challenge the server just chose.
	if err := client.Operate(opts.secret); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// shell is the command.
func shell(opts options, args []string, in io.Reader, out io.Writer) error {
	address := "localhost:7433"
	if len(args) > 0 {
		address = args[0]
	}
	insecure := false
	for _, arg := range args[1:] {
		if arg != "-insecure" {
			return fmt.Errorf("%w: shell takes host:port and optionally -insecure, not %q", ErrUsage, arg)
		}
		insecure = true
	}

	client, err := connect(opts, address, insecure)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	return Shell(client, opts.db, in, out)
}
