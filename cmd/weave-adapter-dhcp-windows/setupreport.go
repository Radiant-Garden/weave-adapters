package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows"
	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/setup"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// tokenFileMode keeps a written token owner-only. Hygiene rather than the
// boundary: the lockdown applied straight afterwards is what protects it on
// Windows.
const tokenFileMode = 0o600

// printSetupReport renders the plan and what became of it.
//
// The plan is printed whether or not anything is applied, because a privileged
// command that mutates a production DHCP server should say what it is about to
// do before it does it — and --dry-run is then the same output with the doing
// left out.
func printSetupReport(p *printer, result setup.Result) {
	if result.DryRun {
		p.printf("Dry run — nothing was changed.\n\n")
	}

	for _, step := range result.Steps {
		p.printf("  %-9s %-9s %s\n", step.Name, marker(step), step.Verdict.Detail)

		if step.Err != nil {
			// The first line only. main prints the whole error to stderr, and
			// a configuration failure joins one message per bad key — so
			// repeating it in full here turns a seven-line plan into a page
			// and buries the plan it exists to show.
			p.printf("            FAILED: %s\n", firstLine(step.Err.Error()))
		}
	}

	p.printf("\n")
}

// marker renders one step's outcome in the column an operator scans.
//
// Applied is distinguished from pending because in a real run they mean
// opposite things: "pending" after the applying pass means the run stopped
// before reaching it.
func marker(step setup.StepResult) string {
	switch {
	case step.Err != nil:
		return "failed"
	case step.Applied:
		return "done"
	default:
		return step.Verdict.Condition.String()
	}
}

// printBlocked explains what is in the way, which is the whole value of exit 3.
func printBlocked(p *printer, result setup.Result) {
	p.printf("Nothing was changed: %d step(s) need an explicit instruction.\n\n", len(result.Blocked))

	for _, step := range result.Blocked {
		p.printf("  %s: %s\n", step.Name, step.Verdict.Detail)
	}

	p.printf("\nRe-run with the flag each line names, or with --%s to see the whole plan again.\n", flagDryRun)
}

// reportToken shows a freshly minted token, once, and optionally writes it.
//
// The asymmetry with the namespace key is deliberate. The key's consumer is
// this host, so its home is the ACL'd config file and printing it would only
// put a second copy somewhere worse. The TOKEN's consumer is weave on another
// host, so it has to leave this machine over some channel regardless — and a
// file inside a SYSTEM-and-Administrators directory lengthens that path rather
// than shortening it, while leaving a durable plaintext credential at rest
// beside a store whose entire design is "this file is not a credential".
// Nobody deletes that file afterwards.
//
// Transient scrollback is the weaker leak, and a leaked token is revoked and
// reminted while a leaked key is not.
func reportToken(p *printer, result setup.Result, opts setup.Options, tokenOut string, secure setup.SecureFunc) error {
	token := mintedToken(result)
	if token == "" {
		return nil
	}

	p.printf("Token %q was minted.\n\n", opts.TokenLabel)
	p.printf("  %s\n\n", token)
	p.printf("This is the only time it is shown — the store keeps a hash.\n")
	p.printf("Give it to weave as the full Authorization header value, including the scheme:\n")
	p.printf("  Bearer %s\n\n", token)

	if tokenOut == "" {
		return nil
	}

	if err := writeToken(tokenOut, token, secure); err != nil {
		// The token is already in the store and has already been printed, so
		// this is not fatal to the provisioning — but it must not be silent
		// either, or an automated caller reads an empty file as an empty
		// token.
		return fmt.Errorf("the token was minted and shown above, but writing it to %q failed: %w", tokenOut, err)
	}

	p.printf("Also written to %s. That file is a LIVE CREDENTIAL: move it to wherever weave\n", tokenOut)
	p.printf("takes its secrets from, then delete it.\n\n")

	return nil
}

// mintedToken pulls the token out of a completed run, if one was minted.
func mintedToken(result setup.Result) string { return result.Token }

// printGeneratedKey reports a namespace key this run invented — as a
// FINGERPRINT, never as the key.
//
// The fingerprint is the same one DHCP-001 logs at startup, derived by the
// adapter rather than re-derived here, so an operator can match the two. That
// is the whole affordance: it lets somebody confirm which key a host is using
// without the key ever being displayed, transcribed by Group Policy, or
// written into an MSI log in a world-readable %TEMP%.
func printGeneratedKey(p *printer, provisioned []config.Provisioned, configPath string) {
	for _, v := range provisioned {
		if v.Key != dhcpwindows.KeyNamespaceKey || !v.FreshlyGenerated {
			continue
		}

		key, ok := v.Value.(string)
		if !ok {
			return
		}

		p.printf("A new identity.namespaceKey was generated for this host.\n")
		p.printf("  fingerprint: %s\n\n", dhcpwindows.NamespaceKeyFingerprint(key))
		p.printf("It is in %s and NOWHERE ELSE. It is never printed, because it cannot be\n", configPath)
		p.printf("rotated: changing it re-derives every ID weave has seen for this server, and\n")
		p.printf("the recreates that follow are refused by Windows' one-scope-per-subnet rule.\n")
		p.printf("BACK THAT FILE UP.\n\n")

		return
	}
}

