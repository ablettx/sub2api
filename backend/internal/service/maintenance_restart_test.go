//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestMaintenanceReleaseRestartDrainsOrRestoresAdmission(t *testing.T) {
	for _, test := range []struct {
		name     string
		pending  int
		queryErr error
		wantErr  bool
	}{
		{name: "idle can restart"},
		{name: "active settlement cancels restart", pending: 1, wantErr: true},
		{name: "database timeout cancels restart", queryErr: context.DeadlineExceeded, wantErr: true},
		{name: "database failure cancels restart", queryErr: errors.New("isolated storage failure"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			expectation := mock.ExpectQuery(`SELECT COUNT\(\*\) FROM account_user_quota_reservations`)
			if test.queryErr != nil {
				expectation.WillReturnError(test.queryErr)
			} else {
				expectation.WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(test.pending))
			}
			quota := &AccountUserQuotaService{}
			update := ProvideUpdateService(nil, nil, BuildInfo{Version: "0.2.17-ablett.1"}, db, quota, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
			defer cancel()
			err = update.PrepareRestart(ctx)
			if test.wantErr {
				require.Error(t, err)
				require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err), "the real admin error envelope must retain a retryable status")
				require.Equal(t, AccountUserQuotaUnavailable, infraerrors.Reason(err))
				require.False(t, quota.draining, "a canceled maintenance operation must restore ordinary admission")
			} else {
				require.NoError(t, err)
				require.True(t, quota.draining, "new controlled executions stay stopped until the process restarts")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
