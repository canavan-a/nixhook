// Command nixhook deploys NixOS flake configurations over HTTP. The same
// binary is the server that runs on each box and the client that triggers
// and watches deploys.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/canavan-a/nixhook/internal/api"
	"github.com/canavan-a/nixhook/internal/auth"
	"github.com/canavan-a/nixhook/internal/flake"
	"github.com/canavan-a/nixhook/internal/job"
)

// Version is set at build time.
var Version = "dev"

const defaultStateDir = "/var/lib/nixhook"

const usage = `nixhook deploys NixOS flake configurations over HTTP.

Client:
  nixhook deploy  [--box B] [--target T] [--ref R] [--dry-run] [--no-wait] [--quiet] [--timeout 1h]
  nixhook status  [--box B]
  nixhook jobs    [--box B] [--limit N]
  nixhook targets [--box B]
  nixhook logs    [--box B] [-f] [id|latest]

  Boxes are read from ~/.config/nixhook/boxes.json. --url and --key-file
  (or NIXHOOK_URL, NIXHOOK_KEY_FILE, NIXHOOK_KEY) work without a config.

Server:
  nixhook serve --flake <path|uri> [--listen 127.0.0.1:9099] [--state-dir DIR]
                [--allow a,b] [--default-target T] [--rollback]
  nixhook key show|rotate [--state-dir DIR]

  nixhook version
`

// exitCode makes main exit with a specific status without printing anything
// further.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "run-job":
		err = runJob(args)
	case "key":
		err = keyCmd(args)
	case "deploy":
		err = deployCmd(args)
	case "status":
		err = statusCmd(args)
	case "jobs":
		err = jobsCmd(args)
	case "targets":
		err = targetsCmd(args)
	case "logs":
		err = logsCmd(args)
	case "version":
		fmt.Println(Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "nixhook: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		var code exitCode
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		fmt.Fprintln(os.Stderr, "nixhook:", err)
		os.Exit(2)
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := flags.String("listen", "127.0.0.1:9099", "address to listen on")
	stateDir := flags.String("state-dir", defaultStateDir, "directory for the key and job history")
	src := flags.String("flake", "", "local flake checkout (synced with git pull) or flake URI (required)")
	allow := flags.String("allow", "", "comma-separated targets this box may deploy (default: any the flake defines)")
	defaultTarget := flags.String("default-target", "", "target deployed when a request names none")
	rollback := flags.Bool("rollback", false, "roll back to the previous generation if switch or verify fails")
	parseArgs(flags, args)

	if *src == "" {
		return errors.New("serve: --flake is required")
	}
	allowed, err := parseTargets(*allow)
	if err != nil {
		return err
	}
	if *defaultTarget != "" && !flake.ValidTarget(*defaultTarget) {
		return fmt.Errorf("invalid --default-target %q", *defaultTarget)
	}
	if os.Geteuid() != 0 {
		slog.Warn("not running as root; switching the system will fail")
	}

	keyPath := api.KeyPath(*stateDir)
	_, created, err := auth.LoadOrCreate(keyPath)
	if err != nil {
		return fmt.Errorf("api key: %w", err)
	}
	if created {
		slog.Info("generated API key; print it with `nixhook key show`", "path", keyPath)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	srv := api.New(api.Config{
		StateDir:      *stateDir,
		Source:        flake.Source(*src),
		Allow:         allowed,
		DefaultTarget: *defaultTarget,
		Rollback:      *rollback,
		Exe:           exe,
		Env:           []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home},
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	slog.Info("nixhook listening", "addr", *listen, "flake", *src, "version", Version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Deploys run in their own unit, so shutting down only drops open log
	// streams; clients reconnect.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// runJob is started by the server inside the deploy unit.
func runJob(args []string) error {
	flags := flag.NewFlagSet("run-job", flag.ExitOnError)
	stateDir := flags.String("state-dir", defaultStateDir, "state directory")
	src := flags.String("flake", "", "flake source")
	rollback := flags.Bool("rollback", false, "roll back on failure")
	settle := flags.Duration("settle", 10*time.Second, "time to let services settle before verifying")
	pos := parseArgs(flags, args)
	if len(pos) != 1 || *src == "" {
		return errors.New("usage: nixhook run-job --flake <src> [--state-dir DIR] [--rollback] <id>")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := &job.Runner{
		Store:    job.NewStore(*stateDir),
		Source:   flake.Source(*src),
		Rollback: *rollback,
		Settle:   *settle,
	}
	if err := r.Run(ctx, pos[0]); err != nil {
		slog.Error("deploy failed", "job", pos[0], "err", err)
		return exitCode(1)
	}
	return nil
}

func keyCmd(args []string) error {
	flags := flag.NewFlagSet("key", flag.ExitOnError)
	stateDir := flags.String("state-dir", defaultStateDir, "state directory")
	pos := parseArgs(flags, args)
	if len(pos) != 1 {
		return errors.New("usage: nixhook key show|rotate [--state-dir DIR]")
	}
	path := api.KeyPath(*stateDir)
	switch pos[0] {
	case "show":
		key, err := auth.Load(path)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no key at %s; it is created when `nixhook serve` first starts", path)
		}
		if err != nil {
			return err
		}
		fmt.Println(key)
	case "rotate":
		key, err := auth.Rotate(path)
		if err != nil {
			return err
		}
		fmt.Println(key)
		fmt.Fprintln(os.Stderr, "key rotated; the old key no longer works, update your clients' key files")
	default:
		return fmt.Errorf("unknown key command %q", pos[0])
	}
	return nil
}

func parseTargets(list string) ([]string, error) {
	var targets []string
	for t := range strings.SplitSeq(list, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !flake.ValidTarget(t) {
			return nil, fmt.Errorf("invalid target %q", t)
		}
		targets = append(targets, t)
	}
	return targets, nil
}

// parseArgs parses flags that may appear before, after or between positional
// arguments, returning the positional ones.
func parseArgs(flags *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		flags.Parse(args)
		args = flags.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}