// writeToken writes a token to a new file and locks it down.
//
// O_EXCL, so it can never overwrite something — an operator who points this at
// an existing file has almost certainly made a mistake, and the one thing
// worse than refusing is destroying whatever was there.
//
// The order is lock-then-write, and it used to be the other way round. A file
// created under a destination directory's inherited grants is readable by
// whoever that directory admits, so writing the credential first left it in
// plaintext under those grants for as long as the lockdown took — and if the
// lockdown then failed, the function returned an error and left the plaintext
// file behind. Creating it EMPTY, securing it, and only then writing means the
// only thing ever exposed is a zero-byte file.
//
// The handle stays open across the lockdown rather than being closed and
// reopened: the write must land in the file this call created, and a reopen by
// name is a window in which that is no longer true.
func writeToken(path, token string, secure setup.SecureFunc) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, tokenFileMode) //nolint:gosec // the operator named this path on the command line, which is the point of the flag
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%q already exists and will not be overwritten", path)
		}

		return err
	}

	// From here every failure removes the file. It is this call's to remove —
	// O_EXCL proves nothing else created it — and leaving an empty or
	// half-written credential file behind is how a later run is told the
	// destination is already occupied.
	if err := writeSecuredToken(f, path, token, secure); err != nil {
		_ = f.Close()
		_ = os.Remove(path) //nolint:gosec // G703: the operator named this path on the command line, and O_EXCL above proves this call created what it is removing

		return err
	}

	return nil
}

// writeSecuredToken locks the created file down and then writes the credential
// into it.
func writeSecuredToken(f *os.File, path, token string, secure setup.SecureFunc) error {
	// Through the injected seam rather than winsvc.Secure directly, like every
	// other lockdown in this binary. Calling the platform function here would
	// make this one path untestable off Windows — and it is the path that
	// leaves a plaintext credential on disk, which is the last one to leave
	// unexercised.
	if _, err := secure([]winsvc.Securable{{
		Path: path,
		Kind: winsvc.SecurableFile,
		Why:  "a written bearer token, which is a live credential",
	}}); err != nil {
		return fmt.Errorf("locking down %q: %w", path, err)
	}

	if _, err := f.WriteString(token + "\n"); err != nil {
		return err
	}

	return f.Close()
}

// printProvisioned reports what a run put into a generated config, naming
// secrets rather than showing them.
//
// identity.namespaceKey never appears. Printing it would contradict the whole
// reason it is kept out of argv: PowerShell transcription is routine Group
// Policy in exactly these environments, scrollback persists, and an MSI log
// lands in a world-readable %TEMP%. The key has to be in the config file
// anyway, so showing it only adds a second copy somewhere less protected.
func printProvisioned(p *printer, provisioned []config.Provisioned) {
	for _, v := range provisioned {
		if v.Secret {
			p.printf("  %s: set (not shown)\n", v.Key)

			continue
		}

		p.printf("  %s: %v\n", v.Key, v.Value)
	}
}

// printHealth says what verification found, which is what exit 4 means.
//
// Said in words as well as in a code, because the code reaches automation and
// the words reach the operator standing at the console — and "installed and
// running, backend unhealthy" is the normal answer on every host without a
// DHCP server, including every CI runner. An operator who reads only a
// non-zero exit would undo a correct installation.
func printHealth(p *printer, result setup.Result) {
	if !result.Health.Checked || result.DryRun {
		return
	}

	if result.Health.ComponentHealthy {
		p.printf("%s answered health and %s is healthy.\n", serviceName, result.Health.Component)
	} else {
		p.printf("%s is installed and running, but %s is not healthy: %s\n",
			serviceName, result.Health.Component, result.Health.Detail)
		p.printf("That is the expected answer on a host with no reachable DHCP backend, and it is\n")
		p.printf("why this exits %d rather than 0. The service itself is fine.\n", exitUnhealthy)
	}

	switch {
	case result.Health.AuthProved:
		p.printf("An authenticated call reached %s, so the token store is being read.\n", protectedPath)

	case result.Health.AuthRejected != "":
		// Its own arm, because the 401 is neither proof nor a skip. Rendered
		// through the skip arm, it told an operator "the authenticated call was
		// not made" and then explained that it was made and refused.
		p.printf("The authenticated call was REFUSED: %s\n", result.Health.AuthRejected)
		p.printf("The service is running but will reject every request weave sends, so this run is\n")
		p.printf("reported as a failure rather than as an install with a note.\n")

	case result.Health.AuthSkipReason != "":
		p.printf("The authenticated call was not made: %s\n", result.Health.AuthSkipReason)
	}
}

// firstLine returns text up to its first newline, marking a truncation so a
// reader knows there is more on stderr.
func firstLine(text string) string {
	if head, _, found := strings.Cut(text, "\n"); found {
		return head + " […]"
	}

	return text
}
