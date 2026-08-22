// Command quorractl talks to a GoQuorra server from a shell.
//
// It uses the flag package rather than a command line framework. The commands
// here are four verbs with a handful of options each, and the standard
// library covers that. The dependency the old version carried for it was
// larger than this whole program.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const usage = `quorractl talks to a GoQuorra server.

Usage:
  quorractl <command> [options]

Commands:
  create    Submit a job
  get       Show one job
  list      Show jobs, newest first, with optional filters
  queues    Count the jobs in each queue
  cancel    Stop a job that has not finished
  revive    Put a dead or cancelled job back in the queue

Options common to every command:
  -server   The server address (default http://localhost:8080,
            or QUORRA_SERVER)
  -key      The API key (default from QUORRA_API_KEY)

Run "quorractl <command> -h" for the options of one command.

The key is read from the environment by preference. A key typed on a command
line is kept in the shell history and is visible in the process list to every
other user of the machine.
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "quorractl:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return errors.New("no command given")
	}

	switch args[0] {
	case "create":
		return create(args[1:], out)
	case "get":
		return get(args[1:], out)
	case "list":
		return list(args[1:], out)
	case "queues":
		return queues(args[1:], out)
	case "cancel":
		return act(args[1:], out, "cancel", "cancelled")
	case "revive":
		return act(args[1:], out, "revive", "put back in the queue")
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return nil
	default:
		fmt.Fprint(out, usage)
		return fmt.Errorf("%q is not a command", args[0])
	}
}

// reorder moves the options in front of the arguments.
//
// The flag package stops parsing at the first thing that is not an option, so
// `quorractl get 6f1c0c64 -server http://elsewhere` reads the address as a
// second job identifier and refuses the command with a message about how many
// identifiers were given. That is standard behaviour for the package and a
// surprise to everybody who meets it, because every other tool on the machine
// takes them in either order.
//
// Whether an option swallows the token after it is asked of the flag set
// rather than guessed, because -limit 20 takes one and a boolean does not.
//
// One limit, and it does not bite here: a bare negative number is read as an
// option. Every argument this tool takes is a job identifier.
func reorder(set *flag.FlagSet, args []string) []string {
	var options, rest []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Everything after -- is an argument, whatever it looks like.
		if arg == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			rest = append(rest, arg)
			continue
		}

		options = append(options, arg)

		// -name=value carries its value already.
		if strings.Contains(arg, "=") {
			continue
		}

		named := set.Lookup(strings.TrimLeft(arg, "-"))
		if named == nil {
			// Unknown. Leave it for the flag package to report by name.
			continue
		}
		if boolean, ok := named.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}

	if len(rest) == 0 {
		return options
	}

	// A -- between the two halves, always.
	//
	// Without it the flag package reads the arguments that were moved to the
	// back as options, and the first version of this function did exactly
	// that to whatever followed a -- the caller had typed. Ending the options
	// explicitly means no argument can ever be read as one, whether the
	// caller wrote a separator or not.
	return append(append(options, "--"), rest...)
}

// client holds where to send a request and how to prove who is asking.
type client struct {
	server string
	key    string
}

// common adds the options every command takes.
func common(set *flag.FlagSet) *client {
	c := &client{}
	set.StringVar(&c.server, "server", envOr("QUORRA_SERVER", "http://localhost:8080"), "the server address")
	set.StringVar(&c.key, "key", os.Getenv("QUORRA_API_KEY"), "the API key, or set QUORRA_API_KEY")
	return c
}

func (c *client) send(ctx context.Context, method, path string, body any) (map[string]any, error) {
	if c.key == "" {
		return nil, errors.New("no API key: set QUORRA_API_KEY, or pass -key")
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.server, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-API-Key", c.key)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", c.server, err)
	}
	defer func() { _ = response.Body.Close() }()

	// The body is read whatever the status, because the server explains a
	// refusal in it. Reporting the status alone sends the reader to the
	// server logs for something already in their terminal.
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var answer map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &answer); err != nil {
			return nil, fmt.Errorf("the server answered %s with something that is not JSON: %s",
				response.Status, strings.TrimSpace(string(raw)))
		}
	}

	if response.StatusCode >= 400 {
		if message, found := answer["error"].(string); found {
			return nil, fmt.Errorf("the server refused this: %s", message)
		}
		return nil, fmt.Errorf("the server answered %s", response.Status)
	}
	return answer, nil
}

func create(args []string, out io.Writer) error {
	set := flag.NewFlagSet("create", flag.ContinueOnError)
	c := common(set)

	jobType := set.String("type", "", "the job type (required)")
	payload := set.String("payload", "{}", "the payload, as JSON")
	queue := set.String("queue", "", "the queue (default \"default\")")
	priority := set.Int("priority", 0, "higher runs first")
	delay := set.Int("delay", 0, "seconds to wait before the job is ready")
	retries := set.Int("retries", -1, "retries after the first attempt, or -1 for the server default")

	if err := set.Parse(reorder(set, args)); err != nil {
		return err
	}
	if *jobType == "" {
		set.Usage()
		return errors.New("-type is required")
	}

	// Checked here rather than at the server, so that a mistake in a shell
	// quote is reported next to the shell that made it.
	if !json.Valid([]byte(*payload)) {
		return fmt.Errorf("-payload is not JSON: %s", *payload)
	}

	request := map[string]any{
		"type":          *jobType,
		"payload":       json.RawMessage(*payload),
		"priority":      *priority,
		"delay_seconds": *delay,
	}
	if *queue != "" {
		request["queue"] = *queue
	}
	if *retries >= 0 {
		request["max_retries"] = *retries
	}

	answer, err := c.send(context.Background(), http.MethodPost, "/v1/jobs", request)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "%v\n", answer["id"])
	return nil
}

