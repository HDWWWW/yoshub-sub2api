//go:build unit

package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIPrivacyOutcomeWritesOnlyMatchingCurrentPreference(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, matched := range []bool{true, false} {
			t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled]+"/"+map[bool]string{true: "current", false: "changed"}[matched], func(t *testing.T) {
				repo, mock := proxyHydrationRepository(t)
				mode := service.PrivacyModeTrainingOff
				if !enabled {
					mode = service.PrivacyModeTrainingOn
				}
				mock.ExpectBegin()
				affected := int64(0)
				if matched {
					affected = 1
				}
				mock.ExpectExec(`(?s)UPDATE accounts.*jsonb_build_object\('privacy_mode', \$2::text\).*WHERE id = \$1.*IS DISTINCT FROM 'false'::jsonb\) = \$3`).
					WithArgs(int64(27), mode, enabled).WillReturnResult(sqlmock.NewResult(0, affected))
				if matched {
					mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
						WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
						WillReturnResult(sqlmock.NewResult(1, 1))
					mock.ExpectCommit()
				} else {
					mock.ExpectRollback()
				}
				saved, err := repo.UpdateOpenAIPrivacyMode(context.Background(), 27, enabled, mode)
				require.NoError(t, err)
				require.Equal(t, matched, saved)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}
