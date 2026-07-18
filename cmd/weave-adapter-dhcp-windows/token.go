package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"text/tabwriter"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/auth"
)

// defaultStorePath is the token store the CLI manages. It is deliberately a
// separate file from config.toml: this one is machine-owned and rewritten
// wholesale, and go-toml/v2 does not preserve comments on round-trip, so
// rewriting the hand-edited config would silently eat the operator's comments.
const defaultStorePath = "tokens.toml"

// restartNotice is printed after every mutation. Token changes are picked up
// only at startup — the adapter never watches or reloads this file — so an
// operator who is not told this will conclude the CLI silently failed.
const restartNotice = "Restart the adapter for this change to take effect."

// commandUsage describes the subcommands.
const commandUsage = `Usage: weave-adapter-dhcp-windows token <command> [flags]

Commands:
  gen      mint a new token and add it to the store
  list     show configured tokens (never the tokens themselves)
  revoke   remove a token by label
`

// printer writes human-facing output and remembers the first failure, so a
// closed stdout (a piped `| head`, a full disk) surfaces once from the command
// instead of being discarded at every call site.
type printer struct {
	w   io.Writer
	err error
}

// printf writes a formatted line unless an earlier write already failed.
func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}

	_, p.err = fmt.Fprintf(p.w, format, args...)
}

// isHelpVerb reports whether arg is a request for the command list.
func isHelpVerb(arg string) bool {
	return arg == "help" || arg == "-h" || arg == "--help"
}

// skipHelp maps an explicit help request to success. The flag package prints
// the usage itself and returns ErrHelp; surfacing that as an error would make
// `token gen --help` exit 1, which is wrong — asking for help is not a failure.
func skipHelp(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}

	return err
}

// runToken dispatches a token subcommand. now is injected so expiry handling is
// testable; out receives all human-facing output.
func runToken(args []string, out io.Writer, now func() time.Time) error {
	p := &printer{w: out}

	if len(args) == 0 {
		p.printf("%s", commandUsage)

		return errors.New("token: a command is required")
	}

	if isHelpVerb(args[0]) {
		p.printf("%s", commandUsage)

		return p.err
	}

	switch args[0] {
	case "gen":
		return runTokenGen(args[1:], p, now)
	case "list":
		return runTokenList(args[1:], p, now)
	case "revoke":
		return runTokenRevoke(args[1:], p)
	default:
		p.printf("%s", commandUsage)

		return fmt.Errorf("token: unknown command %q", args[0])
	}
}

// runTokenGen mints a token, stores its hash, and prints the token once.
func runTokenGen(args []string, p *printer, now func() time.Time) error {
	flags := flag.NewFlagSet("token gen", flag.ContinueOnError)
	flags.SetOutput(p.w)

	label := flags.String("label", "", "identifier for this token (becomes the caller subject in logs)")
	path := flags.String("file", defaultStorePath, "path to the token store")
	expiresInDays := flags.Int("expires-in-days", 0, "days until the token stops being accepted (0 = never expires)")

	if err := flags.Parse(args); err != nil {
		return skipHelp(err)
	}

	if *label == "" {
		return errors.New("--label is required")
	}

	if *expiresInDays < 0 {
		return fmt.Errorf("--expires-in-days must not be negative, got %d", *expiresInDays)
	}

	store, err := loadOrEmpty(*path)
	if err != nil {
		return err
	}

	token, err := auth.Generate()
	if err != nil {
		return fmt.Errorf("generating token: %w", err)
	}

	entry := auth.Entry{Label: *label, Hash: auth.Hash(token), CreatedAt: now().UTC()}

	if *expiresInDays > 0 {
		expiry := auth.NewExpiry(entry.CreatedAt.AddDate(0, 0, *expiresInDays))

		// Asking the expiry to render is the bound on the flag: a value large
		// enough to push the year past four digits — or far enough to wrap it
		// negative — cannot be written and read back. Checking here rather than
		// against a made-up ceiling means the limit can never drift from the one
		// the store actually enforces, and the operator hears about the flag
		// they typed instead of a marshalling failure three steps later.
		if _, err := expiry.MarshalText(); err != nil {
			return fmt.Errorf("--expires-in-days %d is too large: %w", *expiresInDays, err)
		}

		entry.ExpiresAt = expiry
	}

	// Add before Save so a duplicate label fails without touching the file.
	if err := store.Add(entry); err != nil {
		return err
	}

	if err := store.Save(*path); err != nil {
		return err
	}

	printGenerated(p, *path, token, entry)

	return p.err
}

