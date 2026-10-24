package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/canavan-a/nixhook/internal/api"
	"github.com/canavan-a/nixhook/internal/client"
	"github.com/canavan-a/nixhook/internal/job"
)

const maxBackoff = 5 * time.Second

// boxFlags registers the flags that pick which server to talk to.
func boxFlags(flags *flag.FlagSet) *client.Options {
	var o client.Options
	flags.StringVar(&o.Box, "box", "", "box name from ~/.config/nixhook/boxes.json")
	flags.StringVar(&o.URL, "url", "", "server URL (overrides the box's)")
	flags.StringVar(&o.KeyFile, "key-file", "", "file holding the API key (overrides the box's)")
	return &o
}

func deployCmd(args []string) error {
	flags := flag.NewFlagSet("deploy", flag.ExitOnError)
	opts := boxFlags(flags)
	target := flags.String("target", "", "flake tag to deploy, nixosConfigurations.<target> (default: the box's)")
	ref := flags.String("ref", "", "branch or tag to deploy (default: main)")
	dryRun := flags.Bool("dry-run", false, "sync and build only, do not switch")
	noWait := flags.Bool("no-wait", false, "start the deploy and print its job ID without watching it")
	quiet := flags.Bool("quiet", false, "show stage progress only, not command output")
	timeout := flags.Duration("timeout", time.Hour, "give up watching after this long (the deploy keeps running)")
	if pos := parseArgs(flags, args); len(pos) > 0 {
		return fmt.Errorf("deploy takes no arguments, got %q (use --target)", pos)
	}

	c, box, err := client.Resolve(*opts)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	req := api.DeployRequest{Target: *target, Ref: *ref, DryRun: *dryRun}
	if req.Target == "" {
		req.Target = box.Target
	}
	p := newPrinter()
	resp, err := c.Deploy(ctx, req)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			if j := apiErr.Body.Job; apiErr.Status == http.StatusConflict && j != nil {
				return fmt.Errorf("%w\n  running: %s (%s, stage %s); watch it with `nixhook logs -f %s`", err, j.ID, j.Target, j.Stage, j.ID)
			}
			if len(apiErr.Body.Targets) > 0 {
				return fmt.Errorf("%w\n  targets: %s", err, strings.Join(apiErr.Body.Targets, ", "))
			}
		}
		return err
	}
	j := resp.Job
	mode := ""
	if j.DryRun {
		mode = ", dry run"
	}
	p.info("→ job %s started on %s (target %s, ref %s%s)", j.ID, c.URL, j.Target, j.Ref, mode)
	if *noWait {
		fmt.Println(j.ID)
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	final, err := watch(ctx, c, j.ID, *quiet, p)
	return finish(p, final, err)
}

func statusCmd(args []string) error {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	opts := boxFlags(flags)
	parseArgs(flags, args)
	c, _, err := client.Resolve(*opts)
	if err != nil {
		return err
	}
	st, err := c.Status(context.Background())
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "host\t%s\n", st.Hostname)
	fmt.Fprintf(w, "nixos\t%s\n", st.NixOSVersion)
	fmt.Fprintf(w, "generation\t%d\n", st.Generation)
	fmt.Fprintf(w, "flake\t%s\n", st.Flake)
	if st.Rev != "" {
		fmt.Fprintf(w, "rev\t%s\n", st.Rev)
	}
	fmt.Fprintf(w, "state\t%s\n", st.State)
	fmt.Fprintf(w, "failed units\t%s\n", orNone(strings.Join(st.FailedUnits, ", ")))
	if j := st.ActiveJob; j != nil {
		fmt.Fprintf(w, "deploying\t%s (%s, stage %s, %s)\n", j.ID, j.Target, j.Stage, j.Duration().Round(time.Second))
	}
	return w.Flush()
}

func jobsCmd(args []string) error {
	flags := flag.NewFlagSet("jobs", flag.ExitOnError)
	opts := boxFlags(flags)
	limit := flags.Int("limit", 20, "number of jobs to list")
	parseArgs(flags, args)
	c, _, err := client.Resolve(*opts)
	if err != nil {
		return err
	}
	jobs, err := c.Jobs(context.Background(), *limit)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tSTAGE\tTARGET\tREF\tGEN\tDURATION")
	for _, j := range jobs {
		gen := "-"
		if j.GenAfter != 0 && j.GenAfter != j.GenBefore {
			gen = fmt.Sprintf("%d→%d", j.GenBefore, j.GenAfter)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, j.Status, j.Stage, j.Target, j.Ref, gen, j.Duration().Round(time.Second))
	}
	return w.Flush()
}

func targetsCmd(args []string) error {
	flags := flag.NewFlagSet("targets", flag.ExitOnError)
	opts := boxFlags(flags)
	parseArgs(flags, args)
	c, _, err := client.Resolve(*opts)
	if err != nil {
		return err
	}
	targets, err := c.Targets(context.Background())
	if err != nil {
		return err
	}
	for _, t := range targets {
		fmt.Println(t)
	}
	return nil
}

