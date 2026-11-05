package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/installation"
	mongostore "github.com/arksnorman/open-kase/api/internal/persistence/mongo"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/product"
	"github.com/arksnorman/open-kase/api/package/secrets"
)

func runInit(ctx context.Context, args []string, stdio IO) int {
	boot := bootstrapFromEnv()
	fs := newFlagSet("init", `Initialize the installation. Safe to run again: existing records are kept.

Creates the master key file (mode 0600) when it does not exist, creates the
database collections and indexes, and seeds the workspace, default settings, built-in
statuses, default request types, the deployment's single project (with its
Triage group) and the first administrator. Tickets are numbered 1, 2, 3, ...
The administrator password is read from a hidden prompt, or from the first
line of standard input with --password-stdin. It is never accepted as a flag.`)
	boot.registerFlags(fs, false)
	workspace := fs.String("workspace-name", "", "workspace (organisation) name")
	email := fs.String("admin-email", "", "first administrator email")
	given := fs.String("admin-given-name", "", "first administrator first (given) name")
	middle := fs.String("admin-middle-name", "", "first administrator middle name (optional)")
	family := fs.String("admin-family-name", "", "first administrator last (family) name")
	pwStdin := fs.Bool("password-stdin", false, "read the administrator password from standard input")
	if code, stop := parseFlags(fs, args, stdio); stop {
		return code
	}
	if err := boot.validate(false); err != nil {
		fmt.Fprintf(stdio.Err, "configuration error:\n%v\n", err)
		return ExitUsage
	}
	log := boot.newLogger(stdio.Err)

	if _, err := os.Stat(boot.MasterKeyFile); errors.Is(err, os.ErrNotExist) {
		if code := refuseNewKeyForInstallation(ctx, boot, stdio); code != ExitOK {
			return code
		}
	}
	keys, created, err := secrets.EnsureKeyFile(boot.MasterKeyFile)
	if err != nil {
		fmt.Fprintf(stdio.Err, "master key: %v\n", err)
		return ExitUsage
	}
	if created {
		fmt.Fprintf(stdio.Out, "Created master key %s (key id %s).\n", boot.MasterKeyFile, keys.ActiveKeyID())
		fmt.Fprintln(stdio.Out, "Back this file up separately from database backups; encrypted settings cannot be recovered without it.")
	} else {
		fmt.Fprintf(stdio.Out, "Using master key %s (key id %s).\n", boot.MasterKeyFile, keys.ActiveKeyID())
	}

	c, err := newContainer(log, boot, keys)
	if err != nil {
		fmt.Fprintf(stdio.Err, "%v\n", err)
		return ExitUsage
	}
	defer c.Close(context.Background())

	opCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	rs, err := c.Store.ReplicaSet(opCtx)
	if err != nil {
		fmt.Fprintf(stdio.Err, "mongodb: %v\n", err)
		return ExitFailure
	}
	if rs.Members == 1 {
		fmt.Fprintf(stdio.Out, "MongoDB replica set %q has one member: transactions work, but it is not highly available.\n", rs.Name)
	}
	if err := c.Store.EnsureSchema(opCtx); err != nil {
		fmt.Fprintf(stdio.Err, "database schema: %v\n", err)
		return ExitFailure
	}

	state, err := c.Installation.State(opCtx)
	if errors.Is(err, installation.ErrKeyMismatch) {
		fmt.Fprintf(stdio.Err, "The master key %s does not match this installation. Use the original key file; do not generate a new one.\n", boot.MasterKeyFile)
		return ExitFailure
	}
	if err != nil {
		fmt.Fprintf(stdio.Err, "installation state: %v\n", err)
		return ExitFailure
	}

	// Administrator MFA is required by default outside development; the
	// first administrator enrols at first sign-in.
	in := installation.Input{WorkspaceName: *workspace, AdminEmail: *email, Actor: operatorActor(),
		RequireAdminMFA: boot.Env != EnvDevelopment && boot.Env != EnvTest}
	in.AdminName = users.PersonName{Given: *given, Middle: *middle, Family: *family}
	interactive := term.IsTerminal(int(stdio.In.Fd()))
	reader := bufio.NewReader(stdio.In)
	if !state.Initialized && in.WorkspaceName == "" {
		if in.WorkspaceName, err = prompt(reader, stdio, interactive, "Workspace name"); err != nil {
			return promptFailed(stdio, err, "--workspace-name")
		}
	}
	if !state.AdminExists {
		if in.AdminEmail == "" {
			if in.AdminEmail, err = prompt(reader, stdio, interactive, "Administrator email"); err != nil {
				return promptFailed(stdio, err, "--admin-email")
			}
		}
		if in.AdminName.Given == "" {
			if in.AdminName.Given, err = prompt(reader, stdio, interactive, "Administrator first name"); err != nil {
				return promptFailed(stdio, err, "--admin-given-name")
			}
		}
		if in.AdminName.Family == "" {
			if in.AdminName.Family, err = prompt(reader, stdio, interactive, "Administrator last name"); err != nil {
				return promptFailed(stdio, err, "--admin-family-name")
			}
		}
		if in.AdminPassword, err = readPassword(reader, stdio, interactive, *pwStdin); err != nil {
			fmt.Fprintf(stdio.Err, "password: %v\n", err)
			return ExitUsage
		}
	} else if *email != "" || *pwStdin {
		fmt.Fprintln(stdio.Out, "An administrator already exists; administrator flags were ignored.")
	}

	res, err := c.Installation.Initialize(opCtx, in)
	if err != nil {
		var fe *fault.Error
		if errors.As(err, &fe) && fe.Kind == fault.KindValidation {
			fmt.Fprintln(stdio.Err, "Invalid input:")
			for _, f := range fe.Fields {
				fmt.Fprintf(stdio.Err, "  %s: %s\n", f.Field, f.Message)
			}
			return ExitUsage
		}
		fmt.Fprintf(stdio.Err, "initialize: %v\n", err)
		return ExitFailure
	}
	fmt.Fprintf(stdio.Out, "Workspace %s: %s\n", res.WorkspaceId, createdOrKept(res.WorkspaceCreated))
	fmt.Fprintf(stdio.Out, "Settings: %s\n", createdOrKept(res.SettingsCreated))
	fmt.Fprintf(stdio.Out, "Project: %s\n", createdOrKept(res.ProjectCreated))
	fmt.Fprintf(stdio.Out, "Statuses created (built-ins and presets): %d; default request types created: %d\n", res.StatusesCreated, res.RequestTypesCreated)
	switch {
	case res.AdminCreated:
		fmt.Fprintf(stdio.Out, "Administrator %s created.\n", in.AdminEmail)
	case state.AdminExists:
		fmt.Fprintln(stdio.Out, "Administrator: already exists (kept).")
	default:
		fmt.Fprintln(stdio.Out, "Administrator: another initialization created it concurrently (kept).")
	}
	fmt.Fprintf(stdio.Out, "Initialization complete. Start the server with `%s serve`.\n", product.Binary)
	return ExitOK
}

