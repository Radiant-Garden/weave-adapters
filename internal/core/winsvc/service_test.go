/*
Testing: service.go

Pending:

Tested:

	Definition.Validate -> - TestDefinitionValidate_ShouldRequireNameBinPathAndBudget:
	                         including why a non-positive drain budget is
	                         refused rather than defaulted.
	Definition.withDefaults -> - TestDefinitionWithDefaults_ShouldFillTheRecoverySettings

Tested elsewhere:

	Manager, and everything registered through it: the service subcommand's
	tests drive a fake implementation, and task service-gate drives the real
	one against a live SCM.

	The Windows implementation of Manager: service_windows.go, which has no
	unit coverage by design -- see its own doc block.

Declined:

	Testing the ServiceStatus struct. It is a value the Windows manager fills
	and the subcommand renders; both sides are covered where they have
	behaviour.

Additional Remarks:

	The defaults tested here are not arbitrary. The widening restart schedule
	keeps a genuinely broken config from looping every five seconds forever,
	and the reset period is what stops the last interval becoming permanent --
	without it a service that failed three times months ago is still on the
	60-second schedule.
*/
package winsvc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefinitionValidate_ShouldRequireNameBinPathAndBudget(t *testing.T) {
	t.Parallel()

	valid := Definition{Name: "wadapt-test", BinPath: `C:\x\a.exe`, DrainBudget: time.Second}

	tests := map[string]struct {
		mutate  func(*Definition)
		wantErr string
	}{
		"should accept a complete definition": {
			mutate: func(*Definition) {},
		},
		"should reject a missing name": {
			mutate: func(d *Definition) { d.Name = "" }, wantErr: "Name is required",
		},
		"should reject a missing binary path": {
			mutate: func(d *Definition) { d.BinPath = "" }, wantErr: "BinPath is required",
		},
		"should reject a zero drain budget": {
			mutate: func(d *Definition) { d.DrainBudget = 0 }, wantErr: "DrainBudget must be positive",
		},
		"should reject a negative drain budget": {
			mutate: func(d *Definition) { d.DrainBudget = -time.Second }, wantErr: "DrainBudget must be positive",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			d := valid
			tc.mutate(&d)

			// ACT
			err := d.Validate()

			// ASSERT
			// The budget is refused rather than defaulted because it becomes
			// the service's PreshutdownTimeout: a zero there tells the SCM it
			// may kill a drain immediately, which is the opposite of what an
			// unset field means.
			if tc.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestDefinitionWithDefaults_ShouldFillTheRecoverySettings(t *testing.T) {
	t.Parallel()

	// ARRANGE
	bare := Definition{Name: "wadapt-test", BinPath: `C:\x\a.exe`, DrainBudget: time.Second}

	// ACT
	filled := bare.withDefaults()

	// ASSERT
	assert.Equal(t, DefaultRecovery, filled.Recovery)
	assert.Equal(t, DefaultResetPeriod, filled.ResetPeriod)

	// Widening, not flat: a transient cause clears in the first few seconds,
	// while a real one would otherwise restart-loop every five seconds forever
	// and fill the Event Log faster than anyone can read it.
	require.Len(t, filled.Recovery, 3)

	for i := 1; i < len(filled.Recovery); i++ {
		assert.Greater(t, filled.Recovery[i], filled.Recovery[i-1])
	}

	// Without a reset period the failure counter never clears, so a service
	// that failed three times months ago is still on the last interval.
	assert.Positive(t, filled.ResetPeriod)
}

func TestDefinitionWithDefaults_ShouldKeepExplicitSettings(t *testing.T) {
	t.Parallel()

	// ARRANGE
	custom := Definition{
		Name: "wadapt-test", BinPath: `C:\x\a.exe`, DrainBudget: time.Second,
		Recovery: []time.Duration{time.Second}, ResetPeriod: time.Hour,
	}

	// ACT
	filled := custom.withDefaults()

	// ASSERT
	assert.Equal(t, []time.Duration{time.Second}, filled.Recovery)
	assert.Equal(t, time.Hour, filled.ResetPeriod)
}