func logsCmd(args []string) error {
	flags := flag.NewFlagSet("logs", flag.ExitOnError)
	opts := boxFlags(flags)
	follow := flags.Bool("f", false, "follow the job until it finishes")
	pos := parseArgs(flags, args)
	if len(pos) > 1 {
		return errors.New("usage: nixhook logs [--box B] [-f] [id|latest]")
	}
	id := "latest"
	if len(pos) == 1 {
		id = pos[0]
	}
	c, _, err := client.Resolve(*opts)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	j, err := c.Job(ctx, id)
	if err != nil {
		return err
	}
	p := newPrinter()
	if *follow {
		p.info("→ job %s (target %s, ref %s)", j.ID, j.Target, j.Ref)
		final, err := watch(ctx, c, j.ID, false, p)
		return finish(p, final, err)
	}
	var last *job.Job
	err = c.Stream(ctx, j.ID, 0, false, func(ev client.Event) error {
		switch ev.Type {
		case "log":
			fmt.Println(ev.Data)
		case "state", "done":
			var cur job.Job
			if err := json.Unmarshal([]byte(ev.Data), &cur); err == nil {
				last = &cur
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if last != nil {
		if last.Status.Done() {
			p.summary(last)
		} else {
			p.info("job is still %s (stage %s); follow it with -f", last.Status, last.Stage)
		}
	}
	return nil
}

// watch follows a job's log stream until the job finishes, reconnecting when
// the connection drops, which is expected while a switch restarts the server
// or the tunnel in front of it.
func watch(ctx context.Context, c *client.Client, id string, quiet bool, p *printer) (*job.Job, error) {
	var (
		offset  int64
		last    *job.Job
		done    bool
		lost    bool
		backoff = time.Second
	)
	for {
		err := c.Stream(ctx, id, offset, true, func(ev client.Event) error {
			if lost {
				p.info("  reconnected")
				lost = false
				backoff = time.Second
			}
			switch ev.Type {
			case "log":
				if n, err := strconv.ParseInt(ev.ID, 10, 64); err == nil {
					offset = n
				}
				if !quiet {
					p.log(ev.Data)
				}
			case "state", "done":
				var cur job.Job
				if err := json.Unmarshal([]byte(ev.Data), &cur); err != nil {
					return fmt.Errorf("decode job: %w", err)
				}
				p.stages(last, &cur, quiet)
				last = &cur
				done = ev.Type == "done"
			case "error":
				msg, _ := strconv.Unquote(ev.Data)
				return fmt.Errorf("server: %s", msg)
			}
			return nil
		})
		if done {
			return last, nil
		}
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status < 500 {
			return last, err
		}
		if !lost {
			reason := "stream closed"
			if err != nil {
				reason = err.Error()
			}
			p.warn("  connection lost (%s), reconnecting…", reason)
			lost = true
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// finish prints the outcome of watch and turns it into the exit status:
// 0 when the deploy succeeded, 1 when it failed.
func finish(p *printer, j *job.Job, err error) error {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			id := "latest"
			if j != nil {
				id = j.ID
			}
			p.warn("stopped watching; the deploy keeps running. Reattach with `nixhook logs -f %s`", id)
			return exitCode(2)
		}
		return err
	}
	p.summary(j)
	if j.Status != job.Succeeded {
		return exitCode(1)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func shortRev(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

// printer writes deploy progress, in color when stdout is a terminal.
type printer struct {
	color bool
}

func newPrinter() *printer {
	info, err := os.Stdout.Stat()
	return &printer{color: err == nil && info.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""}
}

const (
	green = "32"
	red   = "31"
	dim   = "2"
	bold  = "1"
	amber = "33"
)

func (p *printer) paint(code, s string) string {
	if !p.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p *printer) info(format string, args ...any) {
	fmt.Println(p.paint(bold, fmt.Sprintf(format, args...)))
}

func (p *printer) warn(format string, args ...any) {
	fmt.Println(p.paint(amber, fmt.Sprintf(format, args...)))
}

func (p *printer) log(line string) {
	fmt.Println(p.paint(dim, line))
}

// stages prints each stage whose status changed between prev and cur.
func (p *printer) stages(prev, cur *job.Job, quiet bool) {
	for i, st := range cur.Stages {
		if prev != nil && i < len(prev.Stages) && prev.Stages[i].Status == st.Status {
			continue
		}
		name := fmt.Sprintf("[%s]", st.Name)
		switch st.Status {
		case job.Running:
			if quiet {
				fmt.Printf("%-9s …\n", name)
			}
		case job.Succeeded:
			fmt.Printf("%-9s %s (%s)\n", name, p.paint(green, "✓"), st.Duration().Round(time.Second))
		case job.Failed:
			fmt.Printf("%-9s %s %s\n", name, p.paint(red, "✗"), st.Error)
		}
	}
}

func (p *printer) summary(j *job.Job) {
	took := j.Duration().Round(time.Second)
	if j.Status == job.Succeeded {
		var parts []string
		if j.GenAfter != 0 && j.GenAfter != j.GenBefore {
			parts = append(parts, fmt.Sprintf("gen %d → %d", j.GenBefore, j.GenAfter))
		}
		switch {
		case j.RevBefore != "" && j.RevAfter != "" && j.RevBefore != j.RevAfter:
			parts = append(parts, fmt.Sprintf("rev %s → %s", shortRev(j.RevBefore), shortRev(j.RevAfter)))
		case j.RevAfter != "":
			parts = append(parts, "rev "+shortRev(j.RevAfter))
		}
		if j.DryRun {
			parts = append(parts, "dry run, built "+j.OutPath)
		}
		fmt.Printf("%s %s in %s\n", p.paint(green+";1", "✔ SUCCESS"), strings.Join(parts, ", "), took)
		return
	}
	where := ""
	if j.Stage != "" {
		where = " at stage " + j.Stage
	}
	fmt.Printf("%s%s: %s\n", p.paint(red+";1", "✘ FAILED"), where, j.Error)
	switch {
	case j.RolledBack:
		fmt.Printf("  rolled back to generation %d\n", j.GenAfter)
	case j.GenBefore == 0:
		fmt.Println("  box unchanged")
	case j.GenAfter == 0 || j.GenAfter == j.GenBefore:
		fmt.Printf("  box unchanged (generation %d)\n", j.GenBefore)
	default:
		fmt.Printf("  box is now on generation %d (not rolled back)\n", j.GenAfter)
	}
	fmt.Printf("  full log: nixhook logs %s\n", j.ID)
}
