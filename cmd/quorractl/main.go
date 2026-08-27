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
  history   Show what a job did, run by run
  workers   Show the workers the queue has heard from
  cancel    Stop a job that has not finished
  revive    Put a dead or cancelled job back in the queue
  whoami    Show the name and the scope of the key in use

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
	case "history":
		return history(args[1:], out)
	case "workers":
		return workers(args[1:], out)
	case "cancel":
		return act(args[1:], out, "cancel", "cancelled")
	case "revive":
		return act(args[1:], out, "revive", "put back in the queue")
	case "whoami":
		return whoami(args[1:], out)
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
	after := set.String("after", "", "run only after these jobs succeed, as a comma separated list of identifiers")

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
	if waits := identifiers(*after); len(waits) > 0 {
		request["after"] = waits
	}

	answer, err := c.send(context.Background(), http.MethodPost, "/v1/jobs", request)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "%v\n", answer["id"])

	// A job that is waiting says so on a second line. The identifier stays
	// alone on the first, because a shell reads it with a pipe and a line
	// that grew a second word would break every one of those.
	if status, _ := answer["status"].(string); status == "blocked" {
		fmt.Fprintf(out, "It waits for %d job(s) and is not queued yet.\n", len(identifiers(*after)))
	}
	return nil
}

// identifiers splits a comma separated list and drops the empty pieces.
//
// A trailing comma, or a shell that expanded an empty variable, would
// otherwise send an empty identifier, and the server answers that no such job
// exists, which reads as a mistake somebody made rather than one they did
// not.
func identifiers(text string) []string {
	var out []string
	for _, piece := range strings.Split(text, ",") {
		if trimmed := strings.TrimSpace(piece); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
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

// history shows what a job did, run by run.
//
// A table and not the JSON that get prints. The question this answers is
// "which worker kept failing, and was it getting slower", and that is read
// down a column rather than out of a nested document.
func history(args []string, out io.Writer) error {
	set := flag.NewFlagSet("history", flag.ContinueOnError)
	c := common(set)
	if err := set.Parse(reorder(set, args)); err != nil {
		return err
	}
	if set.NArg() != 1 {
		set.Usage()
		return errors.New("give exactly one job identifier")
	}

	id := set.Arg(0)
	answer, err := c.send(context.Background(), http.MethodGet, "/v1/jobs/"+id+"/attempts", nil)
	if err != nil {
		return err
	}

	rows, _ := answer["attempts"].([]any)
	if len(rows) == 0 {
		fmt.Fprintln(out, "This job has not finished a run.")
		return nil
	}

	fmt.Fprintf(out, "%-4s %-20s %-10s %-10s %s\n", "RUN", "WORKER", "OUTCOME", "TOOK", "ERROR")
	for _, row := range rows {
		run, _ := row.(map[string]any)
		worker, _ := run["worker"].(string)
		if worker == "" {
			worker = "unknown"
		}
		// The error is read as a string rather than printed with %v. It is
		// omitempty, so a run that worked carries no key at all, and %v on
		// the missing value prints the word nil in every one of those rows.
		reason, _ := run["error"].(string)

		fmt.Fprintf(out, "%-4d %-20s %-10v %-10s %s\n",
			number(run["attempt"]), worker, run["outcome"],
			took(run["started_at"], run["finished_at"]), reason)
	}
	return nil
}

// took gives how long a run lasted, or a dash when that is not known.
//
// A job leased by a build older than the history has no recorded start. A
// dash says so. Printing a duration measured from the zero time would give
// every one of those rows the same wrong answer, in years.
func took(started, finished any) string {
	from, ok := moment(started)
	if !ok {
		return "-"
	}
	to, ok := moment(finished)
	if !ok {
		return "-"
	}
	return to.Sub(from).Round(time.Millisecond).String()
}

// moment reads a time out of a decoded JSON document.
func moment(value any) (time.Time, bool) {
	text, ok := value.(string)
	if !ok || text == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// workers shows the workers the queue has heard from.
//
// The answer to "is anything out there", which no other command gives. A
// queue with a thousand waiting jobs and no worker looks exactly like a busy
// one in list and in queues.
func workers(args []string, out io.Writer) error {
	set := flag.NewFlagSet("workers", flag.ContinueOnError)
	c := common(set)
	if err := set.Parse(reorder(set, args)); err != nil {
		return err
	}

	answer, err := c.send(context.Background(), http.MethodGet, "/v1/workers", nil)
	if err != nil {
		return err
	}

	rows, _ := answer["workers"].([]any)
	if len(rows) == 0 {
		fmt.Fprintln(out, "No worker has asked for work.")
		return nil
	}

	fmt.Fprintf(out, "%-24s %-16s %-12s %s\n", "WORKER", "QUEUE", "IDLE", "FIRST SEEN")
	for _, row := range rows {
		one, _ := row.(map[string]any)
		idle, _ := one["idle_seconds"].(float64)
		first, _ := one["first_seen_at"].(string)
		fmt.Fprintf(out, "%-24v %-16v %-12s %s\n",
			one["id"], one["queue"],
			(time.Duration(idle * float64(time.Second))).Round(time.Second),
			runAt(first))
	}
	return nil
}

// whoami says which key this shell holds.
//
// A profile that exports QUORRA_API_KEY gives no hint of which key it is,
// and a key that may only read looks exactly like one that may write until
// something is refused. Asking is cheaper than finding out from a 403 in the
// middle of clearing a dead letter queue.
func whoami(args []string, out io.Writer) error {
	set := flag.NewFlagSet("whoami", flag.ContinueOnError)
	c := common(set)
	if err := set.Parse(reorder(set, args)); err != nil {
		return err
	}

	answer, err := c.send(context.Background(), http.MethodGet, "/v1/whoami", nil)
	if err != nil {
		return err
	}

	name, _ := answer["name"].(string)
	scope, _ := answer["scope"].(string)
	fmt.Fprintf(out, "%s (may %s)\n", name, scope)
	return nil
}

func list(args []string, out io.Writer) error {
	set := flag.NewFlagSet("list", flag.ContinueOnError)
	c := common(set)
	limit := set.Int("limit", 20, "how many jobs to show at once")
	queue := set.String("queue", "", "only this queue")
	status := set.String("status", "", "only this status: blocked, pending, leased, succeeded, dead or cancelled")
	jobType := set.String("type", "", "only this job type")
	worker := set.String("worker", "", "only the jobs this worker is holding")
	ready := set.Bool("ready", false, "only the jobs the queue would hand out now")
	soonest := set.Bool("soonest", false, "the job that runs first, first, rather than the newest first")
	before := set.String("before", "", "start after this job, from a previous page")
	all := set.Bool("all", false, "follow the pages to the end")

	if err := set.Parse(reorder(set, args)); err != nil {
		return err
	}

	query := url.Values{}
	query.Set("limit", strconv.Itoa(*limit))
	for name, value := range map[string]string{
		"queue": *queue, "status": *status, "type": *jobType, "worker": *worker,
	} {
		if value != "" {
			query.Set(name, value)
		}
	}
	if *ready {
		// Two conditions and not one. A job the queue would hand out now is
		// pending and due, and due alone matches every job that has ever run,
		// because a finished job keeps the run_at of its last attempt.
		query.Set("due", "now")
		query.Set("status", "pending")
	}
	if *soonest {
		query.Set("order", "soonest")
	}

	shown := 0
	cursor := *before

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
		//
		// The run at column appears when the order or the filter is about
		// when a job runs. A list sorted by a value it does not show reads as
		// a list that is not sorted at all.
		when := *soonest || *ready
		if shown == 0 {
			fmt.Fprintf(out, "%-38s %-20s %-12s %-10s %s", "ID", "TYPE", "QUEUE", "STATUS", "ATTEMPTS")
			if when {
				fmt.Fprintf(out, "  %s", "RUNS AT")
			}
			fmt.Fprintln(out)
		}
		for _, row := range rows {
			job, _ := row.(map[string]any)
			fmt.Fprintf(out, "%-38v %-20v %-12v %-10v %v of %v",
				job["id"], job["type"], job["queue"], job["status"],
				number(job["attempts"]), number(job["max_retries"])+1)
			if when {
				fmt.Fprintf(out, "  %v", runAt(job["run_at"]))
			}
			fmt.Fprintln(out)
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
	fmt.Fprintf(out, "%s %s (now %s)", id, done, status)

	// Which name the queue wrote against the job, and not which key this
	// shell holds. An operator with several keys in a profile finds out here
	// that the action went down under the wrong one, rather than a month
	// later when somebody reads the job.
	if by, _ := answer["acted_by"].(string); by != "" {
		fmt.Fprintf(out, ", by %s", by)
	}
	fmt.Fprintln(out)
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
// runAt renders a moment for the table.
//
// The time of day when the job runs today, and the date when it does not. A
// full timestamp on every row is the widest thing in the table and is read by
// nobody, and the reason to look at this column is to tell soon from later.
func runAt(value any) string {
	text, _ := value.(string)
	moment, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return text
	}
	moment = moment.Local()
	if moment.Format("2006-01-02") == time.Now().Format("2006-01-02") {
		return moment.Format("15:04:05")
	}
	return moment.Format("2006-01-02 15:04")
}

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
