package monitor

import "github.com/cocoonlife/influxdb/models"

type Reporter interface {
	Statistics(tags map[string]string) []models.Statistic
}
