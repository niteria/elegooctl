package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/niteria/elegooctl/internal/printer"
)

const help = `elegooctl --host HOST [connection options] COMMAND [options]

Connection options (before COMMAND):
  --host HOST             Printer hostname or IP; required
  --sdcp-port PORT        SDCP WebSocket port (3030)
  --http-port PORT        Upload/download HTTP port (80)
  --camera-port PORT      MJPEG camera HTTP port (3031)
  --notify-command PATH   Optional executable for removal-ready events (JSON stdin)

Commands:
  status                  Print raw printer status as JSON
  attributes              Print printer attributes as JSON
  query --command N --data JSON
                          Read-only file/history query (258,260,320,321,323)
  upload [--wait-idle] [--verify-only] [--timeout 2h] FILE
                          Upload and verify; never starts printing
  snapshot --output FILE  Capture a JPEG without moving the printer
  watch [--file NAME] [--temperature 30] [--timeout 24h] [--output FILE]
                          Wait for selected/current job to finish and cool
  record [--file NAME] --output NEW_DIRECTORY [--timeout 24h]
         [--max-duration 6h] [--camera-retries 5]
                          Record current/selected job; needs ffmpeg
  jog --axis X|Y|Z --step MM
                          Explicit relative move on an idle, homed printer

Use --file to wait for a future job; without it watch/record attach to the
currently printing job. Global options must precede the command. No command
starts prints or homes the printer. Only jog requests motion.
`

func output(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	global := flag.NewFlagSet("elegooctl", flag.ContinueOnError)
	global.SetOutput(stderr)
	client := &printer.Client{}
	global.StringVar(&client.Host, "host", "", "printer hostname/IP")
	global.IntVar(&client.SDCPPort, "sdcp-port", 3030, "SDCP port")
	global.IntVar(&client.HTTPPort, "http-port", 80, "HTTP port")
	global.IntVar(&client.CameraPort, "camera-port", 3031, "camera port")
	notify := global.String("notify-command", "", "executable receiving ready-event JSON on stdin")
	global.Usage = func() { fmt.Fprint(stderr, help) }
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = global.Args()
	if len(args) == 0 {
		fmt.Fprint(stdout, help)
		return nil
	}
	if err := client.Validate(); err != nil {
		return err
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var file, path, axis, data string
	var wait, verify bool
	var number, retries int
	var threshold, step float64
	var timeout, duration time.Duration
	switch command {
	case "status", "attributes":
	case "query":
		flags.IntVar(&number, "command", -1, "read-only command")
		flags.StringVar(&data, "data", "{}", "JSON object")
	case "upload":
		flags.BoolVar(&wait, "wait-idle", false, "wait until idle; retry rejected uploads")
		flags.BoolVar(&verify, "verify-only", false, "only verify printer copy")
		flags.DurationVar(&timeout, "timeout", 2*time.Hour, "maximum total wait/upload time")
	case "snapshot":
		flags.StringVar(&path, "output", "", "new JPEG file")
	case "watch", "record":
		flags.StringVar(&file, "file", "", "selected G-code filename")
		flags.StringVar(&path, "output", "", "event JSON file or new recording directory")
		flags.DurationVar(&timeout, "timeout", 24*time.Hour, "overall timeout including waiting for job")
		if command == "watch" {
			flags.Float64Var(&threshold, "temperature", 30, "cooled-bed threshold")
		} else {
			flags.DurationVar(&duration, "max-duration", 6*time.Hour, "recording limit after job attachment")
			flags.IntVar(&retries, "camera-retries", 5, "stream reconnection attempts")
		}
	case "jog":
		flags.StringVar(&axis, "axis", "", "axis")
		flags.Float64Var(&step, "step", 0, "relative distance in mm")
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if command != "upload" && flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if timeout < 0 || duration < 0 {
		return errors.New("durations must be positive")
	}
	if (command == "upload" || command == "watch" || command == "record") && timeout == 0 {
		return errors.New("--timeout must be positive")
	}
	if command == "watch" && (threshold < 20 || threshold > 35) {
		return errors.New("temperature must be between 20 and 35 C")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	switch command {
	case "status":
		value, err := client.RawStatus(ctx)
		if err != nil {
			return err
		}
		return output(stdout, value)
	case "attributes":
		value, err := client.Attributes(ctx)
		if err != nil {
			return err
		}
		return output(stdout, value)
	case "query":
		var request map[string]any
		if err := json.Unmarshal([]byte(data), &request); err != nil {
			return err
		}
		if request == nil {
			return errors.New("--data must be a JSON object")
		}
		value, err := client.Query(ctx, number, request)
		if err != nil {
			return err
		}
		return output(stdout, value)
	case "snapshot":
		if path == "" {
			return errors.New("--output is required")
		}
		return client.Snapshot(ctx, path)
	case "jog":
		return client.Jog(ctx, strings.ToUpper(axis), step)
	case "upload":
		if flags.NArg() != 1 {
			return errors.New("upload requires one local file")
		}
		path = flags.Arg(0)
		if _, err := os.Stat(path); err != nil {
			return err
		}
		for {
			if wait && !verify {
				s, err := client.Status(ctx)
				if err != nil || !s.Idle() {
					if err := printer.Pause(ctx, 5*time.Second); err != nil {
						return err
					}
					continue
				}
			}
			err := client.Upload(ctx, path, verify, stderr)
			if err == nil {
				return nil
			}
			if !wait || verify {
				return err
			}
			fmt.Fprintf(stderr, "Upload attempt failed: %v; retrying\n", err)
			if err := printer.Pause(ctx, 10*time.Second); err != nil {
				return err
			}
		}
	case "watch", "record":
		if command == "record" && path == "" {
			return errors.New("--output is required")
		}
		if command == "watch" && path != "" {
			if _, err := os.Lstat(path); err == nil {
				return errors.New("output already exists")
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		first, err := client.WaitJob(ctx, file)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Attached to %s (task %s)\n", printer.Filename(first.Print.Filename), first.Print.TaskID)
		if command == "record" {
			if duration == 0 {
				return errors.New("--max-duration must be positive")
			}
			ctx, cancel := context.WithTimeout(ctx, duration)
			defer cancel()
			return client.Record(ctx, first, path, retries, stderr)
		}
		event, err := client.WaitReady(ctx, first, threshold, func(s printer.Status, err error) {
			if err != nil {
				fmt.Fprintln(stderr, "Status unavailable; retrying")
			}
		})
		if err != nil {
			return err
		}
		if event.Event == "removal-ready" && *notify != "" {
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			cmd := exec.CommandContext(ctx, *notify)
			cmd.Stdin = strings.NewReader(string(encoded))
			cmd.Stdout = stderr
			cmd.Stderr = stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("notification command failed: %w", err)
			}
		}
		if path != "" {
			if err := printer.WriteJSON(path, event); err != nil {
				return err
			}
		}
		return output(stdout, event)
	}
	return nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "elegooctl:", err)
		os.Exit(1)
	}
}
