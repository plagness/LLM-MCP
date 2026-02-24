package discovery

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HandleOfflineDevices сбрасывает lease для running jobs на offline устройствах
// Это позволяет быстро переназначить задачи на другие устройства
func HandleOfflineDevices(ctx context.Context, db *pgxpool.Pool, offlineDeviceIDs []string) error {
	if len(offlineDeviceIDs) == 0 {
		return nil
	}

	slog.Info("handling offline devices", "component", "discovery", "count", len(offlineDeviceIDs))

	// Сбрасываем lease для всех running jobs на offline устройствах
	tag, err := db.Exec(ctx, `
		UPDATE jobs
		SET lease_until = NULL, updated_at = now()
		WHERE status = 'running'
		  AND payload->>'device_id' = ANY($1)
		  AND lease_until > now()
	`, offlineDeviceIDs)

	if err != nil {
		return err
	}

	affected := tag.RowsAffected()
	if affected > 0 {
		slog.Info("reset lease for running jobs on offline devices", "component", "discovery", "affected", affected)
	}

	return nil
}
