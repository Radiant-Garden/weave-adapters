package setup

import (
	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// SecurePaths locks down everything the resolved configuration names, and
// reports what happened to each target.
//
// The targets come from the resolved values rather than from what a caller was
// told, which is the argument for doing this in the binary at all: a script
// would have to be handed all three paths and would drift from the
// configuration silently. It is shared by install and by the standalone
// lockdown verb so that the two cannot cover different sets.
//
// Reporting rather than printing: whether a skipped target deserves a warning,
// and in what words, belongs to whichever command is speaking.
func SecurePaths(deps Deps, configPath string, values *config.Values) ([]winsvc.SecureResult, error) {
	targets := winsvc.SecurablesFor(
		configPath,
		values.String(config.KeyAuthTokensFile),
		values.String(config.KeyLogFile),
	)

	return deps.Secure(targets)
}
