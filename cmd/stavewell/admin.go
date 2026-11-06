package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/glsmith3/stavewell/internal/apikey"
	"github.com/glsmith3/stavewell/internal/store"
)

// usage describes the command line.
const usage = `Usage:
  stavewell
      Run the API server.
  stavewell admin org create --name NAME
  stavewell admin org list
  stavewell admin key create --org ORG_ID --name NAME --scopes SCOPES [--expires-in-days DAYS]
  stavewell admin key list --org ORG_ID
  stavewell admin key revoke --key KEY_ID

Admin commands use the database file that STAVEWELL_DB names (default:
stavewell.db), so run them where the server's database is.

SCOPES is a comma-separated list of ledger:read and ledger:write. DAYS is 1 to
365, default 90.
`

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// errUsage means the command line was wrong. The usage text has already
// been printed.
var errUsage = errors.New("usage")

// errHelp means help was requested and the usage text has been printed.
var errHelp = errors.New("help")

// errKeyInArgs is the error for an argument that looks like it contains an
// API key. Its message deliberately doesn't repeat the argument.
var errKeyInArgs = errors.New("an argument looks like it contains an API key, so the command didn't run and the argument isn't shown. " +
	"Admin commands never take a key itself: to revoke a key, use its key ID (key_…). If the key was exposed, revoke it")

// command is the environment an admin command runs in.
type command struct {
	ctx    context.Context
	stdout io.Writer
	stderr io.Writer
	dbPath string
	now    func() time.Time
}

// runCommand runs a command line other than the bare server command. args
// are the arguments after the program name, at least one. It returns the
// process exit code.
func runCommand(c command, args []string) int {
	switch {
	case slices.ContainsFunc(args, apikey.LooksLikeKey):
		// Checked here as well as in dispatch, so that no message repeats
		// the argument, whichever command it was given to.
		fmt.Fprintf(c.stderr, "stavewell: %v\n", errKeyInArgs)
		return exitError
	case args[0] == "admin":
		return runAdmin(c, args[1:])
	case isHelp(args[0]):
		fmt.Fprint(c.stdout, usage)
		return exitOK
	}
	_ = c.usageError(fmt.Sprintf("unknown command %q", args[0]))
	return exitUsage
}

// runAdmin runs an admin command. args are the arguments after "admin". It
// returns the process exit code.
func runAdmin(c command, args []string) int {
	err := dispatch(c, args)
	switch {
	case err == nil, errors.Is(err, errHelp):
		return exitOK
	case errors.Is(err, errUsage):
		return exitUsage
	default:
		fmt.Fprintf(c.stderr, "stavewell: %v\n", err)
		return exitError
	}
}

func dispatch(c command, args []string) error {
	// No admin command takes an API key, so an argument that looks like one
	// is a mistake, such as a key pasted in place of a key ID. Refuse it
	// before anything can repeat it in a message.
	for _, arg := range args {
		if apikey.LooksLikeKey(arg) {
			return errKeyInArgs
		}
	}
	if len(args) == 1 && isHelp(args[0]) {
		fmt.Fprint(c.stdout, usage)
		return errHelp
	}
	if len(args) < 2 {
		return c.usageError("")
	}
	switch args[0] + " " + args[1] {
	case "org create":
		return c.orgCreate(args[2:])
	case "org list":
		return c.orgList(args[2:])
	case "key create":
		return c.keyCreate(args[2:])
	case "key list":
		return c.keyList(args[2:])
	case "key revoke":
		return c.keyRevoke(args[2:])
	}
	return c.usageError(fmt.Sprintf("unknown command %q", "admin "+args[0]+" "+args[1]))
}

// isHelp reports whether arg asks for help.
func isHelp(arg string) bool {
	switch arg {
	case "help", "-h", "-help", "--help":
		return true
	}
	return false
}

// usageError prints msg, if any, and the usage text, and returns errUsage.
func (c command) usageError(msg string) error {
	if msg != "" {
		fmt.Fprintf(c.stderr, "stavewell: %s\n\n", msg)
	}
	fmt.Fprint(c.stderr, usage)
	return errUsage
}

// parse parses flags for the named command and rejects extra arguments.
func (c command) parse(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(c.stdout, usage)
		return errHelp
	} else if err != nil {
		return c.usageError(fmt.Sprintf("%s: %v", fs.Name(), err))
	}
	if fs.NArg() > 0 {
		return c.usageError(fmt.Sprintf("%s: unexpected argument %q", fs.Name(), fs.Arg(0)))
	}
	return nil
}

// required reports a usage error if any of the named flags is empty.
func (c command) required(fs *flag.FlagSet, names ...string) error {
	for _, name := range names {
		if fs.Lookup(name).Value.String() == "" {
			return c.usageError(fmt.Sprintf("%s: --%s is required", fs.Name(), name))
		}
	}
	return nil
}

