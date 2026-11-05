package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"time"

	"github.com/arksnorman/open-kase/api/internal/cli"
	"github.com/arksnorman/open-kase/api/package/product"
	"github.com/arksnorman/open-kase/api/package/secrets"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1 // runtime or dependency failure
	ExitUsage   = 2 // invalid arguments or bootstrap configuration
)

// IO carries the process streams so commands are testable.
type IO struct {
	In  *os.File
	Out io.Writer
	Err io.Writer
}

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, io IO) int
}

func commands() []command {
	return []command{
		{"serve", "Run the HTTP API server", runServe},
		{"init", "Initialize the installation (idempotent, interactive)", runInit},
		{"doctor", "Check configuration and dependencies", runDoctor},
		{"version", "Print version information", runVersion},
		{"health", "Probe a running server's readiness endpoint (for container health checks)", runHealth},
		{"seed-demo", "Create idempotent demo groups, agents, contacts and tickets (refused in production)", runSeedDemo},
		{"worker", "Run only the outbox dispatcher and background job workers (no HTTP)", runWorker},
		{"openapi", "Write the OpenAPI document: 'openapi --out docs/openapi.json'", runOpenAPI},
		{"jobs", "Inspect durable jobs: 'jobs list [--state dead]', 'jobs retry <id>'", runJobs},
		{"keys", "Rotate the JWT signing key: 'keys rotate-jwt' (old key verifies until tokens expire)", runKeys},
	}
}

// userCommands call a running server's REST API with the user's own
// credentials (internal/cli); they need no database configuration.
func userCommands() []command {
	var out []command
	for _, c := range cli.Commands() {
		name := c[0]
		out = append(out, command{name, c[1], func(ctx context.Context, args []string, stdio IO) int {
			return cli.Run(ctx, name, args, cli.IO{In: stdio.In, Out: stdio.Out, Err: stdio.Err})
		}})
	}
	return out
}

// Execute dispatches a CLI command and returns the process exit code.
func Execute(ctx context.Context, args []string, stdio IO) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdio.Out)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	for _, c := range append(commands(), userCommands()...) {
		if c.name == args[0] {
			return c.run(ctx, args[1:], stdio)
		}
	}
	fmt.Fprintf(stdio.Err, "%s: unknown command %q\n\n", product.Binary, args[0])
	usage(stdio.Err)
	return ExitUsage
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s <command> [flags]\n\nCommands:\n", product.Binary)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, "\nUser commands (call a server's REST API; see docs/cli.md):\n")
	for _, c := range userCommands() {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, "\nBootstrap environment: APP_ENV, MONGODB_URI, REDIS_URI, %sMASTER_KEY_FILE, %sHTTP_ADDR.\n", product.EnvPrefix, product.EnvPrefix)
	fmt.Fprintf(w, "Run '%s <command> --help' for command flags.\n", product.Binary)
}

// parseFlags parses fs; it returns (exitCode, true) when the caller should stop.
func parseFlags(fs *flag.FlagSet, args []string, stdio IO) (int, bool) {
	fs.SetOutput(stdio.Err)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, true
		}
		return ExitUsage, true
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stdio.Err, "unexpected arguments: %v\n", fs.Args())
		return ExitUsage, true
	}
	return 0, false
}

func newFlagSet(name, summary string) *flag.FlagSet {
	fs := flag.NewFlagSet(product.Binary+" "+name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s %s [flags]\n\n%s\n\nFlags:\n", product.Binary, name, summary)
		fs.PrintDefaults()
	}
	return fs
}