// refuseNewKeyForInstallation stops init from generating a master key for a
// database that already holds an installation: its secrets are sealed with
// the original key, which must be put back instead (a new key file there
// would only be a useless, misleading copy).
func refuseNewKeyForInstallation(ctx context.Context, boot Bootstrap, stdio IO) int {
	store, err := mongostore.Connect(boot.MongoURI)
	if err != nil {
		fmt.Fprintf(stdio.Err, "%v\n", err)
		return ExitUsage
	}
	defer store.Close(context.Background())
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	st, err := mongostore.NewInstallationRepository(store).State(cctx)
	if err != nil {
		fmt.Fprintf(stdio.Err, "installation state: %v\n", err)
		return ExitFailure
	}
	if st.Initialized {
		fmt.Fprintf(stdio.Err, "The master key file %s does not exist, but this database already holds an initialized installation "+
			"(workspace %q) whose secrets are sealed with its original key. Restore that key file there; init does not generate a new one.\n",
			boot.MasterKeyFile, st.WorkspaceName)
		return ExitFailure
	}
	return ExitOK
}

func createdOrKept(created bool) string {
	if created {
		return "created"
	}
	return "already present (kept)"
}

func operatorActor() audit.Actor {
	label := "cli init"
	if u, err := user.Current(); err == nil {
		label += " (os user " + u.Username + ")"
	}
	return audit.Operator(audit.SystemInstallation, label)
}

var errNotInteractive = errors.New("standard input is not a terminal")

func prompt(r *bufio.Reader, stdio IO, interactive bool, label string) (string, error) {
	if !interactive {
		return "", errNotInteractive
	}
	fmt.Fprintf(stdio.Out, "%s: ", label)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func promptFailed(stdio IO, err error, flagName string) int {
	if errors.Is(err, errNotInteractive) {
		fmt.Fprintf(stdio.Err, "%s is required when not running interactively\n", flagName)
	} else {
		fmt.Fprintf(stdio.Err, "input: %v\n", err)
	}
	return ExitUsage
}

// readPassword reads the administrator password without echo, with
// confirmation, or from the first stdin line when --password-stdin is set.
func readPassword(r *bufio.Reader, stdio IO, interactive, fromStdin bool) (string, error) {
	if fromStdin {
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("no password on standard input: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	if !interactive {
		return "", errors.New("standard input is not a terminal; use --password-stdin")
	}
	fd := int(stdio.In.Fd())
	for range 3 {
		fmt.Fprint(stdio.Out, "Administrator password (hidden, 12+ characters): ")
		pw, err := term.ReadPassword(fd)
		fmt.Fprintln(stdio.Out)
		if err != nil {
			return "", err
		}
		fmt.Fprint(stdio.Out, "Confirm password: ")
		confirm, err := term.ReadPassword(fd)
		fmt.Fprintln(stdio.Out)
		if err != nil {
			return "", err
		}
		if string(pw) == string(confirm) {
			return string(pw), nil
		}
		fmt.Fprintln(stdio.Out, "Passwords do not match; try again.")
	}
	return "", errors.New("passwords did not match")
}
