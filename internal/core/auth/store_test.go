/*
Testing: store.go

Pending:

Tested:
  Load
    - TestLoad_ShouldRoundTripASavedStore: what Save writes, Load reads back.
    - TestLoad_ShouldReportMissingFileAsNotExist: absent file is distinguishable from unreadable.
    - TestLoad_ShouldReturnErrorWhenFileMalformed: garbage is an error, not an empty store.
    - TestLoad_ShouldReturnErrorWhenExpiryMalformed: an unparseable expiry fails the load rather than reading as never-expires.
    - TestLoad_ShouldEnforceTheInvariantsAddApplies: a hand-edited file gets label, duplicate and hash checks.
  Expiry.MarshalText
    - TestMarshalText_ShouldRefuseAYearItCouldNotParseBack: a 5-digit year is refused, not written.
  Save
    - TestSave_ShouldWriteOwnerOnlyPermissions: the file is not world-readable.
    - TestSave_ShouldReplaceExistingFileAtomically: no temp files left behind.
    - TestSave_ShouldNeverPersistTheTokenItself: only the hash reaches disk.
    - TestSave_ShouldRefuseToWriteAnUnreadableStore: an unrenderable expiry fails the Save, leaving no file.
  Add
    - TestAdd_ShouldRejectDuplicateLabel: adding never silently revokes a live token.
    - TestAdd_ShouldRejectInvalidLabel: label charset is enforced.
    - TestAdd_ShouldAppendValidEntry: the happy path.
  Revoke
    - TestRevoke_ShouldRemoveMatchingEntry: removes only the named token.
    - TestRevoke_ShouldReturnErrorWhenLabelUnknown: a typo is not a silent no-op.
  Find
    - TestFind_ShouldReportPresence: hit and miss.
  Entry.Expired
    - TestExpired_ShouldClassifyAgainstClock: table over never/future/past/boundary.

Tested elsewhere:

Declined:

Additional Remarks:
  Expiry tests use a fixed clock passed explicitly rather than time.Now, so the
  boundary case (expiry exactly now) is deterministic.

  Permission assertions are skipped on Windows, where Unix mode bits are not
  meaningful and the directory ACL is the real protection.

  Save's directory fsync is a no-op on Windows, which has no equivalent
  operation. The Save tests cover it only in the sense that they would fail if
  it returned an error there, which is how the omission was found.
*/

package auth

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testClock is a fixed reference time; expiry cases are expressed relative to it.
var testClock = time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)

// newEntry builds a valid entry for the given label.
func newEntry(label string) Entry {
	return Entry{Label: label, Hash: Hash("wadapt_" + label), CreatedAt: testClock}
}

func TestLoad_ShouldRoundTripASavedStore(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := filepath.Join(t.TempDir(), "tokens.toml")
	expires := testClock.Add(24 * time.Hour)

	store := &Store{Tokens: []Entry{
		newEntry("weave-prod"),
		{Label: "weave-staging", Hash: Hash("wadapt_staging"), CreatedAt: testClock, ExpiresAt: NewExpiry(expires)},
	}}

	// ACT
	require.NoError(t, store.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)

	// ASSERT — labels, hashes, and the optional expiry all survive.
	require.Len(t, loaded.Tokens, 2)
	assert.Equal(t, "weave-prod", loaded.Tokens[0].Label)
	assert.Equal(t, store.Tokens[0].Hash, loaded.Tokens[0].Hash)
	assert.Nil(t, loaded.Tokens[0].ExpiresAt)

	require.NotNil(t, loaded.Tokens[1].ExpiresAt)
	assert.True(t, expires.Equal(loaded.Tokens[1].ExpiresAt.Time()),
		"expiry should survive the round-trip, got %s", loaded.Tokens[1].ExpiresAt.Time())
}