// printGenerated reports a freshly minted token. This is the only moment the
// token exists in readable form — the store keeps a hash, so nothing can
// recover it afterwards.
func printGenerated(p *printer, path, token string, entry auth.Entry) {
	p.printf("Token %q added to %s\n\n", entry.Label, path)
	p.printf("  %s\n\n", token)
	p.printf("This is the only time the token is shown — it is stored as a hash.\n")

	// weave sends this value verbatim (its credential store does not prepend a
	// scheme), so show the full header value rather than the bare token.
	p.printf("Give it to weave as the full Authorization header value, including the scheme:\n")
	p.printf("  Bearer %s\n", token)

	if entry.ExpiresAt != nil {
		p.printf("\nExpires %s.\n", entry.ExpiresAt.Time().Format(time.RFC3339))
	}

	p.printf("\n%s\n", restartNotice)
}

// runTokenList prints the configured tokens and their expiry state.
func runTokenList(args []string, p *printer, now func() time.Time) error {
	flags := flag.NewFlagSet("token list", flag.ContinueOnError)
	flags.SetOutput(p.w)

	path := flags.String("file", defaultStorePath, "path to the token store")

	if err := flags.Parse(args); err != nil {
		return skipHelp(err)
	}

	store, err := loadOrEmpty(*path)
	if err != nil {
		return err
	}

	if len(store.Tokens) == 0 {
		p.printf("No tokens configured in %s\n", *path)

		return p.err
	}

	table := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)

	if _, err := fmt.Fprintln(table, "LABEL\tCREATED\tEXPIRES\tSTATUS"); err != nil {
		return fmt.Errorf("writing token list: %w", err)
	}

	for _, entry := range store.Tokens {
		expires, status := describeExpiry(entry, now())

		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\n",
			entry.Label, entry.CreatedAt.Format(time.DateOnly), expires, status); err != nil {
			return fmt.Errorf("writing token list: %w", err)
		}
	}

	return table.Flush()
}

// describeExpiry renders an entry's expiry column and status. Expiry is opt-in
// and enforced at auth time, so the status has to make an approaching deadline
// visible here — nothing else warns before requests start failing.
func describeExpiry(entry auth.Entry, now time.Time) (expires, status string) {
	if entry.ExpiresAt == nil {
		return "never", "active"
	}

	at := entry.ExpiresAt.Time()
	expires = at.Format(time.DateOnly)

	if entry.Expired(now) {
		return expires, "EXPIRED " + formatDays(now.Sub(at)) + " ago"
	}

	return expires, "expires in " + formatDays(at.Sub(now))
}

// formatDays renders a duration in whole days, rounding up so a token with any
// time left never reads as "0 days".
func formatDays(d time.Duration) string {
	days := int(math.Ceil(d.Hours() / 24))
	if days == 1 {
		return "1 day"
	}

	return fmt.Sprintf("%d days", days)
}

// runTokenRevoke removes a token by label.
func runTokenRevoke(args []string, p *printer) error {
	flags := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	flags.SetOutput(p.w)

	label := flags.String("label", "", "label of the token to remove")
	path := flags.String("file", defaultStorePath, "path to the token store")

	if err := flags.Parse(args); err != nil {
		return skipHelp(err)
	}

	if *label == "" {
		return errors.New("--label is required")
	}

	store, err := loadOrEmpty(*path)
	if err != nil {
		return err
	}

	if err := store.Revoke(*label); err != nil {
		return err
	}

	if err := store.Save(*path); err != nil {
		return err
	}

	p.printf("Token %q removed from %s\n\n%s\n", *label, *path, restartNotice)

	return p.err
}

// loadOrEmpty reads the token store, treating a missing file as an empty one —
// a fresh install has no tokens yet, which is not an error. Any other failure
// (unreadable, malformed) propagates, so a corrupt file is never mistaken for
// an empty allow-list and silently overwritten.
func loadOrEmpty(path string) (*auth.Store, error) {
	store, err := auth.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &auth.Store{}, nil
	}

	if err != nil {
		return nil, err
	}

	return store, nil
}
