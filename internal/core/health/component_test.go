/*
Testing: component.go

Pending:

Tested:
  overallStatus
    - TestOverallStatus_ShouldReturnWorst: worst-of-wins across components,
      including a status outside the vocabulary.
  httpStatus
    - TestHTTPStatus_ShouldMapUnavailableTo503: 503 for unavailable and for any
      unknown status; 200 only for healthy/unhealthy.
  coreProbe
    - TestCoreProbe_ShouldReportHealthy: the core self-probe is always healthy,
      with the core detail string.

Tested elsewhere:
  rank: all branches exercised through overallStatus, including the default
    branch via the unknown-status case.

Declined:

Additional Remarks:
  httpStatus fails safe: any status outside healthy/unhealthy serves 503. This
  keeps it consistent with rank, which orders an unknown status worst — a probe
  returning a zero-value Result must stop weave routing to the adapter rather
  than reporting 200 with an empty status.
*/

package health

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOverallStatus_ShouldReturnWorst(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		comps []Component
		want  Status
	}{
		{name: "empty is healthy", comps: nil, want: StatusHealthy},
		{
			name:  "all healthy",
			comps: []Component{{Status: StatusHealthy}, {Status: StatusHealthy}},
			want:  StatusHealthy,
		},
		{
			name:  "unhealthy beats healthy",
			comps: []Component{{Status: StatusHealthy}, {Status: StatusUnhealthy}},
			want:  StatusUnhealthy,
		},
		{
			name:  "unavailable beats all",
			comps: []Component{{Status: StatusHealthy}, {Status: StatusUnhealthy}, {Status: StatusUnavailable}},
			want:  StatusUnavailable,
		},
		{
			name:  "unknown status beats unavailable when a probe returns a zero-value Result",
			comps: []Component{{Status: StatusUnavailable}, {Status: ""}},
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, overallStatus(tc.comps))
		})
	}
}

func TestHTTPStatus_ShouldMapUnavailableTo503(t *testing.T) {
	t.Parallel()

	assert.Equal(t, http.StatusOK, httpStatus(StatusHealthy))
	assert.Equal(t, http.StatusOK, httpStatus(StatusUnhealthy))
	assert.Equal(t, http.StatusServiceUnavailable, httpStatus(StatusUnavailable))

	// An unknown status fails safe: weave must stop routing, not see 200.
	assert.Equal(t, http.StatusServiceUnavailable, httpStatus(""))
	assert.Equal(t, http.StatusServiceUnavailable, httpStatus("bogus"))
}

func TestCoreProbe_ShouldReportHealthy(t *testing.T) {
	t.Parallel()

	res := coreProbe{}.Check(t.Context())

	assert.Equal(t, StatusHealthy, res.Status)
	assert.Equal(t, "adapter core running", res.Detail)
	assert.Equal(t, "core", coreProbe{}.Name())
}