func TestLoad_ShouldReportMissingFileAsNotExist(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — a fresh install has no token file yet.
	_, err := Load(filepath.Join(t.TempDir(), "absent.toml"))

	// ASSERT — callers must be able to treat this as "no tokens", not a failure.
	require.Error(t, err)
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestLoad_ShouldReturnErrorWhenFileMalformed(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := filepath.Join(t.TempDir(), "tokens.toml")
	require.NoError(t, os.WriteFile(path, []byte("this is not = = toml"), storeFileMode))

	// ACT
	_, err := Load(path)

	// ASSERT — a corrupt file must not read as an empty allow-list.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing token file")
}

func TestLoad_ShouldReturnErrorWhenExpiryMalformed(t *testing.T) {
	t.Parallel()

	// ARRANGE — a hand-edited expiry that isn't RFC 3339.
	path := filepath.Join(t.TempDir(), "tokens.toml")
	content := "[[tokens]]\nlabel = 'weave-prod'\nhash = 'sha256:abc'\ncreatedAt = 2026-07-18T12:00:00Z\nexpiresAt = 'next tuesday'\n"
	require.NoError(t, os.WriteFile(path, []byte(content), storeFileMode))

	// ACT
	_, err := Load(path)

	// ASSERT — must fail loudly, never read as "never expires".
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expiresAt")
}

func TestLoad_ShouldEnforceTheInvariantsAddApplies(t *testing.T) {
	t.Parallel()

	const (
		validHash = "sha256:" +
			"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
		createdAt = "createdAt = 2026-07-18T12:00:00Z\n"
	)

	tests := []struct {
		name    string
		content string
		wantErr error
	}{
		{
			name:    "should reject a label that could not be added",
			content: "[[tokens]]\nlabel = 'weave prod; rm -rf'\nhash = '" + validHash + "'\n" + createdAt,
			wantErr: ErrInvalidLabel,
		},
		{
			name: "should reject duplicate labels",
			content: "[[tokens]]\nlabel = 'weave-prod'\nhash = '" + validHash + "'\n" + createdAt +
				"[[tokens]]\nlabel = 'weave-prod'\nhash = '" + validHash + "'\n" + createdAt,
			wantErr: ErrDuplicateLabel,
		},
		{
			name:    "should reject a hash no presented token could ever match",
			content: "[[tokens]]\nlabel = 'weave-prod'\nhash = 'sha256:abc'\n" + createdAt,
			wantErr: ErrInvalidHash,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE — the file is hand-editable, so this is a shape an
			// operator can produce without going through Add at all.
			path := filepath.Join(t.TempDir(), "tokens.toml")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), storeFileMode))

			// ACT
			_, err := Load(path)

			// ASSERT
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestMarshalText_ShouldRefuseAYearItCouldNotParseBack(t *testing.T) {
	t.Parallel()

	// ARRANGE — past year 9999, Format widens the year field and the strict
	// Parse in UnmarshalText will not read it back.
	expiry := NewExpiry(time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC))

	// ACT
	_, err := expiry.MarshalText()

	// ASSERT — refusing to write it is what keeps every later Load working;
	// writing it would lock the operator out of their own token file.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside RFC 3339")
}

func TestSave_ShouldRefuseToWriteAnUnreadableStore(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := filepath.Join(t.TempDir(), "tokens.toml")
	entry := newEntry("weave-prod")
	entry.ExpiresAt = NewExpiry(time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC))

	store := &Store{Tokens: []Entry{entry}}

	// ACT
	err := store.Save(path)

	// ASSERT — the failure lands on the command that asked for it, not on every
	// later Load. Nothing is left on disk to reject.
	require.Error(t, err)
	assert.NoFileExists(t, path)
}

func TestSave_ShouldWriteOwnerOnlyPermissions(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	// ARRANGE
	path := filepath.Join(t.TempDir(), "tokens.toml")
	store := &Store{Tokens: []Entry{newEntry("weave-prod")}}

	// ACT
	require.NoError(t, store.Save(path))

	// ASSERT
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(storeFileMode), info.Mode().Perm())
}

func TestSave_ShouldReplaceExistingFileAtomically(t *testing.T) {
	t.Parallel()

	// ARRANGE
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.toml")

	first := &Store{Tokens: []Entry{newEntry("first")}}
	require.NoError(t, first.Save(path))

	// ACT — a second save replaces the file in place.
	second := &Store{Tokens: []Entry{newEntry("second")}}
	require.NoError(t, second.Save(path))

	// ASSERT — content replaced, and no temp file survives the write.
	loaded, err := Load(path)
	require.NoError(t, err)
	require.Len(t, loaded.Tokens, 1)
	assert.Equal(t, "second", loaded.Tokens[0].Label)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the temp file should not outlive the save")
}

