package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"

	"chains-automation/operator"
	"github.com/charmbracelet/x/ansi"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "chainsctl:", clean(err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	usage := func() {
		fmt.Fprintln(diagnostics, "Usage: chainsctl {state|describe|watch|send|tui} [options]\nConfigure chainsctl.toml or set CHAINS_OPERATOR_TOKEN and optionally CHAINS_OPERATOR_URL.")
	}
	if len(args) == 0 {
		usage()
		return errors.New("choose a command")
	}
	command := args[0]
	f := flag.NewFlagSet("chainsctl "+command, flag.ContinueOnError)
	f.SetOutput(diagnostics)
	address := f.String("url", "", "operator base URL")
	configPath := f.String("config", "", "configuration file")
	format := new(string)
	if command != "tui" {
		format = f.String("output", "table", "table, json or csv")
	}
	var filter operator.Filter
	var recent int
	var profile, name, data, file string
	switch command {
	case "-h", "--help", "help":
		usage()
		return nil
	case "state", "describe", "tui":
	case "watch":
		f.IntVar(&recent, "recent", 0, "number of recent records to replay")
		f.BoolVar(&filter.All, "all", false, "show heartbeats and state synchronization")
		f.StringVar(&filter.Type, "type", "", "comma-separated types, supports a final * wildcard")
		f.StringVar(&filter.Service, "service", "", "service ID")
		f.StringVar(&filter.Device, "device", "", "device ID")
		f.StringVar(&filter.Engine, "engine", "", "engine ID")
		f.StringVar(&filter.Rule, "rule", "", "rule name")
		f.StringVar(&filter.Correlation, "correlation", "", "correlation ID")
		f.StringVar(&filter.Search, "search", "", "case-insensitive text")
	case "send":
		f.StringVar(&profile, "profile", "", "configured event profile")
		f.StringVar(&name, "name", "", "custom manual event name")
		f.StringVar(&data, "data", "", "JSON payload")
		f.StringVar(&file, "file", "", "JSON file, or - for standard input")
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
	var selector string
	params := args[1:]
	if (command == "state" || command == "describe") && len(params) > 0 && !strings.HasPrefix(params[0], "-") {
		selector, params = params[0], params[1:]
	}
	if err := f.Parse(params); err != nil {
		return err
	}
	if (command != "state" && command != "describe" && f.NArg() != 0) || f.NArg() > 1 || (selector != "" && f.NArg() != 0) {
		return errors.New("unexpected positional arguments")
	}
	if command != "tui" && *format != "table" && *format != "json" && *format != "csv" {
		return errors.New("output must be table, json or csv")
	}
	url, token, err := operatorConnection(ctx, *configPath, *address)
	if err != nil {
		return err
	}
	client, err := operator.NewClient(url, token)
	if err != nil {
		return err
	}
	switch command {
	case "describe":
		if f.NArg() == 1 {
			selector = f.Arg(0)
		}
		services, err := client.Services(ctx)
		if err != nil {
			return err
		}
		return printDescriptions(out, *format, selector, services)
	case "state":
		var path []string
		if f.NArg() == 1 {
			selector = f.Arg(0)
		}
		if selector != "" {
			path = strings.Split(selector, "/")
		}
		state, err := client.State(ctx, path)
		if err != nil {
			return err
		}
		return printState(out, *format, state)
	case "watch":
		if recent < 0 || recent > operator.HistorySize {
			return fmt.Errorf("recent must be between 0 and %d", operator.HistorySize)
		}
		return printWatch(ctx, out, *format, client, recent, filter)
	case "send":
		if (profile == "") == (name == "") || data != "" && file != "" {
			return errors.New("supply one of --profile or --name, and at most one of --data or --file")
		}
		if file != "" {
			if file != "-" {
				handle, err := os.Open(file)
				if err != nil {
					return err
				}
				defer handle.Close()
				in = handle
			}
			const limit = 1 << 20
			raw, err := io.ReadAll(io.LimitReader(in, limit+1))
			if err != nil {
				return err
			}
			if len(raw) > limit {
				return errors.New("event payload exceeds 1 MiB")
			}
			data = string(raw)
		}
		if data == "" && file == "" {
			data = "{}"
		}
		if !json.Valid([]byte(data)) {
			return errors.New("event data must be valid JSON")
		}
		receipt, err := client.Send(ctx, operator.Submission{Profile: profile, Name: name, Data: json.RawMessage(data)})
		if err != nil {
			return err
		}
		if *format == "json" {
			return json.NewEncoder(out).Encode(receipt)
		}
		return printRows(out, *format, []string{"ID", "TYPE", "SOURCE"}, [][]string{{receipt.ID, receipt.Type, receipt.Source}})
	case "tui":
		return runTUI(ctx, client, in, out)
	}
	return nil
}

func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, value)
}
func tableText(value string, width int) string { return ansi.Truncate(clean(value), width, "...") }

func timestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}
func optionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return timestamp(*t)
}

func printRows(out io.Writer, format string, header []string, rows [][]string) error {
	if format == "csv" {
		writer := csv.NewWriter(out)
		if err := writer.Write(header); err != nil {
			return err
		}
		return writer.WriteAll(rows)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, strings.Join(header, "\t"))
	for _, row := range rows {
		safe := make([]string, len(row))
		for i, cell := range row {
			safe[i] = clean(cell)
		}
		_, _ = fmt.Fprintln(w, strings.Join(safe, "\t"))
	}
	return w.Flush()
}

func stateValueText(r operator.StateRow) string {
	if r.Kind == "attribute" && len(r.Value) == 0 {
		return "Unreported"
	}
	return string(r.Value)
}

func printState(out io.Writer, format string, state operator.State) error {
	if format == "json" {
		return json.NewEncoder(out).Encode(state)
	}
	var rows [][]string
	if format == "csv" {
		for _, r := range state.Rows {
			rows = append(rows, []string{state.Epoch, strconv.FormatUint(state.Revision, 10), r.Kind, r.Service, r.ServiceName, r.Device, r.DeviceName, r.Attribute, string(r.Value), optionalTime(r.Observed), optionalTime(r.Changed), strconv.FormatUint(r.Revision, 10)})
		}
		return printRows(out, format, []string{"epoch", "state_revision", "kind", "service_id", "service_name", "device_id", "device_name", "attribute", "value", "observed_at", "changed_at", "revision"}, rows)
	}
	for _, r := range state.Rows {
		path := r.Service
		label := r.ServiceName
		if r.Device != "" {
			path += "/" + r.Device
			label = r.DeviceName
		}
		if r.Attribute != "" {
			path += "/" + r.Attribute
			label = ""
		}
		rows = append(rows, []string{tableText(path, 64), tableText(label, 28), tableText(stateValueText(r), 54), optionalTime(r.Changed)})
	}
	return printRows(out, format, []string{"PATH", "NAME", "VALUE", "CHANGED"}, rows)
}

func printWatch(ctx context.Context, out io.Writer, format string, client *operator.Client, recent int, filter operator.Filter) error {
	encoder := json.NewEncoder(out)
	csvWriter := csv.NewWriter(out)
	switch format {
	case "csv":
		if err := csvWriter.WriteAll([][]string{{"time", "type", "source", "service_id", "device_id", "target", "engine_id", "rule", "run_id", "event_id", "action_id", "causation_id", "correlation_id", "summary", "data", "truncated", "id"}}); err != nil {
			return err
		}
	case "table":
		if _, err := fmt.Fprintf(out, "%-12s  %-27s  %-23s  %s\n", "TIME", "TYPE", "SUBJECT", "DETAIL"); err != nil {
			return err
		}
	}
	return client.Watch(ctx, recent, func(r operator.Record) error {
		if !filter.Match(r) {
			return nil
		}
		switch format {
		case "json":
			return encoder.Encode(r)
		case "csv":
			return csvWriter.WriteAll([][]string{{timestamp(r.At), r.Type, r.Source, r.Service, recordDevice(r), r.Target, r.Engine, r.Rule, r.Run, r.Event, r.Action, r.Cause, r.Correlation, r.Summary, string(r.Data), strconv.FormatBool(r.Truncated), r.ID}})
		default:
			subject, detail := activityText(r)
			_, err := fmt.Fprintf(out, "%-12s  %-27s  %-23s  %s\n", r.At.Local().Format("15:04:05.000"), tableText(r.Type, 27), tableText(subject, 23), tableText(detail, 96))
			return err
		}
	})
}

func activityText(r operator.Record) (string, string) {
	subject := r.Service
	if r.Rule != "" {
		subject = r.Engine + "/" + r.Rule
	} else if device := recordDevice(r); device != "" {
		subject += "/" + device
	} else if subject == "" {
		subject = r.Engine
	}
	detail := r.Summary
	if detail == "" {
		detail = string(r.Data)
	}
	return subject, detail
}

func recordDevice(r operator.Record) string {
	if len(r.Devices) > 0 {
		return strings.Join(r.Devices, ", ")
	}
	return r.Device
}
