/*
Testing: component.go

Pending:

Tested:
  overallStatus
    - TestOverallStatus_ShouldReturnWorst: worst-of-wins across components.
  httpStatus
    - TestHTTPStatus_ShouldMapUnavailableTo503: 503 only for unavailable.
  coreProbe
    - TestCoreProbe_ShouldReportHealthy: the core self-probe is always healthy.

Tested elsewhere:
  rank: exercised through overallStatus.

Declined:

Additional Remarks:
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
}

func TestCoreProbe_ShouldReportHealthy(t *testing.T) {
	t.Parallel()

	res := coreProbe{}.Check(t.Context())

	assert.Equal(t, StatusHealthy, res.Status)
	assert.Equal(t, "core", coreProbe{}.Name())
}