// open opens the database.
func (c command) open() (*store.Store, error) {
	return store.Open(c.ctx, c.dbPath)
}

func (c command) orgCreate(args []string) error {
	fs := flag.NewFlagSet("org create", flag.ContinueOnError)
	name := fs.String("name", "", "")
	if err := c.parse(fs, args); err != nil {
		return err
	}
	if err := c.required(fs, "name"); err != nil {
		return err
	}
	db, err := c.open()
	if err != nil {
		return err
	}
	defer db.Close()
	org, err := db.CreateOrganization(c.ctx, *name, c.now())
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, "Organization created.")
	fmt.Fprintln(c.stdout)
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID:\t%s\n", org.ID)
	fmt.Fprintf(w, "Name:\t%s\n", org.Name)
	fmt.Fprintf(w, "Created:\t%s\n", store.FormatTime(org.CreatedAt))
	return w.Flush()
}

func (c command) orgList(args []string) error {
	fs := flag.NewFlagSet("org list", flag.ContinueOnError)
	if err := c.parse(fs, args); err != nil {
		return err
	}
	db, err := c.open()
	if err != nil {
		return err
	}
	defer db.Close()
	orgs, err := db.ListOrganizations(c.ctx)
	if err != nil {
		return err
	}
	if len(orgs) == 0 {
		fmt.Fprintln(c.stdout, "No organizations.")
		return nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tCREATED")
	for _, org := range orgs {
		fmt.Fprintf(w, "%s\t%s\t%s\n", org.ID, org.Name, store.FormatTime(org.CreatedAt))
	}
	return w.Flush()
}

func (c command) keyCreate(args []string) error {
	fs := flag.NewFlagSet("key create", flag.ContinueOnError)
	org := fs.String("org", "", "")
	name := fs.String("name", "", "")
	scopes := fs.String("scopes", "", "")
	days := fs.Int("expires-in-days", store.DefaultKeyDays, "")
	if err := c.parse(fs, args); err != nil {
		return err
	}
	if err := c.required(fs, "org", "name", "scopes"); err != nil {
		return err
	}
	db, err := c.open()
	if err != nil {
		return err
	}
	defer db.Close()
	key, secret, err := db.CreateAPIKey(c.ctx, store.NewAPIKey{
		OrganizationID: *org,
		Name:           *name,
		Scopes:         splitScopes(*scopes),
		Days:           *days,
	}, c.now())
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, "API key created. This is the only time the key is shown, so copy it now.")
	fmt.Fprintln(c.stdout)
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Key:\t%s\n", secret)
	fmt.Fprintf(w, "Key ID:\t%s\n", key.ID)
	fmt.Fprintf(w, "Organization:\t%s\n", key.OrganizationID)
	fmt.Fprintf(w, "Name:\t%s\n", key.Name)
	fmt.Fprintf(w, "Scopes:\t%s\n", strings.Join(key.Scopes, ", "))
	fmt.Fprintf(w, "Expires:\t%s\n", store.FormatTime(key.ExpiresAt))
	return w.Flush()
}

// splitScopes splits a comma-separated list, ignoring spaces around items.
func splitScopes(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func (c command) keyList(args []string) error {
	fs := flag.NewFlagSet("key list", flag.ContinueOnError)
	org := fs.String("org", "", "")
	if err := c.parse(fs, args); err != nil {
		return err
	}
	if err := c.required(fs, "org"); err != nil {
		return err
	}
	db, err := c.open()
	if err != nil {
		return err
	}
	defer db.Close()
	keys, err := db.ListAPIKeys(c.ctx, *org)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Fprintf(c.stdout, "Organization %s has no API keys.\n", *org)
		return nil
	}
	now := c.now()
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY ID\tNAME\tSCOPES\tCREATED\tEXPIRES\tSTATUS")
	for _, key := range keys {
		status := "active"
		switch {
		case key.Revoked():
			status = "revoked " + store.FormatTime(key.RevokedAt)
		case key.Expired(now):
			status = "expired"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", key.ID, key.Name, strings.Join(key.Scopes, ","),
			store.FormatTime(key.CreatedAt), store.FormatTime(key.ExpiresAt), status)
	}
	return w.Flush()
}

func (c command) keyRevoke(args []string) error {
	fs := flag.NewFlagSet("key revoke", flag.ContinueOnError)
	keyID := fs.String("key", "", "")
	if err := c.parse(fs, args); err != nil {
		return err
	}
	if err := c.required(fs, "key"); err != nil {
		return err
	}
	db, err := c.open()
	if err != nil {
		return err
	}
	defer db.Close()
	at, err := db.RevokeAPIKey(c.ctx, *keyID, c.now())
	if errors.Is(err, store.ErrAlreadyRevoked) {
		fmt.Fprintf(c.stdout, "API key %s was already revoked at %s.\n", *keyID, store.FormatTime(at))
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "API key %s revoked at %s.\n", *keyID, store.FormatTime(at))
	return nil
}