func runHealth(ctx context.Context, args []string, stdio IO) int {
	fs := newFlagSet("health", "Exit 0 when the server's readiness endpoint returns 200. Uses no database connection.")
	url := fs.String("url", "http://127.0.0.1:8080/health/ready", "readiness URL to probe")
	if code, stop := parseFlags(fs, args, stdio); stop {
		return code
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		fmt.Fprintf(stdio.Err, "health: %v\n", err)
		return ExitUsage
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(stdio.Err, "health: %v\n", err)
		return ExitFailure
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Fprintf(stdio.Out, "%d %s\n", resp.StatusCode, body)
	if resp.StatusCode != http.StatusOK {
		return ExitFailure
	}
	return ExitOK
}

func runVersion(_ context.Context, args []string, stdio IO) int {
	fs := newFlagSet("version", "Print version information.")
	asJSON := fs.Bool("json", false, "print JSON")
	if code, stop := parseFlags(fs, args, stdio); stop {
		return code
	}
	info := map[string]string{
		"product": product.Name, "version": product.Version, "commit": product.Commit,
		"builtAt": product.BuiltAt, "go": goruntime.Version(), "platform": goruntime.GOOS + "/" + goruntime.GOARCH,
	}
	if *asJSON {
		_ = json.NewEncoder(stdio.Out).Encode(info)
		return ExitOK
	}
	fmt.Fprintf(stdio.Out, "%s %s (commit %s, built %s, %s %s)\n", info["product"], info["version"],
		info["commit"], info["builtAt"], info["go"], info["platform"])
	return ExitOK
}

// setup parses bootstrap configuration, loads the master key and wires the
// container. It is shared by serve, worker, doctor and the operator commands.
func setup(boot Bootstrap, needRedis bool, stdio IO) (*Container, int) {
	if err := boot.validate(needRedis); err != nil {
		fmt.Fprintf(stdio.Err, "configuration error:\n%v\n", err)
		return nil, ExitUsage
	}
	log := boot.newLogger(stdio.Err)
	keys, err := secrets.LoadKeyFile(boot.MasterKeyFile)
	if err != nil {
		log.Error("master key unavailable", "error", err.Error(), "hint", "create it with `init` or provide "+product.EnvPrefix+"MASTER_KEY_FILE")
		return nil, ExitUsage
	}
	c, err := newContainer(log, boot, keys)
	if err != nil {
		log.Error("startup failed", "error", err.Error())
		return nil, ExitUsage
	}
	return c, ExitOK
}

func runServe(ctx context.Context, args []string, stdio IO) int {
	boot := bootstrapFromEnv()
	fs := newFlagSet("serve", "Run the HTTP API server with the outbox dispatcher and a bounded pool of background job workers.")
	boot.registerFlags(fs, true)
	workers := fs.Bool("workers", true, "run the outbox dispatcher and job workers in this process")
	concurrency := fs.Int("worker-concurrency", 4, "number of concurrent job workers (1-64)")
	if code, stop := parseFlags(fs, args, stdio); stop {
		return code
	}
	if boot.UIDir != "" {
		if info, err := os.Stat(filepath.Join(boot.UIDir, "index.html")); err != nil || !info.Mode().IsRegular() {
			fmt.Fprintf(stdio.Err, "configuration error:\n--ui-dir %s must contain a built index.html (run the UI build first)\n", boot.UIDir)
			return ExitUsage
		}
	}
	if *concurrency < 1 || *concurrency > 64 {
		fmt.Fprintln(stdio.Err, "configuration error:\n--worker-concurrency must be between 1 and 64")
		return ExitUsage
	}
	c, code := setup(boot, true, stdio)
	if c == nil {
		return code
	}
	defer c.Close(context.Background())
	if code := ensureSchema(ctx, c, stdio); code != ExitOK {
		return code
	}
	if err := serve(ctx, c, serveOptions{Workers: *workers, Concurrency: *concurrency}); err != nil {
		c.Log.Error("server stopped with error", "error", err.Error())
		return ExitFailure
	}
	return ExitOK
}

// ensureSchema checks that MongoDB is a replica set (transactions need one)
// and creates any missing collections, validators and indexes. Cheap when
// the schema is already in place; init, serve and worker run it.
func ensureSchema(ctx context.Context, c *Container, stdio IO) int {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := c.Store.ReplicaSet(ctx); err != nil {
		fmt.Fprintf(stdio.Err, "mongodb: %v\n", err)
		return ExitFailure
	}
	if err := c.Store.EnsureSchema(ctx); err != nil {
		fmt.Fprintf(stdio.Err, "database schema: %v\n", err)
		return ExitFailure
	}
	return ExitOK
}