func get(args []string, out io.Writer) error {
	set := flag.NewFlagSet("get", flag.ContinueOnError)
	c := common(set)
	if err := set.Parse(reorder(set, args)); err != nil {
		return err
	}
	if set.NArg() != 1 {
		set.Usage()
		return errors.New("give exactly one job identifier")
	}

	answer, err := c.send(context.Background(), http.MethodGet, "/v1/jobs/"+set.Arg(0), nil)
	if err != nil {
		return err
	}
	return print(out, answer)
}

func list(args []string, out io.Writer) error {
	set := flag.NewFlagSet("list", flag.ContinueOnError)
	c := common(set)
	limit := set.Int("limit", 20, "how many jobs to show at once")
	queue := set.String("queue", "", "only this queue")
	status := set.String("status", "", "only this status: pending, leased, succeeded, dead or cancelled")
	jobType := set.String("type", "", "only this job type")
	all := set.Bool("all", false, "follow the pages to the end")

	if err := set.Parse(reorder(set, args)); err != nil {
		return err
	}

	query := url.Values{}
	query.Set("limit", strconv.Itoa(*limit))
	for name, value := range map[string]string{"queue": *queue, "status": *status, "type": *jobType} {
		if value != "" {
			query.Set(name, value)
		}
	}

	shown := 0
	cursor := ""

	for {
		if cursor != "" {
			query.Set("before", cursor)
		}

		answer, err := c.send(context.Background(), http.MethodGet, "/v1/jobs?"+query.Encode(), nil)
		if err != nil {
			return err
		}

		rows, _ := answer["jobs"].([]any)
		if len(rows) == 0 && shown == 0 {
			fmt.Fprintln(out, "No jobs.")
			return nil
		}

		// The heading once, and only when there is something under it.
		if shown == 0 {
			fmt.Fprintf(out, "%-38s %-20s %-12s %-10s %s\n", "ID", "TYPE", "QUEUE", "STATUS", "ATTEMPTS")
		}
		for _, row := range rows {
			job, _ := row.(map[string]any)
			fmt.Fprintf(out, "%-38v %-20v %-12v %-10v %v of %v\n",
				job["id"], job["type"], job["queue"], job["status"],
				number(job["attempts"]), number(job["max_retries"])+1)
			shown++
		}

		next, _ := answer["next_cursor"].(string)

		// Without -all the tool stops after one page and says how to see the
		// rest, rather than deciding for somebody that they wanted every job
		// in a table holding a month of them.
		if next == "" {
			return nil
		}
		if !*all {
			fmt.Fprintf(out, "\n%d shown. There are more: add -all, or -before %s\n", shown, next)
			return nil
		}
		cursor = next
	}
}

func queues(args []string, out io.Writer) error {
	set := flag.NewFlagSet("queues", flag.ContinueOnError)
	c := common(set)
	if err := set.Parse(reorder(set, args)); err != nil {
		return err
	}

	answer, err := c.send(context.Background(), http.MethodGet, "/v1/queues", nil)
	if err != nil {
		return err
	}

	rows, _ := answer["queues"].([]any)
	if len(rows) == 0 {
		fmt.Fprintln(out, "No jobs.")
		return nil
	}

	fmt.Fprintf(out, "%-20s %-12s %s\n", "QUEUE", "STATUS", "COUNT")
	for _, row := range rows {
		stat, _ := row.(map[string]any)
		fmt.Fprintf(out, "%-20v %-12v %v\n", stat["queue"], stat["status"], number(stat["count"]))
	}
	return nil
}

// act runs one of the verbs that change a job.
//
// cancel and revive differ by a word each. Writing the flag parsing, the
// argument check and the error handling twice is how the two of them end up
// answering differently to the same mistake.
func act(args []string, out io.Writer, verb, done string) error {
	set := flag.NewFlagSet(verb, flag.ContinueOnError)
	c := common(set)
	if err := set.Parse(reorder(set, args)); err != nil {
		return err
	}
	if set.NArg() != 1 {
		set.Usage()
		return errors.New("give exactly one job identifier")
	}

	id := set.Arg(0)
	answer, err := c.send(context.Background(), http.MethodPost, "/v1/jobs/"+id+"/"+verb, nil)
	if err != nil {
		return err
	}

	status, _ := answer["status"].(string)
	fmt.Fprintf(out, "%s %s (now %s)\n", id, done, status)
	return nil
}

func print(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// number turns a JSON number into an int. Every number in a decoded JSON
// document is a float64, and printing one straight gives 3 as "3" but 1e+06
// as "1e+06".
func number(value any) int {
	if asFloat, ok := value.(float64); ok {
		return int(asFloat)
	}
	return 0
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