func TestSave_ShouldNeverPersistTheTokenItself(t *testing.T) {
	t.Parallel()

	// ARRANGE — the whole point of the store: a leak yields no credentials.
	token, err := Generate()
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "tokens.toml")
	store := &Store{Tokens: []Entry{{Label: "weave-prod", Hash: Hash(token), CreatedAt: testClock}}}

	// ACT
	require.NoError(t, store.Save(path))

	// ASSERT
	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.NotContains(t, string(raw), token)
	assert.Contains(t, string(raw), Hash(token))
}

func TestAdd_ShouldAppendValidEntry(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := &Store{}

	// ACT
	require.NoError(t, store.Add(newEntry("weave-prod")))

	// ASSERT
	require.Len(t, store.Tokens, 1)
	assert.Equal(t, "weave-prod", store.Tokens[0].Label)
}

func TestAdd_ShouldRejectDuplicateLabel(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := &Store{Tokens: []Entry{newEntry("weave-prod")}}

	// ACT
	err := store.Add(newEntry("weave-prod"))

	// ASSERT — an overwrite would revoke a live credential with no trace.
	require.ErrorIs(t, err, ErrDuplicateLabel)
	assert.Len(t, store.Tokens, 1)
}

func TestAdd_ShouldRejectInvalidLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		label string
	}{
		{name: "should reject when empty", label: ""},
		{name: "should reject when it contains spaces", label: "weave prod"},
		{name: "should reject when it starts with a dash", label: "-weave"},
		{name: "should reject when it contains a quote", label: `weave"prod`},
		{name: "should reject when it is too long", label: string(make([]byte, 65))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			store := &Store{}

			// ACT
			err := store.Add(Entry{Label: tt.label, Hash: Hash("x"), CreatedAt: testClock})

			// ASSERT
			require.ErrorIs(t, err, ErrInvalidLabel)
			assert.Empty(t, store.Tokens)
		})
	}
}

func TestRevoke_ShouldRemoveMatchingEntry(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := &Store{Tokens: []Entry{newEntry("keep"), newEntry("drop"), newEntry("keep-too")}}

	// ACT
	require.NoError(t, store.Revoke("drop"))

	// ASSERT — only the named token goes.
	require.Len(t, store.Tokens, 2)
	_, found := store.Find("drop")
	assert.False(t, found)

	_, found = store.Find("keep")
	assert.True(t, found)
}

func TestRevoke_ShouldReturnErrorWhenLabelUnknown(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := &Store{Tokens: []Entry{newEntry("weave-prod")}}

	// ACT — a typo must not look like a successful revocation.
	err := store.Revoke("weave-prd")

	// ASSERT
	require.ErrorIs(t, err, ErrUnknownLabel)
	assert.Len(t, store.Tokens, 1)
}

func TestFind_ShouldReportPresence(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := &Store{Tokens: []Entry{newEntry("weave-prod")}}

	// ACT
	hit, found := store.Find("weave-prod")
	_, missing := store.Find("absent")

	// ASSERT
	assert.True(t, found)
	assert.Equal(t, "weave-prod", hit.Label)
	assert.False(t, missing)
}

func TestExpired_ShouldClassifyAgainstClock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		expires *Expiry
		want    bool
	}{
		{name: "should not expire when no expiry is set", expires: nil, want: false},
		{name: "should not expire when the expiry is ahead", expires: NewExpiry(testClock.Add(time.Hour)), want: false},
		{name: "should expire when the expiry has passed", expires: NewExpiry(testClock.Add(-time.Hour)), want: true},
		{name: "should expire when the expiry is exactly now", expires: NewExpiry(testClock), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			entry := Entry{Label: "weave-prod", ExpiresAt: tt.expires}

			// ACT
			got := entry.Expired(testClock)

			// ASSERT
			assert.Equal(t, tt.want, got)
		})
	}
}
