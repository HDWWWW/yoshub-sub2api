package service

import "time"

// Notifications use Beijing civil time regardless of the server or sender's timezone.
var beijingLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

func formatNotificationTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(beijingLocation).Format("2006-01-02 15:04:05 (UTC-07:00)")
}
